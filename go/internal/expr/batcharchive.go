// Package expr's batch-archive emitters: a NEW derivation shape that
// combines N compiles + 1 archive into ONE derivation, for a same-
// group batch (see internal/batch). Purely additive — does not touch
// Derivation, Kind, buildScript, ToNix, or toJSON, to keep zero risk
// of perturbing the byte-pinned output of the three existing Kinds
// (KindCompile/KindLink/KindArchive), which tests/drv-equivalence.sh
// depends on matching exactly across every existing fixture.
//
// Unlike every existing Kind, this derivation's inputs are never a
// not-yet-realized sibling drv/thunk — by construction, the caller
// (internal/shim's tryBatchArchive) only reaches this once every
// input is confirmed to be a plain staged source tree belonging to
// the same batch group. So none of internal/expr's own
// @NIXGG_*@-marker / native-mode resolve-script.nix substitution
// machinery applies here.
//
// Sandbox mode's script is fully-resolved text, same as any Kind's
// own script() with tag=="". Native mode splits differently from
// every other Kind: Go renders each member's compile line as PLAIN,
// already shell-quoted text (compileLine, via shellQuoteFlags — same
// escaping every other Kind's Flags gets) but leaves the member's
// own srcTree as a Nix path literal for nix/batchArchiver.nix ITSELF
// to interpolate into the final script, the same way builder.nix
// already interpolates its own single srcTree. Go never sees the
// resolved store path; Nix never re-quotes shell text. This is the
// one place a batch-archive derivation's construction is genuinely
// split between Go and Nix rather than "Go renders complete text" —
// see BatchArchive's own docstring for why.
//
// Named "batch-<outName>" rather than "ar-<outName>", deliberately:
// tests/drv-equivalence.sh's own existing filter
// (^[a-z0-9]+-(tu-|ar-|bin-)) is regex-based and would otherwise see
// this as an ar-produced drv with no native-mode counterpart of the
// SAME shape, and report a false-positive-looking "only in sandbox"
// mismatch. The "batch-" prefix is invisible to that filter by
// construction — tests/batch-drv-equivalence.sh is this shape's own,
// separate equivalence check.
package expr

import (
	"fmt"
	"strings"
)

// BatchCompileMember is one compile folded into a combined batch
// archive, in the archive's own `ar` argv order (order matters: it
// determines both compile-then-archive script order and the
// resulting archive's own member order).
type BatchCompileMember struct {
	Tool     string // "cc", "gcc", "c++", "g++"
	SrcTree  string // native: Nix path literal, e.g. "../srcs/foo" (unused in sandbox JSON path)
	SrcStore string // sandbox: full /nix/store/... path, already uploaded (unused in native path)
	Source   string // relative path inside the src tree, e.g. "sds.c"
	OutName  string // "sds.o"
	Flags    []string
}

// BatchArchiveParams is the native-mode input for one combined
// batch-archive expression.
type BatchArchiveParams struct {
	Helpers    string
	OutName    string // the archive's own output name, e.g. "libhiredis.a"
	ARFlags    string
	Members    []BatchCompileMember
	StoreDeps  []string
	WrapperEnv map[string]string
}

// BatchArchive renders a native-mode `import
// <helpers>/batchArchiver.nix { ... }` expression. Mirrors
// Derivation.ToNix's KindArchive case in shape, but constructs the
// call directly rather than going through Derivation, since this
// Kind's argument shape (a members list, not a single scriptTemplate)
// doesn't fit ToNix's existing per-Kind switch.
//
// Each member's compileLine is fully shell-quoted PLAIN TEXT (see
// memberCompileLine) with no reference to its own srcTree at all —
// nix/batchArchiver.nix splices `cd ${member.srcTree} && ` onto the
// front of each compileLine itself, at eval time, the same way
// builder.nix interpolates its own srcTree. This keeps every value
// Go computes here shell-safe without needing this package to also
// know Nix's own string-escaping rules for a value it never actually
// resolves (srcTree is a bare path literal Nix resolves internally;
// Go only ever sees the literal text of the Nix expression, never the
// resulting store path).
func BatchArchive(p BatchArchiveParams) string {
	var b strings.Builder
	fmt.Fprintf(&b, "import %s/batchArchiver.nix {\n", p.Helpers)
	fmt.Fprintf(&b, "  outName        = %q;\n", p.OutName)
	fmt.Fprintf(&b, "  arFlags        = %q;\n", p.ARFlags)
	b.WriteString("  members        = ")
	b.WriteString(batchMembersList(p.Members))
	b.WriteString(";\n")
	fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(p.StoreDeps))
	fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(p.WrapperEnv))
	b.WriteString("}\n")
	return b.String()
}

