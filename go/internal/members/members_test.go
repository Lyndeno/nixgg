package members

import (
	"path/filepath"
	"testing"

	"github.com/tbereknyei/nixgg/internal/paths"
)

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	base := t.TempDir()
	return paths.Layout{Members: filepath.Join(base, "members")}
}

func TestWriteThenRead(t *testing.T) {
	l := testLayout(t)
	recs := []Record{
		{Kind: "drv", Ref: "/nix/store/aaaa-tu-foo.o.drv", Name: "foo.o"},
		{Kind: "store", Ref: "/nix/store/bbbb-tu-bar.o", Name: "bar.o"},
	}
	if _, err := Write(l, "somekey", recs); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Read(l, "somekey")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("Read reported ok=false for a key that was written")
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(got), got)
	}
	if got[0] != recs[0] || got[1] != recs[1] {
		t.Errorf("got %+v, want %+v", got, recs)
	}
}

// TestReadMissingIsNotAnError pins the exact signal classifyInputs
// relies on: no sidecar for this key at all — the normal case for
// every archive that isn't thin — must return ok=false with a nil
// error, not something a caller has to distinguish from a real I/O
// failure.
func TestReadMissingIsNotAnError(t *testing.T) {
	l := testLayout(t)
	recs, ok, err := Read(l, "never-written")
	if err != nil {
		t.Fatalf("expected nil error for a missing sidecar, got %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false for a missing sidecar, got records %+v", recs)
	}
}

func TestWriteOverwritesExisting(t *testing.T) {
	l := testLayout(t)
	if _, err := Write(l, "k", []Record{{Kind: "drv", Ref: "a", Name: "a.o"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(l, "k", []Record{{Kind: "drv", Ref: "b", Name: "b.o"}}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Read(l, "k")
	if err != nil || !ok {
		t.Fatalf("Read failed: ok=%v err=%v", ok, err)
	}
	if len(got) != 1 || got[0].Ref != "b" {
		t.Errorf("got %+v, want a single record with Ref=b (the second write should win)", got)
	}
}
