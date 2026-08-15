package shim

import (
	"fmt"
	"path/filepath"
	"strings"

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
		return Passthrough(realARFor(cfg), args)
	}

	logf("archive %s <- %s", archive, joinBase(inputs))

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
	// Bail explicitly. Until now these were rejected only by accident:
	// the archive name in `ar rN 3 archive.a obj.o` failed the
	// members-must-end-in-.o check. Allowing `.a` members for kbuild
	// removed that accident, so the guard has to be real.
	if strings.ContainsAny(modifiers, "abiN") {
		return "", "", nil, false
	}
	archive = args[1]
	for _, in := range args[2:] {
		// `.a` members are not a curiosity: kbuild nests archives, so
		// lib/math/built-in.a lists lib/math/tests/built-in.a as a
		// member alongside its own objects. Rejecting them sent every
		// kernel directory down the passthrough path, where the real
		// `ar` then met a drvref stub instead of an object file.
		if !strings.HasSuffix(in, ".o") && !strings.HasSuffix(in, ".a") {
			// Anything else — skip modeling.
			return "", "", nil, false
		}
		inputs = append(inputs, in)
	}
	// A creating invocation with NO members is legitimate and must be
	// modelled. kbuild emits `ar cDPrST <dir>/built-in.a` with an empty
	// member list for any directory whose objects are all modules, and
	// leaving those to passthrough is not harmless: the result is a
	// plain file, its PARENT archive then reports "can't model input
	// (regular)" and passes through too, and the damage climbs the tree
	// until `ld -r vmlinux.o <- vmlinux.a` meets a half-modelled
	// archive and dies with
	//
	//	ld.bfd: vmlinux.a: member init/built-in.a in archive is not an object
	//
	// One empty archive poisons every ancestor. Verified directly that
	// `ar DcDPrST empty.a` succeeds and that a parent thin archive can
	// include the result.
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
	// `T` (thin archive) is in here deliberately. The kernel builds
	// every built-in.a with `ar cDPrST`, and its absence meant that one
	// character sent the whole invocation to passthrough — where the
	// real ar was handed drvref stubs.
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
// output at the returned drv path. Never submits — archives are
// intermediate; only the link shim submits.
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

	drv := expr.ArchiveJSON(expr.ArchiveJSONParams{
		Name:        "ar-" + outName,
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

	// See maybeSubmit's comment.
	maybeSubmit(cfg, drvPath, archive, false)
	return nil
}

var _ = fmt.Sprintf // keep import; used only when sandbox path is compiled
