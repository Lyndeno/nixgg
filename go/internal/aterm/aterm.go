// Package aterm renders the same ATerm-format derivation text `nix
// derivation add` computes internally (Store::writeDerivation ->
// derivation::unparse), from the same expr.JSONDrv struct the sandbox
// shim already builds for the CLI JSON path. Exists so
// internal/sandbox can register a derivation over the raw
// worker-protocol client (internal/rpc) instead of fork+exec'ing the
// CLI to do this exact conversion.
//
// Every rule here was read directly out of the pinned nix-15793
// source (NixOS/nix@8307c48): src/libstore/derivation/aterm.cc's
// unparseDerivation<FullInputs, false> — the non-hash-modulo,
// non-masked path derivation::unparse (called from
// Store::writeDerivation) actually takes. Verified byte-for-byte
// against real .drv files pulled from live nixgg sandbox builds; see
// aterm_test.go.
package aterm

import (
	"sort"
	"strings"

	"github.com/tbereknyei/nixgg/internal/expr"
)

// Unparse renders drv's ATerm text — the exact bytes
// Store::writeDerivation hashes to compute the drv's own store path,
// and the exact bytes internal/rpc.Conn.AddDerivation must upload.
//
// nixgg never emits a dynamic-derivation input (drv.inputs.drvs
// entries are always plain StorePath keys with a flat output-name
// list, never a nested childMap — nixgg's own inputDrvs are always
// already-resolved sibling .drv paths, not further dynamic-derivation
// references), so this only implements the "Derive(...)" traditional
// form, never "DrvWithVersion(...)". A future caller needing the
// dynamic form would need real childMap support, not a silent
// fallback here.
// References returns the full set of store-path references this
// derivation's text-hashed CA output needs registered — the same set
// derivations.cc's own infoForDerivation computes (drv.inputs.srcs +
// the keys of drv.inputs.drvs.map) before calling
// makeFixedOutputPathFromCA. Callers pass this straight to
// rpc.Conn.AddDerivation's own refs parameter; getting this set wrong
// (missing an entry, or including one Nix's own writeDerivation
// wouldn't) computes a WRONG store path silently, not an error — see
// AddDerivation's own docstring on why it doesn't try to verify this
// client-side before uploading.
func References(drv expr.JSONDrv) []string {
	refs := fullSrcPaths(drv.Inputs.Srcs)
	for b := range drv.Inputs.Drvs {
		refs = append(refs, storeDir+"/"+b)
	}
	sort.Strings(refs)
	return refs
}

func Unparse(drv expr.JSONDrv) string {
	var s strings.Builder
	s.Grow(4096)

	s.WriteString("Derive([")
	writeOutputs(&s, drv.Outputs)
	s.WriteString("],[")
	writeInputDrvs(&s, drv.Inputs.Drvs)
	s.WriteString("],")
	writeQuotedStrings(&s, fullSrcPaths(drv.Inputs.Srcs))
	s.WriteByte(',')
	writeQuotedString(&s, drv.System)
	s.WriteByte(',')
	writeEscapedString(&s, drv.Builder)
	s.WriteByte(',')
	writeEscapedStrings(&s, drv.Args)
	s.WriteString(",[")
	writeEnv(&s, drv.Env)
	s.WriteString("])")

	return s.String()
}

// writeOutputs renders the outputs list. Nix iterates drv.outputs in
// its own std::map<std::string, ...> order — lexicographic by output
// name — same as sorting drv.Outputs' keys here.
//
// nixgg's JSONOut always describes a CAFloating output (Method +
// HashAlgo set, no fixed path/hash — the whole point of a dynamic
// derivation is that the output path isn't known until build time):
// path="", hashAlgo=renderPrefix(method)+hashAlgo, hash="". See
// aterm.cc's DerivationOutput::CAFloating branch.
func writeOutputs(s *strings.Builder, outputs map[string]expr.JSONOut) {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)

	for i, name := range names {
		if i > 0 {
			s.WriteByte(',')
		}
		out := outputs[name]
		s.WriteByte('(')
		writeQuotedString(s, name)
		s.WriteByte(',')
		writeQuotedString(s, "") // path: unknown until build
		s.WriteByte(',')
		writeQuotedString(s, methodPrefix(out.Method)+out.HashAlgo)
		s.WriteByte(',')
		writeQuotedString(s, "") // hash: unknown until build
		s.WriteByte(')')
	}
}

// methodPrefix mirrors ContentAddressMethod::renderPrefix(): "r:" for
// a NAR-hashed (recursive) output, "" for a flat one. nixgg's JSONOut
// only ever uses "nar"/"flat" (see expr.go's own JSONOut docstring) —
// git-hashed outputs don't exist in this codebase.
func methodPrefix(method string) string {
	if method == "nar" {
		return "r:"
	}
	return ""
}

