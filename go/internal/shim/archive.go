package shim

import (
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/activitylog"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/thunk"
	"github.com/tbereknyei/nixgg/internal/toolchain"
	"github.com/tbereknyei/nixgg/internal/wrapperenv"
)

// Archive is the shim entrypoint for `ar <mods> archive.a obj1 obj2 …`.
// It parses the ar modifier string, resolves inputs the same way link
// does, and writes an archive thunk.
//
// We don't model `ar r archive.a …` operations that mutate an existing
// archive — every archive we build is fresh. Modifier flags like
// `s` (index), `u` (update), `q` (quick-append) still make it into the
// thunk expression's ARFlags so `ar` inside the sandbox is called
// with the caller's exact intent.
func Archive(args []string, cfg *toolchain.Config, l paths.Layout) error {
	if bypassed() {
		// See compile.go's identical carveout: no logf here.
		return Passthrough(realARFor(cfg), args)
	}
	modifiers, archive, inputs, ok := parseARArgs(args)
	if !ok {
		// Read-mode invocations (ar t/p/x) land here, as does anything
		// whose modifier string we don't model. Both are fine to pass
		// through — but silence here means a build that accelerated
		// nothing looks exactly like one that accelerated everything.
		logf("ar passthrough: not an archive-creating invocation (%s)", joinBase(args))
		activitylog.Emit("ar", "passthrough", activitylog.Fields{"reason": "not_archive_creating", "argv": args})
		return Passthrough(realARFor(cfg), args)
	}

	if carvedOut(archive) {
		logf("ar passthrough: %s is in a carved-out subtree", archive)
		return Passthrough(realARFor(cfg), args)
	}

	logf("archive %s <- %s", archive, joinBase(inputs))

	if handled, err := tryBatchArchive(cfg, l, archive, modifiers, inputs); handled {
		return err
	}

	altPrefix := altStorePrefix(cfg.Store)
	arInputs, jsonInputs, err, ok := classifyInputs(cfg, inputs, altPrefix, l, "ar", func() error {
		return Passthrough(realARFor(cfg), args)
	})
	if !ok {
		return err
	}

	wrapperEnvJSON, err := wrapperenv.JSON()
	if err != nil {
		return err
	}
	// Archives have no flag list of their own; the CA hash comes from
	// inputs + modifiers. Wrapper env still matters if any input was
	// compiled with -fPIC / whatever, so we plumb it.
	storeDeps := storedeps.From(nil, wrapperEnvJSON, cfg.KnownStorePaths)

	if sandbox.Enabled() {
		return archiveSandbox(cfg, archive, modifiers, jsonInputs, storeDeps, wrapperEnvJSON)
	}

	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}
	e := expr.Archive(expr.ArchiveParams{
		Helpers:    cfg.Helpers,
		Name:       multiTargetName(archive),
		OutName:    filepath.Base(archive),
		Inputs:     arInputs,
		ARFlags:    modifiers,
		StoreDeps:  storeDeps,
		WrapperEnv: wrapperEnv,
	})

	id := thunk.Compute(e)
	thunkPath, err := thunk.Write(l, id, e)
	if err != nil {
		return err
	}
	if err := thunk.LinkPlaceholder(l, archive, thunkPath); err != nil {
		return err
	}
	if err := thunk.RecordSymlink(l, id, archive); err != nil {
		return err
	}
	logf("  thunk:      %s", thunkPath)
	activitylog.Emit("ar", "thunk", activitylog.Fields{"archive": archive, "thunk": thunkPath, "inputs": arInputs})
	return nil
}