// batchMembersList renders `[ { srcTree = ...; outName = ...;
// compileLine = ”...”; } ... ]` — srcTree stays an unquoted Nix
// path literal (Nix resolves it to a store path at eval time, same
// convention as builder.nix's own srcTree argument); compileLine is
// a Nix indented string (may contain the double quotes memberCompileLine
// itself already emitted, so a plain %q would double-escape them).
func batchMembersList(members []BatchCompileMember) string {
	if len(members) == 0 {
		return "[ ]"
	}
	var b strings.Builder
	b.WriteString("[\n")
	for _, m := range members {
		b.WriteString("    { ")
		fmt.Fprintf(&b, "srcTree = %s; ", m.SrcTree) // unquoted path literal
		fmt.Fprintf(&b, "outName = %q; ", m.OutName)
		fmt.Fprintf(&b, "compileLine = %s; ", nixIndentedStringLiteral(memberCompileLine(m)))
		b.WriteString("}\n")
	}
	b.WriteString("  ]")
	return b.String()
}

// memberCompileLine renders one member's compile invocation as plain
// shell text, everything already resolved and quoted EXCEPT the
// leading `cd` into its own srcTree (left to the Nix side — see
// package docstring). Shape matches buildScript's own KindCompile
// case exactly, just without the surrounding `cd "$src"`.
func memberCompileLine(m BatchCompileMember) string {
	return fmt.Sprintf(`"%s" %s -c "%s" -o "$objroot/%s"`,
		m.Tool, shellQuoteFlags(m.Flags), m.Source, m.OutName)
}

// BatchArchiveJSONParams is the sandbox-mode input for one combined
// batch-archive JSON drv.
type BatchArchiveJSONParams struct {
	Name      string // derivation name, e.g. "batch-libhiredis.a" — no .drv suffix
	OutName   string // the archive's own output name, e.g. "libhiredis.a"
	System    string
	Bash      string
	Coreutils string
	AR        string // full /nix/store/... path to gnu binutils (for `ar`); also
	// put on PATH for compiling — every member's Tool must be reachable
	// from the same bin/ dir, same convention compileSandbox already
	// assumes for a single build's toolchain.
	ARFlags   string
	Members   []BatchCompileMember // SrcStore populated, not SrcTree
	StoreDeps []string
	ExtraSrcs []string
	Env       map[string]string
}

// BatchArchiveJSON produces a JSONDrv for a combined batch-archive
// step: N compiles then 1 archive, one derivation, one "out" output
// (the archive itself — same single-output shape as an ordinary
// KindArchive derivation, so downstream consumption via
// thunk.LinkPlaceholder / sandbox.PointOutputAtDrv needs no changes).
//
// The script text goes into Env["batchScript"] + passAsFile, not
// Args, same fix and same reason as assemble.Build's own
// Env["buildScript"]: a same-group batch large enough to matter
// (ffmpeg's per-library archives, LLVM's libLLVMSupport) embeds one
// full compile invocation per member in the script text, and passing
// that as `args = ["-c", script]` makes exec's own argv block exceed
// the kernel's ARG_MAX/MAX_ARG_STRLEN ceiling — confirmed directly
// against both real projects ("Argument list too long" at build
// time, past ~350 members). passAsFile writes the env var's value —
// with CA output placeholders substituted exactly as anywhere else in
// the derivation — to a file at build time, exposed via
// `${name}Path`, instead of ever putting it on the builder's argv.
func BatchArchiveJSON(p BatchArchiveJSONParams) JSONDrv {
	script := batchArchiveScript(p.Coreutils, p.AR, p.ARFlags, p.OutName, p.Members)
	srcs := append([]string{}, p.ExtraSrcs...)
	seen := map[string]bool{}
	for _, s := range srcs {
		seen[s] = true
	}
	for _, m := range p.Members {
		base := StoreBasename(m.SrcStore)
		if !seen[base] {
			srcs = append(srcs, base)
			seen[base] = true
		}
	}
	for _, sd := range p.StoreDeps {
		base := StoreBasename(sd)
		if !seen[base] {
			srcs = append(srcs, base)
			seen[base] = true
		}
	}
	env := map[string]string{
		"out":            "/" + OutPlaceholderNix32,
		"name":           p.Name,
		"system":         p.System,
		"builder":        p.Bash + "/bin/bash",
		"outputHashAlgo": "sha256",
		"outputHashMode": "nar",
		"_storeDeps":     strings.Join(p.StoreDeps, ":"),
		"passAsFile":     "batchScript",
		"batchScript":    script,
	}
	for k, v := range p.Env {
		env[k] = v
	}
	return JSONDrv{
		Name:    p.Name,
		System:  p.System,
		Builder: p.Bash + "/bin/bash",
		Args:    []string{"-c", `source "$batchScriptPath"`},
		Env:     env,
		Inputs: JSONDrvInputs{
			Drvs: map[string]JSONDrvRef{}, // never a sibling drv reference — see package docstring
			Srcs: srcs,
		},
		Outputs: map[string]JSONOut{
			"out": {Method: "nar", HashAlgo: "sha256"},
		},
		Version: 4,
	}
}

