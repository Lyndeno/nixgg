package shim

import (
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/expr"
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
//
// Before classifying, each input is checked against
// batchpending.Path: if it's a still-deferred batch member (see
// deferCompileToBatch), it's resolved into an ordinary per-TU
// thunk/drv HERE, via ResolvePendingMember, before classify.Target
// ever sees it. This is what makes batching safe for every consumer
// that ISN'T a same-group archive (archive.go's own tryBatchArchive
// checks for all-same-group-pending BEFORE calling classifyInputs at
// all, and only reaches this prologue when that fast path didn't
// apply): a mixed-group archive, a direct link with no archive, or
// any other caller of this function transparently falls back to
// today's one-derivation-per-TU behavior for that one input, with
// classify.Target none the wiser that the input was ever deferred.
func classifyInputs(
	cfg *toolchain.Config, inputs []string, altPrefix string, l paths.Layout, logPrefix string, passthrough func() error,
) (linkInputs []expr.Input, jsonInputs []expr.JSONDrvInput, err error, ok bool) {
	linkInputs = make([]expr.Input, 0, len(inputs))
	jsonInputs = make([]expr.JSONDrvInput, 0, len(inputs))
	for _, in := range inputs {
		if batchpending.Is(in) {
			if err := ResolvePendingMember(cfg, l, in); err != nil {
				logf("%s passthrough: resolving deferred batch member %s: %v", logPrefix, in, err)
				return nil, nil, passthrough(), false
			}
		}
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
			//
			// Name is normally the caller's own argv basename — correct
			// when the caller referenced the drv's real output
			// directly. But c.Sub is set when classify.Target followed
			// a SONAME alias symlink (libcrypto.so -> libcrypto.so.3)
			// to reach the drv; there, the caller's basename
			// ("libcrypto.so") is NOT the name the drv's own output
			// will exist under once resolved ("libcrypto.so.3"), so
			// c.Sub — the alias's real target basename — must win.
			name := filepath.Base(in)
			if c.Sub != "" {
				name = c.Sub
			}
			jsonInputs = append(jsonInputs, expr.JSONDrvInput{
				Kind: "drv", Ref: c.Ref, Name: name,
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