// writeInputDrvs renders the input-derivations list. Nix iterates
// drv.inputs.drvs.map in std::map<StorePath, ...> order, which for
// StorePath's own operator<=> is lexicographic on the full printed
// path string (hash part first, but comparing the whole string gives
// the same order since the hash part is a fixed-width prefix) — same
// as sorting the map's string keys directly.
//
// drvs' keys are BASENAMES, not full paths — despite
// JSONDrvInputs.Drvs' own (stale) docstring in expr.go claiming
// "full /nix/store/…-…drv path": expr.go's own toJSON populates this
// map via `refKey := StoreBasename(in.Ref)` (derivation.go's actual
// behavior, confirmed directly against a real sandbox build — the
// docstring is wrong). Same basename-vs-full-path gap fullSrcPaths
// already handles for Srcs; storeDir needs prepending here too.
//
// Every nixgg input-drv entry is a flat output-name list (see
// Unparse's own docstring on why the dynamic/childMap form never
// applies here), so this always takes unparseDerivedPathMapNode's
// childMap-empty branch: just the comma + bracketed output-name list,
// no nested parens.
func writeInputDrvs(s *strings.Builder, drvs map[string]expr.JSONDrvRef) {
	basenames := make([]string, 0, len(drvs))
	for b := range drvs {
		basenames = append(basenames, b)
	}
	sort.Strings(basenames)

	for i, b := range basenames {
		if i > 0 {
			s.WriteByte(',')
		}
		s.WriteByte('(')
		writeQuotedString(s, storeDir+"/"+b)
		s.WriteByte(',')
		writeQuotedStrings(s, sortedCopy(drvs[b].Outputs))
		s.WriteByte(')')
	}
}

// writeEnv renders the environment-variable list. Nix iterates
// drv.env in its own StringPairs (a std::map<std::string,
// std::string>) order — lexicographic by key — same as sorting
// drv.Env's keys.
func writeEnv(s *strings.Builder, env map[string]string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for i, k := range keys {
		if i > 0 {
			s.WriteByte(',')
		}
		s.WriteByte('(')
		writeEscapedString(s, k)
		s.WriteByte(',')
		writeEscapedString(s, env[k])
		s.WriteByte(')')
	}
}

func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// storeDir is Nix's default store directory. nixgg makes no
// provision anywhere for a relocated store (see internal/scan.go's
// own store-path handling, which hardcodes the same assumption) —
// nothing else in this codebase would work under one either.
const storeDir = "/nix/store"

// fullSrcPaths turns JSONDrvInputs.Srcs' basenames (the JSON drv
// format's own convention — see its field docstring in expr.go) back
// into the full "/nix/store/<basename>" paths the raw ATerm format
// requires, sorted the same way Nix's own StorePathSet iterates
// (lexicographic on the full printed path).
func fullSrcPaths(basenames []string) []string {
	out := make([]string, len(basenames))
	for i, b := range basenames {
		out[i] = storeDir + "/" + b
	}
	sort.Strings(out)
	return out
}

// writeQuotedString/writeQuotedStrings emit printUnquotedString's own
// shape: a bare double-quoted string with NO escaping at all — used
// for values aterm.cc already knows can't contain a quote/backslash
// (store paths, output names, method/hash strings). Using the
// escaping writer here would still be byte-correct (none of these
// values ever contain the characters it escapes) but printUnquotedString
// is what Nix's own code path calls, so mirroring it keeps this
// function's contract obviously matched to its source rather than
// "happens to produce the same bytes."
func writeQuotedString(s *strings.Builder, str string) {
	s.WriteByte('"')
	s.WriteString(str)
	s.WriteByte('"')
}

func writeQuotedStrings(s *strings.Builder, strs []string) {
	s.WriteByte('[')
	for i, str := range strs {
		if i > 0 {
			s.WriteByte(',')
		}
		writeQuotedString(s, str)
	}
	s.WriteByte(']')
}

// writeEscapedString/writeEscapedStrings emit printString's own
// shape: a double-quoted string with '"', '\\', '\n', '\r', '\t'
// backslash-escaped — used for values that CAN contain arbitrary
// bytes (the builder path, args, env keys/values — a build script in
// $args or $env routinely contains all five).
func writeEscapedString(s *strings.Builder, str string) {
	s.WriteByte('"')
	for _, r := range str {
		switch r {
		case '"', '\\':
			s.WriteByte('\\')
			s.WriteRune(r)
		case '\n':
			s.WriteString(`\n`)
		case '\r':
			s.WriteString(`\r`)
		case '\t':
			s.WriteString(`\t`)
		default:
			s.WriteRune(r)
		}
	}
	s.WriteByte('"')
}

func writeEscapedStrings(s *strings.Builder, strs []string) {
	s.WriteByte('[')
	for i, str := range strs {
		if i > 0 {
			s.WriteByte(',')
		}
		writeEscapedString(s, str)
	}
	s.WriteByte(']')
}
