package batch

import "testing"

// TestClassifyRedisDeps pins the motivating real-world case from
// ARCHITECTURE.md's batching discussion: redis's deps/{hiredis,
// linenoise,lua,jemalloc,hdr_histogram,fpconv,fast_float}/ trees are
// vendored and rarely edited, unlike redis's own src/ — a project
// author would list deps/ as one batch and leave src/ unbatched.
func TestClassifyRedisDeps(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "vendor", Patterns: []string{"deps/**/*.c"}},
	}}

	tests := []struct {
		path      string
		wantGroup string
		wantOK    bool
	}{
		{"/build/source/deps/hiredis/hiredis.c", "vendor", true},
		{"/build/source/deps/jemalloc/src/jemalloc.c", "vendor", true},
		{"/build/source/deps/lua/src/lapi.c", "vendor", true},
		{"/build/source/src/t_string.c", "", false},
		{"/build/source/src/networking.c", "", false},
	}
	for _, tc := range tests {
		group, ok := cfg.Classify(tc.path)
		if ok != tc.wantOK || group != tc.wantGroup {
			t.Errorf("Classify(%q) = (%q, %v), want (%q, %v)",
				tc.path, group, ok, tc.wantGroup, tc.wantOK)
		}
	}
}

// TestClassifyUnanchoredAcrossVaryingRoots is a regression test for a
// real bug found while verifying this package end to end against a
// live redis build: compile.go originally passed Classify a path
// relative to internal/scan's ProjectRoot, which is NOT a fixed,
// build-wide root — it's recomputed per compile call as the common
// ancestor of that call's own cwd + -I dirs. When `make` recurses
// into deps/hiredis/ and compiles from inside that directory with no
// outside -I references, ProjectRoot collapses to deps/hiredis
// itself, so the "relative path" for sds.c was just "sds.c", not
// "deps/hiredis/sds.c" — deps/**/*.c could never match it. Out of 156
// real TUs compiled in that build, exactly 1 matched.
//
// Classify's fix: take the TU's ABSOLUTE path and search unanchored
// (try every start offset), so it doesn't matter what any particular
// call's cwd happened to be — this test pins that a source under
// deps/hiredis/ classifies the same whether it's reached via a
// project-root-anchored absolute path or one where cwd collapsed
// everything above deps/hiredis/ away.
// TestClassifyUnanchoredAcrossVaryingRoots is a regression test for a
// real bug found while verifying this package end to end against a
// live redis build: compile.go originally passed Classify a path
// relative to internal/scan's ProjectRoot, which is NOT a fixed,
// build-wide root — it's recomputed per compile call as the common
// ancestor of that call's own cwd + -I dirs. When `make` recurses
// into deps/hiredis/ and compiles from inside that directory with no
// outside -I references, ProjectRoot collapses to deps/hiredis
// itself, so what reached Classify was effectively just "sds.c" —
// deps/**/*.c can never match that. Out of 156 real TUs compiled in
// that build, exactly 1 matched.
//
// The fix has two parts: compile.go now passes the TU's ABSOLUTE
// path (stable, no dependency on any per-call scan state), and
// Classify searches it unanchored (tries every start offset) instead
// of assuming position 0 is the project root. This test pins the
// second half directly: a short, collapsed-root-style path with no
// "deps/" segment at all correctly does NOT match (there's nothing
// left to search), proving the fix has to be "pass the full path",
// not "make Classify smarter about a truncated one" — Classify
// cannot recover information the caller already threw away.
func TestClassifyUnanchoredAcrossVaryingRoots(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "vendor", Patterns: []string{"deps/**/*.c"}},
	}}

	full := "/build/source/deps/hiredis/sds.c"
	if g, ok := cfg.Classify(full); !ok || g != "vendor" {
		t.Fatalf("Classify(%q) = (%q, %v), want (vendor, true)", full, g, ok)
	}

	truncated := "sds.c" // what a collapsed ProjectRoot-relative path looked like in practice
	if _, ok := cfg.Classify(truncated); ok {
		t.Fatalf("Classify(%q) unexpectedly matched — a path with no deps/ segment at all must not match deps/**/*.c", truncated)
	}
}

// TestClassifyFirstMatchWins pins the declaration-order contract: an
// author can list a narrow exception before a broad catch-all, same
// as switch/case fallthrough — reordering the two Groups below would
// change hot.c's classification.
func TestClassifyFirstMatchWins(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "hot", Patterns: []string{"vendor/hot/*.c"}},
		{Name: "vendor", Patterns: []string{"vendor/**/*.c"}},
	}}

	if g, ok := cfg.Classify("/build/source/vendor/hot/hot.c"); !ok || g != "hot" {
		t.Errorf("Classify(vendor/hot/hot.c) = (%q, %v), want (hot, true)", g, ok)
	}
	if g, ok := cfg.Classify("/build/source/vendor/cold/cold.c"); !ok || g != "vendor" {
		t.Errorf("Classify(vendor/cold/cold.c) = (%q, %v), want (vendor, true)", g, ok)
	}
}

