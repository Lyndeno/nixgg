package shim

import (
	"github.com/tbereknyei/nixgg/internal/batchmember"
	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// resolvePendingMember is the safe fallback for a deferred batch
// member (see deferCompileToBatch) that ends up NOT part of a
// successful combined-archive submission: any consumer other than a
// same-group archive (a mixed-group archive, a direct link with no
// archive, a foreign -l reference, a manual `nixgg force`) resolves
// the member individually here, into exactly the ordinary per-TU
// thunk symlink / drvref stub Compile's own non-batched path would
// have written — after which output is indistinguishable from a TU
// that was never batched at all.
//
// Idempotent (thunk.Write and sandbox.PointOutputAtDrv already are),
// so it's safe to call from more than one place — the classifyInputs
// fallback prologue, resolveLibFlag, and cli/force.go's per-target
// loop all reach it.
//
// output is unchanged (still a batch-pending stub) if this returns
// an error, or if output does not reference a pending member at all
// (returns nil, a no-op).
func resolvePendingMember(cfg *toolchain.Config, l paths.Layout, output string) error {
	recordPath := batchpending.Path(output)
	if recordPath == "" {
		return nil // not pending; nothing to do
	}
	m, err := batchmember.Read(recordPath)
	if err != nil {
		return err
	}
	if sandbox.Enabled() {
		return submitCompileSandboxDrv(cfg, m.Tool, m.OutName, output, m.Source, m.SrcStore,
			m.Flags, m.StoreDeps, m.WrapperEnv)
	}
	e := expr.Compile(expr.CompileParams{
		Helpers:    cfg.Helpers,
		Tool:       m.Tool,
		SrcTree:    m.SrcTreeLiteral,
		Source:     m.Source,
		OutName:    m.OutName,
		Flags:      m.Flags,
		StoreDeps:  m.StoreDeps,
		WrapperEnv: m.WrapperEnv,
	})
	thunkPath, err := submitCompileThunk(l, e, output)
	if err != nil {
		return err
	}
	logf("  thunk:      %s (resolved from deferred batch member)", thunkPath)
	return nil
}
