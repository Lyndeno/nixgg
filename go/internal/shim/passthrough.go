package shim

import (
	"os"
	"syscall"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/realise"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Passthrough replaces the current process image with the real tool.
// This is the right thing for shims that decide not to model a call:
// we don't pay for a fork, stdin/stdout/stderr are already correct,
// and the caller sees the tool's true exit code.
func Passthrough(realTool string, args []string) error {
	argv := append([]string{realTool}, args...)
	return syscall.Exec(realTool, argv, os.Environ())
}

// RealiseThunkArgsAndPassthrough realizes every argv token that
// classifies as one of OUR OWN not-yet-realized native-mode outputs
// (a symlink to a .nix thunk) before exec-ing the real tool — then
// calls Passthrough.
//
// Why this exists: when Archive/Link give up modeling an invocation
// (an unrecognized modifier string, a sibling input it can't
// classify, `ar t`/`p`/`x` read-mode, etc.) and fall to Passthrough,
// the REAL tool may read OTHER argv tokens that are still nixgg's own
// deferred placeholder thunk symlinks — not real bytes. For most
// tools this fails loudly (a compiler can't parse a Nix expression as
// source). But `ar` with a thin (`T`) modifier is the dangerous case:
// thin mode stores each member's own file PATH rather than reading
// its content, so `ar cDPrST archive.a <mix of real .o and still-
// thunk .o paths>` SUCCEEDS — silently producing an archive whose
// member list includes literal `.nixgg/thunks/<id>.nix` paths. No
// error anywhere, until a MUCH later consumer (a linker, `nm`, `ar
// mPiT`'s own re-read of the same archive) tries to treat one of
// those paths as real object bytes and fails confusingly far from the
// actual cause.
//
// Confirmed directly against a real Linux kernel build: Kbuild's own
// recursive `built-in.a` construction routinely has ONE unmodelable
// sibling member (e.g. a genuinely empty subsystem archive, itself
// already a real, plain file) alongside many real nixgg thunks for
// the OTHER members — the single unmodelable sibling sends the WHOLE
// `ar` call to Passthrough via classifyInputs' own passthrough
// callback, and every thunk sibling gets baked into the resulting
// thin archive as a dangling path instead of being realized first.
//
// Sandbox mode isn't handled here: realizing a single registered drv
// on demand (rather than at the end of a whole build) has no existing
// mechanism to reuse — confirmed architecturally impossible, not just
// unscoped. builder-rpc-v0's own daemon (src/libstore/daemon.cc's
// performOp) allowlists exactly AddToStore{,Multiple,Nar,Scanning},
// SubmitOutput, AddTempRoot, and IsValidPath for a RecursiveSubmitted
// connection — BuildDerivation/BuildPaths are not on that list, and
// any other op throws "Operation %d not allowed inside derivation"
// (deliberate upstream restriction, "to reduce opportunities for
// nonreproducibility in builds", not a client-side gap this package
// could work around). Confirmed directly against a real sandboxed
// Linux kernel build reaching vmlinux.a's own final assembly: one
// unmodelable sibling (a disabled subsystem's empty built-in.a) sends
// the whole `ar`/`ld` call to Passthrough, and the OTHER siblings are
// still un-realized sandbox-mode drv stubs (plain text files, not
// real archive bytes) with no synchronous way to materialize them
// before the real tool reads them — `ld.bfd: member ... is not an
// object`. A no-op in sandbox mode, so callers can use this
// unconditionally.
func RealiseThunkArgsAndPassthrough(cfg *toolchain.Config, l paths.Layout, realTool string, args []string, sandboxEnabled bool) error {
	if !sandboxEnabled {
		altPrefix := altStorePrefix(cfg.Store)
		for _, a := range args {
			if c := classify.Target(a, altPrefix, l); c.Kind == classify.Thunk {
				if err := realise.Realise(l, cfg, c.Ref, a); err != nil {
					logf("  realise-before-passthrough: %s: %v", a, err)
				}
			}
		}
	}
	return Passthrough(realTool, args)
}

// bypassed reports whether the shim should skip nixgg's derivation
// path and just exec the real tool. Controlled by NIXGG_BYPASS.
//
// Intended for phases that can't route through nixgg without
// breaking — most commonly autoconf `./configure` or cmake's
// probe phase, which compile+exec tiny binaries synchronously.
// Users flip it around those phases:
//
//	NIXGG_BYPASS=1 ./configure
//	make      # NIXGG_BYPASS unset → shims fire as normal
//
// Any non-empty, non-"0" value is truthy.
func bypassed() bool {
	v := os.Getenv("NIXGG_BYPASS")
	return v != "" && v != "0"
}