// batchArchiveScript renders the combined shell script: N compiles
// into a scratch objects dir, then one `ar` over all of them, in
// member order. $objroot is captured before any `cd` so each
// member's own -o target stays absolute regardless of which srcTree
// directory that member's compile runs from.
//
// Compiles run with bounded concurrency, capped at $NIX_BUILD_CORES
// (falls back to 1 if unset/unparseable — never divides by zero,
// never runs more than requested). Folding N TUs into one derivation
// trades away Nix's own per-derivation scheduling, which normally
// runs many TUs' compiles concurrently across the whole build; without
// this, EVERY member of a batch compiles strictly one at a time on a
// single core — confirmed directly via `ps aux` while building
// ffmpeg's libavcodec batch (~350 TUs): exactly one gcc/cc1 process
// running at any moment, for the whole batch's duration, regardless of
// how many cores were available. The FIFO wait below (oldest launched
// member first, not "whichever finishes first") is a deliberate
// choice, not an oversight: `wait -n` only reliably reports a job's
// exit code if the job is STILL RUNNING when `wait -n` is called — a
// background job that already finished before any wait touches it can
// get silently reaped by the shell, and a later `wait -n` (with or
// without an explicit pid list) then returns 127 ("no such job")
// instead of the real exit code, LOSING a real compile failure
// silently. `wait "$pid"` on an explicit, already-known pid does not
// have this problem — confirmed directly (both failure modes
// reproduced and fixed in isolation before wiring this in). The
// tradeoff is head-of-line blocking under wildly uneven per-member
// compile times, which real same-language same-project TUs rarely
// exhibit to a degree that matters.
func batchArchiveScript(coreutils, ar, arFlags, archiveOutName string, members []BatchCompileMember) string {
	var b strings.Builder
	fmt.Fprintf(&b, "set -euo pipefail\n")
	fmt.Fprintf(&b, "export PATH=\"%s/bin:%s/bin\"\n", coreutils, ar)
	b.WriteString("mkdir -p \"$out/lib\" .nixgg-objs\n")
	b.WriteString("objroot=\"$PWD/.nixgg-objs\"\n")
	b.WriteString(batchConcurrencyPreamble)
	for _, m := range members {
		fmt.Fprintf(&b, "(cd \"%s\" && %s) &\n", m.SrcStore, memberCompileLine(m))
		b.WriteString("gg_after $!\n")
	}
	b.WriteString(batchConcurrencyDrain)
	objs := make([]string, 0, len(members))
	for _, m := range members {
		objs = append(objs, "\"$objroot/"+m.OutName+"\"")
	}
	fmt.Fprintf(&b, "ar D%s \"$out/lib/%s\" %s\n", arFlags, archiveOutName, strings.Join(objs, " "))
	return b.String()
}

// batchConcurrencyPreamble/batchConcurrencyDrain implement a bounded-
// concurrency job runner in plain POSIX-ish bash (no external tool —
// this script's only PATH entries are coreutils + the compiler/ar
// root). Each member backgrounds itself (`... &`) and immediately
// calls gg_after with its own $! — gg_after can't take the member's
// whole compound command as its own argument list (a subshell isn't
// a valid argv), so backgrounding stays at each call site and only
// the pid bookkeeping is shared. Once gg_pids reaches gg_max, the
// OLDEST pid is waited on (FIFO) before returning — see
// batchArchiveScript's own docstring for why FIFO, not `wait -n`.
// gg_fail accumulates non-zero exit codes across the whole batch so
// one early failure doesn't get lost behind later successes (plain
// `wait` with no args only ever returns the LAST job's exit status,
// which would silently swallow an earlier failure — confirmed
// directly).
const batchConcurrencyPreamble = `gg_max="${NIX_BUILD_CORES:-1}"
case "$gg_max" in ''|*[!0-9]*) gg_max=1 ;; esac
[ "$gg_max" -ge 1 ] || gg_max=1
gg_pids=""
gg_fail=0
gg_after() {
  gg_pids="$gg_pids $1"
  set -- $gg_pids
  if [ "$#" -ge "$gg_max" ]; then
    wait "$1" || gg_fail=1
    shift
    gg_pids="$*"
  fi
}
`

// batchConcurrencyDrain waits out every still-running member once the
// launch loop is done, then fails the WHOLE script if anything did —
// deliberately after the loop, not per-member, so a later member's
// failure is never masked by ar running anyway on a subset.
const batchConcurrencyDrain = `for gg_pid in $gg_pids; do
  wait "$gg_pid" || gg_fail=1
done
[ "$gg_fail" -eq 0 ] || exit 1
`

