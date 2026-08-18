package stage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// SourcesShared must reproduce the tree's SHAPE exactly — relative
// paths are what make `#include "../foo.h"` and the caller's -I flags
// resolve — while replacing contents with symlinks into shared store
// objects.
func TestSourcesShared(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.c", "sub/dup.h", "other.h"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := paths.Layout{Srcs: filepath.Join(root, "srcs")}
	entries := []Entry{
		{Abs: filepath.Join(src, "a.c"), Rel: "a.c"},
		{Abs: filepath.Join(src, "sub/dup.h"), Rel: "sub/dup.h"},
		{Abs: filepath.Join(src, "other.h"), Rel: "other.h"},
	}

	// Stand-in for the store: identical content collapses to one object,
	// which is the property the real implementation gets from
	// content-addressing.
	calls := 0
	store := func(abs string) (string, error) {
		calls++
		return "/nix/store/deadbeef-" + filepath.Base(abs), nil
	}

	res, err := SourcesShared(l, "tu-1", entries, store)
	if err != nil {
		t.Fatalf("SourcesShared: %v", err)
	}
	if calls != 3 {
		t.Errorf("store called %d times, want 3", calls)
	}
	for _, e := range entries {
		p := filepath.Join(res, e.Rel)
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("%s missing: %v", e.Rel, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is not a symlink — the whole point is to stop copying", e.Rel)
		}
		tgt, _ := os.Readlink(p)
		if !strings.HasPrefix(tgt, "/nix/store/") {
			t.Errorf("%s -> %q, want a store path", e.Rel, tgt)
		}
	}
}
