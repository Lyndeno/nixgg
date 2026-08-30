// Package batch classifies a compile's source path against an opt-in
// list of glob patterns naming directories the caller has decided are
// stable enough to bundle into one multi-output derivation instead of
// nixgg's default one-derivation-per-TU.
//
// This package only answers "does this source belong to a batch, and
// which one" — matching the shape of
// nix/configureSrcFilterPresets.nix's includePatterns (`find
// -path`-style globs, curated per-project or per-subtree, not
// inferred). The actual multi-output batch derivation lives
// elsewhere: internal/shim's deferCompileToBatch records a pending
// member (internal/batchmember/internal/batchpending) instead of
// submitting a per-TU derivation, and internal/shim's
// tryBatchArchive/submitCombinedArchive combine every pending member
// belonging to one archive's same group into ONE derivation (N
// compiles + 1 archive) when that archive's own `ar` invocation sees
// them — see go/internal/expr/batcharchive.go's package docstring for
// the derivation shape itself. See ARCHITECTURE.md's "What we don't
// (yet) do" for the reasoning that motivated scoping batching this
// way (batching an actively-edited directory trades saved Nix
// per-derivation overhead for wasted real compiler time on unchanged
// siblings; only a directory that's genuinely stable relative to how
// often the project rebuilds is a good candidate, and that judgment
// call belongs to the project author, not a heuristic).
package batch

import (
	"path/filepath"
	"strings"
)

// Group is one opt-in batch: a name (used to derive the eventual
// multi-output derivation's own name) and the glob patterns matched,
// UNANCHORED, against the TU's absolute source path — see Classify's
// own docstring for why unanchored, not project-root-relative. Each
// pattern is filepath.Match syntax per path segment (segments split
// on "/"), PLUS a "**" segment meaning "zero or more path segments",
// so "deps/**/*.c" reaches deps/hiredis/foo.c AND
// deps/hiredis/sub/foo.c. Patterns use forward slashes regardless of
// host OS.
type Group struct {
	Name     string
	Patterns []string
}

// Config is the parsed opt-in batch manifest — the set of Groups a
// project author declared, in declaration order. Order matters for
// Classify: the first matching Group wins, so an author can list a
// narrow exception before a broad catch-all pattern, same convention
// as switch/case fallthrough.
type Config struct {
	Groups []Group
}

// Classify reports which Group (if any) a TU belongs to, given its
// absolute source path. ok=false means the TU isn't batched — the
// caller should fall back to nixgg's existing one-derivation-per-TU
// path.
//
// Matching is UNANCHORED: a pattern like "deps/**/*.c" matches if
// "deps/..." appears anywhere in the path's segment sequence, not
// only from some computed root. This is deliberate, not a
// convenience shortcut: the natural anchor — the source's path
// relative to "the project root" — has no single stable value across
// a build. internal/scan computes a ProjectRoot per compile call (the
// common ancestor of that call's own cwd + -I dirs), so the same
// logical file resolves to a DIFFERENT relative path depending on
// which directory `make` happened to be in when it invoked the shim
// for that particular TU — confirmed directly against a real redis
// build, where compiling from inside deps/hiredis/ (no outside -I
// references) collapsed ProjectRoot down to deps/hiredis itself,
// making the "relative path" just "sds.c", not "deps/hiredis/sds.c".
// An unanchored, absolute-path search has no such root to destabilize.
func (c Config) Classify(absPath string) (group string, ok bool) {
	segs := strings.Split(filepath.ToSlash(absPath), "/")
	for _, g := range c.Groups {
		for _, pat := range g.Patterns {
			patSegs := strings.Split(pat, "/")
			for start := range segs {
				if matchSegs(patSegs, segs[start:]) {
					return g.Name, true
				}
			}
		}
	}
	return "", false
}

// matchSegs matches pat (glob segments, "**" meaning zero-or-more)
// against name (path segments) anchored at name's own start — the
// "unanchored" part of Classify's search comes from trying every
// start offset into the full path at the call site, not from this
// function itself.
func matchSegs(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		if matchSegs(pat[1:], name) {
			return true
		}
		if len(name) == 0 {
			return false
		}
		return matchSegs(pat, name[1:])
	}
	if len(name) == 0 {
		return false
	}
	ok, err := filepath.Match(pat[0], name[0])
	if err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], name[1:])
}