// parseARArgs pulls the modifier string, archive path, and input list.
// The classic `ar` CLI has three forms we care about:
//
//	ar rcs   archive.a  obj1 obj2   (rcs = create+replace+index)
//	ar -rcs  archive.a  obj1 obj2   (leading dash tolerated by GNU ar)
//	ar Drcs  archive.a  obj1 obj2   (D = deterministic)
//
// Everything else — tar-like operations, positional -N, `ranlib`-style
// invocations — we pass through unmodeled.
func parseARArgs(args []string) (modifiers, archive string, inputs []string, ok bool) {
	if len(args) < 2 {
		return
	}
	modifiers = args[0]
	// Tolerate GNU's leading dash.
	modifiers = strings.TrimPrefix(modifiers, "-")
	// A modifier string is 1+ chars from a fixed alphabet. Anything
	// else, we bail — it's a positional-count invocation like
	// `ar rN 3 archive.a obj` that we don't model.
	if !isARModifiers(modifiers) {
		return
	}
	// `a`, `b`, `i` and `N` each take a positional argument that follows
	// the modifier string (`ar rN <count> <archive> <member>...`), which
	// shifts the archive name one slot right. Their semantics —
	// insert-relative-to-member, use-instance-N — are member mutations
	// this shim deliberately does not model.
	//
	// Bail explicitly: these were previously rejected only by accident,
	// via the members-must-end-in-.o check that allowing `.a` members
	// removes.
	if strings.ContainsAny(modifiers, "abiN") {
		return "", "", nil, false
	}
	archive = args[1]
	for _, in := range args[2:] {
		// Build systems nest archives, listing a subdirectory's archive
		// as a member alongside objects. Rejecting those sends the
		// invocation to passthrough, where the real ar meets a stub.
		if !strings.HasSuffix(in, ".o") && !strings.HasSuffix(in, ".a") {
			// Anything else — skip modeling.
			return "", "", nil, false
		}
		inputs = append(inputs, in)
	}
	// A creating invocation with NO members is legitimate — an aggregate
	// archive for a directory that contributed none — and must be
	// modelled. Passing it through makes it a plain file, which makes
	// its parent unmodellable in turn, and the damage climbs the tree
	// until a link meets a half-modelled archive.
	//
	// Read-only operations (`ar t archive.a`) also have no members, and
	// those must still bail — hence keying on the creating modifiers
	// rather than on the member count alone.
	if archive == "" {
		return "", "", nil, false
	}
	if len(inputs) == 0 && !strings.ContainsAny(modifiers, "crq") {
		return "", "", nil, false
	}
	return modifiers, archive, inputs, true
}

func isARModifiers(s string) bool {
	if s == "" {
		return false
	}
	// Union of the modifier characters ar accepts. Anything outside
	// means we're looking at a positional arg, not modifiers.
	//
	// `T` (thin archive) is included: one unrecognised character sends
	// the whole invocation to passthrough.
	allowed := "cruvsDxtpqRUbNaimoPST"
	for _, r := range s {
		if !strings.ContainsRune(allowed, r) {
			return false
		}
	}
	return true
}

// realARFor returns the sibling `ar` binary next to the pinned gcc.
// GCC wrappers ship an `ar` in the same bin/ dir; that's what we
// want the passthrough to hit, not whatever's earliest on PATH.
func realARFor(cfg *toolchain.Config) string {
	return filepath.Join(filepath.Dir(cfg.RealCC), "ar")
}

// archiveSandbox handles NIXGG_SANDBOX=1: emit a JSON drv describing
// this archive step, hand it to `nix derivation add`, symlink the
// output at the returned drv path. Usually doesn't submit — archives
// are usually intermediate, consumed by a later link — but DOES
// submit when NIXGG_SANDBOX_TARGET names this archive explicitly
// (e.g. a static-lib-only build, or one of a multi-target build's
// targets is itself an archive). See maybeSubmit's own docstring for
// the naming override a multi-target match needs, mirrored here the
// same way linkSandbox does it.
func archiveSandbox(
	cfg *toolchain.Config,
	archive, modifiers string,
	inputs []expr.JSONDrvInput,
	storeDeps []string,
	wrapperEnvJSON string,
) error {
	outName := filepath.Base(archive)
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}
	// `ar` lives in the same dir as the caller's real cc — that's the
	// gcc-wrapper's binutils dependency.
	arRoot := filepath.Dir(filepath.Dir(cfg.RealCC)) // strip /bin

	name := "ar-" + outName
	if override := multiTargetName(archive); override != "" {
		name = override
	}
	drv := expr.ArchiveJSON(expr.ArchiveJSONParams{
		Name:        name,
		OutName:     outName,
		System:      cfg.System,
		Bash:        cfg.BashRoot,
		Coreutils:   cfg.CoreutilsRoot,
		AR:          arRoot,
		ARFlags:     modifiers,
		Inputs:      inputs,
		StoreDeps:   storeDeps,
		Placeholder: "/" + expr.OutPlaceholderNix32,
		ExtraSrcs: []string{
			baseNameOf(cfg.BashRoot),
			baseNameOf(cfg.CoreutilsRoot),
			baseNameOf(arRoot),
		},
		Env: wrapperEnv,
	})
	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(archive, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s", drvPath)
	activitylog.Emit("ar", "drv", activitylog.Fields{"archive": archive, "drv": drvPath})

	// See maybeSubmit's comment.
	maybeSubmit(cfg, drvPath, archive, false)
	return nil
}