// TestClassifyNoGroups pins the empty-config case: Classify never
// panics on a zero Config, and always reports unbatched — matches
// FromJSON's own "absence is not an error" contract.
func TestClassifyNoGroups(t *testing.T) {
	var cfg Config
	if g, ok := cfg.Classify("/build/source/src/anything.c"); ok || g != "" {
		t.Errorf("Classify on zero Config = (%q, %v), want (\"\", false)", g, ok)
	}
}

// TestClassifySingleStar pins that a bare "*" segment (no "**")
// behaves exactly like configureSrcFilterPresets.nix's own
// includePatterns convention: one path segment, not arbitrary depth.
func TestClassifySingleStar(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "one-level", Patterns: []string{"deps/*/*.c"}},
	}}
	if _, ok := cfg.Classify("/build/source/deps/hiredis/hiredis.c"); !ok {
		t.Error("deps/*/*.c should match deps/hiredis/hiredis.c")
	}
	if _, ok := cfg.Classify("/build/source/deps/hiredis/sub/deep.c"); ok {
		t.Error("deps/*/*.c should NOT match deps/hiredis/sub/deep.c (single-level only)")
	}
}

// TestClassifyDeepStar pins "**"'s zero-or-more-segments behavior at
// both ends: it must match a direct child (zero extra segments) as
// well as an arbitrarily nested one, and must not match a path that
// never contains "deps" at all.
func TestClassifyDeepStar(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "vendor", Patterns: []string{"deps/**/*.c"}},
	}}
	if _, ok := cfg.Classify("/build/source/deps/hiredis.c"); !ok {
		t.Error("deps/**/*.c should match deps/hiredis.c (zero extra segments)")
	}
	if _, ok := cfg.Classify("/build/source/deps/a/b/c/deep.c"); !ok {
		t.Error("deps/**/*.c should match deps/a/b/c/deep.c (arbitrary depth)")
	}
	if _, ok := cfg.Classify("/build/source/other/hiredis.c"); ok {
		t.Error("deps/**/*.c should not match a path with no deps/ segment at all")
	}
}

// TestClassifyUnanchoredDoesNotFalseMatchSimilarNames pins the cost of
// unanchored matching: a directory that merely CONTAINS "deps" as a
// substring of a longer segment name must not match "deps/**/*.c" —
// only an exact "deps" path segment should.
func TestClassifyUnanchoredDoesNotFalseMatchSimilarNames(t *testing.T) {
	cfg := Config{Groups: []Group{
		{Name: "vendor", Patterns: []string{"deps/**/*.c"}},
	}}
	if _, ok := cfg.Classify("/build/source/mydeps/foo.c"); ok {
		t.Error(`deps/**/*.c should not match a "mydeps" segment`)
	}
	if _, ok := cfg.Classify("/build/source/depsx/foo.c"); ok {
		t.Error(`deps/**/*.c should not match a "depsx" segment`)
	}
}

// TestFromJSON pins the wire format $NIXGG_BATCH_GROUPS carries —
// same JSON-array-of-objects shape a Nix-side eval-time computation
// would produce, mirroring $NIXGG_KNOWN_STORE_PATHS's own convention.
func TestFromJSON(t *testing.T) {
	cfg := FromJSON(`[{"name":"vendor","patterns":["deps/**/*.c"]},{"name":"proto","patterns":["*.pb.cc"]}]`)
	if len(cfg.Groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(cfg.Groups))
	}
	if cfg.Groups[0].Name != "vendor" || cfg.Groups[1].Name != "proto" {
		t.Errorf("groups in wrong order or wrong names: %+v", cfg.Groups)
	}
	if g, ok := cfg.Classify("/build/source/deps/hiredis/hiredis.c"); !ok || g != "vendor" {
		t.Errorf("Classify after FromJSON = (%q, %v), want (vendor, true)", g, ok)
	}
}

// TestFromJSONEmptyOrInvalid pins the "absence is not an error"
// contract knownStorePathsFromEnv already established for the
// sibling env var.
func TestFromJSONEmptyOrInvalid(t *testing.T) {
	for _, s := range []string{"", "not json", "{}", "[1,2,3]"} {
		cfg := FromJSON(s)
		if g, ok := cfg.Classify("/build/source/anything.c"); ok || g != "" {
			t.Errorf("FromJSON(%q).Classify(...) = (%q, %v), want (\"\", false)", s, g, ok)
		}
	}
}
