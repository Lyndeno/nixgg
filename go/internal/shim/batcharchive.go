package shim

import (
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/batchmember"
	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/thunk"
	"github.com/tbereknyei/nixgg/internal/toolchain"
	"github.com/tbereknyei/nixgg/internal/wrapperenv"
)

// tryBatchArchive is Archive's fast path for a same-group batch: if
// every one of ar's own inputs is a still-pending member of the SAME
// batch group (see deferCompileToBatch), it combines all of them plus
// this archive step into ONE derivation instead of N+1. Runs BEFORE
// classifyInputs, so on any failure to qualify — a foreign/regular
// input, a mismatched group, or (sandbox mode) this archive being the
// build's own submission target — nothing has been touched yet, and
// Archive's own unmodified classifyInputs path runs exactly as if
// tryBatchArchive didn't exist, resolving each pending member
// individually via its own fallback prologue.
//
// handled=false (with a nil error) means "did not apply, fall
// through to Archive's normal path" — the ONLY other case that
// matters for correctness. handled=true always carries either a
// non-nil error (a real failure — submitting the combined
// derivation, for instance) or nil (fully submitted).
func tryBatchArchive(cfg *toolchain.Config, l paths.Layout, archive, modifiers string, inputs []string) (handled bool, err error) {
	// Never batch the archive that IS this build's own submission
	// target: mkNixggBuild.nix's submit-output naming contract
	// requires the submitted drv's basename to be exactly
	// "ar-<target>.drv"/"bin-<target>.drv" — a "batch-"-named drv
	// here would violate that and fail the build. See
	// go/internal/batch's own package docstring / this feature's
	// design notes for why this is a known, narrow gap: whichever
	// archive happens to be the build's own target never benefits
	// from batching, for a reason unrelated to its own group
	// membership.
	if sandbox.Enabled() && matchesTarget(os.Getenv("NIXGG_SANDBOX_TARGET"), archive) {
		return false, nil
	}

	members, ok := collectSameGroupMembers(inputs)
	if !ok {
		return false, nil
	}

	return true, submitCombinedArchive(cfg, l, archive, modifiers, members)
}

// collectSameGroupMembers reads every input's batch-pending record,
// in ar's own argv order, and reports ok=false the moment ANY input
// isn't a pending member at all, or belongs to a different group than
// the first — either case means this archive isn't a pure same-group
// batch, and the caller must fall through to per-input resolution.
func collectSameGroupMembers(inputs []string) (members []batchmember.MemberRecord, ok bool) {
	if len(inputs) == 0 {
		return nil, false
	}
	members = make([]batchmember.MemberRecord, 0, len(inputs))
	var group string
	for i, in := range inputs {
		recPath := batchpending.Path(in)
		if recPath == "" {
			return nil, false
		}
		m, err := batchmember.Read(recPath)
		if err != nil {
			return nil, false
		}
		if i == 0 {
			group = m.Group
		} else if m.Group != group {
			return nil, false
		}
		members = append(members, m)
	}
	return members, true
}

// submitCombinedArchive builds and submits the combined derivation
// covering every member's compile plus this archive step, then
// reuses the EXACT SAME post-build calls Archive's own non-batched
// path already makes for its output — so the resulting archive
// classifies downstream (via classify.Target) as an ordinary
// Thunk/Drv, identical to any other archive's output. Nothing in
// link.go needs to know this archive was ever batched.
func submitCombinedArchive(cfg *toolchain.Config, l paths.Layout, archive, modifiers string, members []batchmember.MemberRecord) error {
	outName := filepath.Base(archive)

	// Wrapper env is computed fresh, once, here — not reconciled from
	// each member's own snapshot. They should always agree (one
	// ambient build-wide env); a single authoritative computation
	// avoids ever needing to detect/merge a disagreement. Matches
	// Archive's own non-batched path, which does the same.
	wrapperEnvJSON, err := wrapperenv.JSON()
	if err != nil {
		return err
	}
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}

	// Union of every member's own StoreDeps plus the archive's own —
	// same computation Archive's non-batched path makes for its own
	// StoreDeps, unioned across members since the combined derivation
	// is one set of inputs.srcs for all of them together.
	storeDeps := unionStoreDeps(members, storedeps.From(nil, wrapperEnvJSON, cfg.KnownStorePaths))

	if sandbox.Enabled() {
		return submitCombinedArchiveSandbox(cfg, archive, outName, modifiers, members, storeDeps, wrapperEnv)
	}
	return submitCombinedArchiveNative(cfg, l, archive, outName, modifiers, members, storeDeps, wrapperEnv)
}

