// Package mode decides whether a given shim invocation defers via a
// placeholder thunk or realises synchronously.
//
// Placeholder is the default: every compile writes a .nix expression
// file and symlinks the output at it. The link shim's inline realise
// hook (NIXGG_AUTOFORCE=1) or an explicit `nixgg force` materialises
// the whole DAG in one Nix invocation at the end.
//
// Realise mode is a narrow carveout for cases where a downstream tool
// needs to run the just-produced artifact before make continues:
// autoconf conftests (`if ./conftest; then ... fi`) and cmake's
// try_compile probes are the canonical examples. In those cases the
// probe would see a .nix thunk file where it expected a runnable ELF.
// The decision is made per-TU by filename pattern — the outer build
// doesn't need to know or care.
package mode

import (
	"path/filepath"
	"strings"
)

// Mode is the result of the placeholder-vs-realise decision.
type Mode int

const (
	Placeholder Mode = iota
	Realise
	// Passthrough: run the real compiler in the build tree and model
	// nothing.
	//
	// A different reason from Realise. Realise is for builds that need a
	// runnable artifact immediately; Passthrough is for compiles the
	// build EXPECTS TO FAIL, where the failure is the result and the
	// artifact is the compiler's stderr.
	//
	// Accelerating those is not merely wasteful, it is wrong: a
	// derivation that fails, fails the build, whereas the caller was
	// going to inspect the error and carry on. Realise is no help
	// either — `nix build` on a deliberately-failing compile fails just
	// as hard.
	Passthrough
)

// For returns the mode for a given source or output path.
//
// Placeholder unless the path matches:
//   - a caller-declared passthrough subtree (passthrough.go)
//   - autoconf conftests (basename starts with "conftest")
//   - cmake compiler-detection files (test?Compiler…, CheckXXX…)
//   - cmake TryCompile scratch (path contains CMakeFiles/CMake{Scratch,Tmp})
//
// Every pattern here was added because a real project tripped it.
//
// Deliberately NOT consulted by the link or archive shims. It looks like
// an omission — a `try_run` probe execs a link output, so surely the link
// shim needs the same carveout? — but both reachable cases are already
// handled elsewhere:
//
//   - autoconf AC_RUN_IFELSE and cmake try_run run at CONFIGURE time,
//     and every example runs its configure phase under NIXGG_BYPASS=1.
//     bypassed() returns before any mode check, so those links never
//     reach this package at all.
//
//   - Build-time codegen tools (llvm-tblgen, protoc, a project's own
//     generator) are NOT bypassed, and they are the real case. But there
//     is no filename pattern that identifies them: llvm-tblgen looks
//     exactly like llvm-config. Any guess would either miss tools or
//     eagerly realise things that should stay lazy, forfeiting the
//     parallelism that is the point of deferring. Those builds use the
//     phase-chain pattern instead — see examples/two-phase for the
//     minimal shape and examples/llvm for a three-phase real one, where
//     phase N+1 consumes phase N's realised output via buildInputs.
//
// So: if you are here because a mid-build exec got a thunk or a drvref
// stub, the fix is a phase split, not a new pattern in this file.
func For(path string) Mode {
	base := filepath.Base(path)
	switch {
	// Project-specific subtrees the caller declared — see passthrough.go.
	case matchesPassthrough(path):
		return Passthrough
	case strings.HasPrefix(base, "conftest"):
		return Realise
	case matchCMakeProbe(base):
		return Realise
	case strings.Contains(path, "/CMakeFiles/CMakeScratch/") ||
		strings.Contains(path, "/CMakeFiles/CMakeTmp/"):
		return Realise
	}
	return Placeholder
}

func matchCMakeProbe(base string) bool {
	// e.g. testCCompiler.c, testCXXCompilerABI_C.cpp,
	//      CMakeCCompilerId.c, CMakeCXXCompilerABI_CXX.cpp
	if strings.HasPrefix(base, "test") && strings.Contains(base, "Compiler") {
		return true
	}
	if strings.HasPrefix(base, "CMake") && strings.Contains(base, "Compiler") {
		return true
	}
	// CheckFunctionExists, CheckIncludeFile, CheckCSourceCompiles, ...
	if strings.HasPrefix(base, "Check") &&
		(strings.Contains(base, "Exists") ||
			strings.Contains(base, "Include") ||
			strings.Contains(base, "SourceCompiles") ||
			strings.Contains(base, "SourceRuns") ||
			strings.Contains(base, "SymbolExists") ||
			strings.Contains(base, "TypeSize")) {
		return true
	}
	return false
}
