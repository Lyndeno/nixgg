package shim

import (
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/mode"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// storeInput builds the pair of per-input records a Store-classified
// link/archive input needs — one for each wire format.
//
// The subtlety is `Name`. Both serializers render an input's argv token as
// Ref+"/"+Name, so Name must be the path relative to the store ROOT, not
// the caller-visible basename. Those coincide for everything nixgg
// produces (a drv output dir holding one artifact) but not for a
// dependency it merely consumes: LLVM's cmake puts an absolute
// `…-zlib-1.3.2/lib/libz.so` on the link line, and using the basename
// there yields `…-zlib-1.3.2/libz.so` — a file that does not exist, so the
// link fails with `ld.bfd: cannot find`.
//
// Ref stays the root because that is what `builtins.storePath` (native)
// and inputs.srcs (sandbox) accept; neither takes a subpath.
//
// Shared by link.go and archive.go so the two cannot drift, and so this is
// reachable from a test — the original bug lived in code only exercised
// through a full build.
func storeInput(c classify.Result, callerPath string) (expr.Input, expr.JSONDrvInput) {
	rel := c.Sub
	if rel == "" {
		// No Sub means classification could not observe the artifact's
		// position inside its store path. Two ways that happens, and they
		// need opposite treatment:
		//
		//   - A foreign dependency reached through a symlink: Sub IS set
		//     (classify resolved the link), so we never get here.
		//   - One of OUR OWN outputs that `force` promoted to a real file:
		//     the promoted registry records only the store ROOT, so Sub is
		//     empty and the artifact's FHS subdir has to be re-derived.
		//
		// Missing the second case broke native-mode lua: liblua.a lives at
		// <root>/lib/liblua.a but was referenced as <root>/liblua.a, and
		// luac failed with `ld: cannot find …-ar-liblua.a/liblua.a`.
		base := filepath.Base(callerPath)
		if sub := expr.ArtifactSubdir(base); sub != "" {
			rel = sub + "/" + base
		} else {
			rel = base
		}
	}
	return expr.Input{
			Kind: "store", Ref: c.Ref, Name: rel,
		}, expr.JSONDrvInput{
			Kind: "src", Ref: filepath.Base(c.Ref), Name: rel,
		}
}

// classifyInputs classifies every input path a link or archive step
// received, so link.go and archive.go can't drift on how a
// Store/Thunk/Drv classification becomes an expr.Input/JSONDrvInput
// pair. logPrefix names the caller in log lines ("link"/"ar");
// passthrough runs the moment an input can't be modeled, and its
// error is returned with ok=false.
func classifyInputs(
	cfg *toolchain.Config,
	inputs []string, altPrefix string, l paths.Layout, logPrefix string, passthrough func() error,
) (linkInputs []expr.Input, jsonInputs []expr.JSONDrvInput, err error, ok bool) {
	linkInputs = make([]expr.Input, 0, len(inputs))
	jsonInputs = make([]expr.JSONDrvInput, 0, len(inputs))
	for _, in := range inputs {
		c := classify.Target(in, altPrefix, l)
		switch c.Kind {
		case classify.Store:
			ni, ji := storeInput(c, in)
			linkInputs = append(linkInputs, ni)
			jsonInputs = append(jsonInputs, ji)
		case classify.Thunk:
			linkInputs = append(linkInputs, expr.Input{
				Kind: "nix", Ref: c.Ref, Name: filepath.Base(in),
			})
		case classify.Drv:
			// Sandbox-mode input: previous shim produced a .drv here.
			// Only meaningful when we're also in sandbox mode.
			jsonInputs = append(jsonInputs, expr.JSONDrvInput{
				Kind: "drv", Ref: c.Ref, Name: filepath.Base(in),
			})
		case classify.Regular:
			// A real file nixgg did not produce. Not every input comes
			// from a shim: the kernel compiles some objects with rustc
			// (rust/core.o, drivers/gpu/drm/drm_panic_qr.o), which nixgg
			// does not model, so they are ordinary files on disk.
			//
			// Bailing here is not a local decision. An unmodellable input
			// makes THIS archive passthrough, which makes it a plain file,
			// which makes its parent unmodellable in turn — the same
			// cascade the empty-archive case caused, climbing from
			// drivers/gpu/drm all the way to vmlinux.a.
			//
			// So put the file in the store and depend on its content. It
			// is content-addressed, so this stays deterministic; the cost
			// is one `nix store add` for an input that is rare by
			// construction.
			//
			// Sandbox mode only. Native mode has no cascade to break (its
			// inputs are thunks on disk, and a Regular there means the
			// caller is doing something we deliberately do not model), and
			// adding a store round-trip to that path would change drv
			// content for builds that work today.
			if !sandbox.Enabled() {
				logf("%s passthrough: can't model input %s (%s)", logPrefix, in, c.Reason())
				return nil, nil, passthrough(), false
			}
			sp, err := storeAddLooseFile(cfg, in)
			if err != nil {
				logf("%s passthrough: store-add %s failed: %v", logPrefix, in, err)
				return nil, nil, passthrough(), false
			}
			jsonInputs = append(jsonInputs, expr.JSONDrvInput{
				Kind: "src", Ref: filepath.Base(sp), Name: filepath.Base(in),
			})
		default:
			logf("%s passthrough: can't model input %s (%s)", logPrefix, in, c.Reason())
			return nil, nil, passthrough(), false
		}
	}
	return linkInputs, jsonInputs, nil, true
}

// maybeSubmit submits drvPath as the outer derivation's "out" output
// iff path matches NIXGG_SANDBOX_TARGET, or TARGET is unset and
// defaultSubmit is true.
//
// linkSandbox passes defaultSubmit=true (a link is usually the final
// artifact); archiveSandbox passes false (an archive is usually
// intermediate, consumed by a later link — it only submits when
// TARGET names it explicitly, e.g. a static-lib-only build).
func maybeSubmit(cfg *toolchain.Config, drvPath, path string, defaultSubmit bool) {
	target := os.Getenv("NIXGG_SANDBOX_TARGET")
	submit := defaultSubmit
	if target != "" {
		submit = matchesTarget(target, path)
	}
	if !submit {
		return
	}
	if err := sandbox.SubmitOutput(cfg, drvPath, "out"); err != nil {
		logf("  submit-output: %v", err)
	} else {
		logf("  submitted: %s", drvPath)
	}
}

// storeAddLooseFile puts a single build-tree file into the store as a
// DIRECTORY containing it.
//
// `nix store add <file>` would give a store path that IS the file, but
// both serializers render an input's argv token as Ref+"/"+Name and
// expect Ref to be a directory — the shape every drv output already
// has. Staging into a one-file directory keeps that invariant instead
// of special-casing the emitters, which are the byte-identity-critical
// part of the codebase.
func storeAddLooseFile(cfg *toolchain.Config, path string) (string, error) {
	base := filepath.Base(path)
	tmp, err := os.MkdirTemp(os.Getenv("NIX_BUILD_TOP"), "gg-loose-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, base), src, 0o444); err != nil {
		return "", err
	}
	return sandbox.StoreAddScan(cfg, base, tmp)
}

// carvedOut reports whether a subtree is excluded from modelling.
//
// mode.For's directory carveouts mean "nixgg does not model anything
// here" — but until run 10 only the compile shim consulted it. That was
// survivable by accident: the compiles passed through, so the archive
// and link shims saw Regular inputs and bailed, and the whole subtree
// fell through together.
//
// Store-adding Regular inputs removed that accident. arch/x86/purgatory
// compiles passed through as intended, then `ld -r` happily modelled
// purgatory.ro from the store-added objects — and purgatory.chk, a full
// link that is passed through, met the stub:
//
//	ld.bfd: purgatory.ro: file format not recognized
//
// So every shim that produces an artifact has to honour the carveout
// directly rather than inherit it from its inputs.
func carvedOut(path string) bool {
	return mode.For(path) == mode.Passthrough
}