// unionStoreDeps returns the deduplicated union of every member's own
// StoreDeps plus extra (the archive's own).
func unionStoreDeps(members []batchmember.MemberRecord, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, m := range members {
		for _, s := range m.StoreDeps {
			add(s)
		}
	}
	for _, s := range extra {
		add(s)
	}
	return out
}

// submitCombinedArchiveNative builds the native-mode combined
// expression and submits it through the identical thunk-write/
// symlink/record calls Archive's own non-batched path uses.
func submitCombinedArchiveNative(cfg *toolchain.Config, l paths.Layout, archive, outName, modifiers string, members []batchmember.MemberRecord, storeDeps []string, wrapperEnv map[string]string) error {
	batchMembers := make([]expr.BatchCompileMember, len(members))
	for i, m := range members {
		batchMembers[i] = expr.BatchCompileMember{
			Tool:    m.Tool,
			SrcTree: m.SrcTreeLiteral,
			Source:  m.Source,
			OutName: m.OutName,
			Flags:   m.Flags,
		}
	}
	e := expr.BatchArchive(expr.BatchArchiveParams{
		Helpers:    cfg.Helpers,
		OutName:    outName,
		ARFlags:    modifiers,
		Members:    batchMembers,
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
	logf("  thunk:      %s (combined batch archive, %d members)", thunkPath, len(members))
	return nil
}

// submitCombinedArchiveSandbox builds the sandbox-mode combined JSON
// drv and submits it through the identical DerivationAdd/
// PointOutputAtDrv/maybeSubmit calls Archive's own non-batched
// (archiveSandbox) path uses.
func submitCombinedArchiveSandbox(cfg *toolchain.Config, archive, outName, modifiers string, members []batchmember.MemberRecord, storeDeps []string, wrapperEnv map[string]string) error {
	batchMembers := make([]expr.BatchCompileMember, len(members))
	for i, m := range members {
		batchMembers[i] = expr.BatchCompileMember{
			Tool:     m.Tool,
			SrcStore: m.SrcStore,
			Source:   m.Source,
			OutName:  m.OutName,
			Flags:    m.Flags,
		}
	}
	// `ar` (and every member's own compiler) lives in the same dir as
	// the caller's real cc — the gcc-wrapper's binutils dependency.
	// Same convention archiveSandbox already uses.
	arRoot := filepath.Dir(filepath.Dir(cfg.RealCC)) // strip /bin

	extraSrcs := []string{baseNameOf(cfg.BashRoot), baseNameOf(cfg.CoreutilsRoot), baseNameOf(arRoot)}

	drv := expr.BatchArchiveJSON(expr.BatchArchiveJSONParams{
		Name:      "batch-" + outName,
		OutName:   outName,
		System:    cfg.System,
		Bash:      cfg.BashRoot,
		Coreutils: cfg.CoreutilsRoot,
		AR:        arRoot,
		ARFlags:   modifiers,
		Members:   batchMembers,
		StoreDeps: storeDeps,
		ExtraSrcs: extraSrcs,
		Env:       wrapperEnv,
	})

	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(archive, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s (combined batch archive, %d members)", drvPath, len(members))

	// See maybeSubmit's own comment — an archive only submits when
	// NIXGG_SANDBOX_TARGET names it explicitly (a static-lib-only
	// build). tryBatchArchive already refused to batch the actual
	// target archive, so defaultSubmit=false here is never reached
	// via a mismatched-but-still-target path — this mirrors
	// archiveSandbox's own call exactly for defensiveness.
	maybeSubmit(cfg, drvPath, archive, false)
	return nil
}
