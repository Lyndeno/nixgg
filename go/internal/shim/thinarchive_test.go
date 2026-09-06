package shim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/members"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

func thinTestLayout(t *testing.T) paths.Layout {
	t.Helper()
	base := t.TempDir()
	return paths.Layout{
		Thunks:  filepath.Join(base, "thunks"),
		Members: filepath.Join(base, "members"),
	}
}

// TestClassifyInputsExpandsSandboxThinArchive pins the sandbox-mode
// case this whole mechanism exists for: a link argv naming ONE thin
// archive (a drvref stub, same as any other sandbox-mode archive
// input) must pull in every one of that archive's OWN recorded
// members as additional direct inputs — not just the archive's own
// single drv reference, which is all a NORMAL (non-thin) archive ever
// needs.
func TestClassifyInputsExpandsSandboxThinArchive(t *testing.T) {
	l := thinTestLayout(t)

	archiveDrv := "/nix/store/" + strings.Repeat("a", 32) + "-ar-libqemuutil.a.drv"
	memberDrv1 := "/nix/store/" + strings.Repeat("b", 32) + "-tu-foo.c.o.drv"
	memberDrv2 := "/nix/store/" + strings.Repeat("c", 32) + "-tu-bar.c.o.drv"

	key := expr.StoreBasename(archiveDrv)
	if _, err := members.Write(l, key, []members.Record{
		{Kind: "drv", Ref: memberDrv1, Name: "foo.c.o"},
		{Kind: "drv", Ref: memberDrv2, Name: "bar.c.o"},
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "libqemuutil.a")
	if err := os.WriteFile(archivePath, []byte(drvref.Body(archiveDrv)), 0o644); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{archivePath}, "", l, "link", func() error {
		t.Fatal("should not passthrough — the archive resolves to one of our own drvref stubs")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.JSON) != 1 {
		t.Fatalf("got %d primary jsonInputs, want 1 (just the archive itself — members are dependency-only): %+v", len(ci.JSON), ci.JSON)
	}
	if ci.JSON[0].Ref != archiveDrv {
		t.Errorf("primary input Ref = %q, want %q", ci.JSON[0].Ref, archiveDrv)
	}
	if len(ci.ExtraJSON) != 2 {
		t.Fatalf("got %d extraJSON, want 2 (the archive's own members, dependency-only): %+v", len(ci.ExtraJSON), ci.ExtraJSON)
	}
	var gotRefs []string
	for _, ji := range ci.ExtraJSON {
		gotRefs = append(gotRefs, ji.Ref)
	}
	for _, want := range []string{memberDrv1, memberDrv2} {
		found := false
		for _, got := range gotRefs {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("extraJSON missing %q; got refs %v", want, gotRefs)
		}
	}
}

// TestClassifyInputsExpandsNativeThinArchive is the native-mode analog:
// a Thunk-classified archive input (a symlink to an unbuilt .nix
// thunk) must pull in its own recorded members too.
func TestClassifyInputsExpandsNativeThinArchive(t *testing.T) {
	l := thinTestLayout(t)

	thunkDir := t.TempDir()
	thunkPath := filepath.Join(thunkDir, "abcd1234.nix")
	if err := os.WriteFile(thunkPath, []byte("# fake thunk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	key := "abcd1234"
	if _, err := members.Write(l, key, []members.Record{
		{Kind: "store", Ref: "/nix/store/" + strings.Repeat("d", 32) + "-tu-baz.c.o", Name: "baz.c.o"},
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "libfoo.a")
	if err := os.Symlink(thunkPath, archivePath); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{archivePath}, "", l, "link", func() error {
		t.Fatal("should not passthrough")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.Link) != 1 {
		t.Fatalf("got %d primary linkInputs, want 1 (just the archive thunk — members are dependency-only): %+v", len(ci.Link), ci.Link)
	}
	if len(ci.ExtraLink) != 1 {
		t.Fatalf("got %d extraLink, want 1 (the propagated member): %+v", len(ci.ExtraLink), ci.ExtraLink)
	}
	found := false
	for _, li := range ci.ExtraLink {
		if li.Kind == "store" && strings.Contains(li.Ref, "tu-baz.c.o") {
			found = true
		}
	}
	if !found {
		t.Errorf("extraLink missing the propagated member: %+v", ci.ExtraLink)
	}
}

// TestClassifyInputsThinArchiveDedup pins the landmine the design doc
// calls out explicitly: the SAME member reachable through two
// different thin archives on one link line must appear exactly once
// in the result, in BOTH the native and sandbox slice — native mode's
// own serializer has no dedup of its own, so an undeduplicated
// classifyInputs result would make native and sandbox mode's
// rendered scripts diverge for the same logical input set.
func TestClassifyInputsThinArchiveDedup(t *testing.T) {
	l := thinTestLayout(t)

	sharedMember := "/nix/store/" + strings.Repeat("e", 32) + "-tu-shared.c.o.drv"
	archiveDrv1 := "/nix/store/" + strings.Repeat("f", 32) + "-ar-liba.a.drv"
	archiveDrv2 := "/nix/store/" + strings.Repeat("1", 32) + "-ar-libb.a.drv"

	if _, err := members.Write(l, expr.StoreBasename(archiveDrv1), []members.Record{
		{Kind: "drv", Ref: sharedMember, Name: "shared.c.o"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := members.Write(l, expr.StoreBasename(archiveDrv2), []members.Record{
		{Kind: "drv", Ref: sharedMember, Name: "shared.c.o"},
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	pathA := filepath.Join(dir, "liba.a")
	pathB := filepath.Join(dir, "libb.a")
	if err := os.WriteFile(pathA, []byte(drvref.Body(archiveDrv1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte(drvref.Body(archiveDrv2)), 0o644); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{pathA, pathB}, "", l, "link", func() error {
		t.Fatal("should not passthrough")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	// The 2 archives are primary inputs; the shared member is dependency-only.
	if len(ci.JSON) != 2 {
		t.Fatalf("got %d primary jsonInputs, want 2 (the 2 archives): %+v", len(ci.JSON), ci.JSON)
	}
	if len(ci.ExtraJSON) != 1 {
		t.Fatalf("got %d extraJSON, want 1 (the shared member, deduped): %+v", len(ci.ExtraJSON), ci.ExtraJSON)
	}
	if ci.ExtraJSON[0].Ref != sharedMember {
		t.Errorf("extraJSON[0].Ref = %q, want %q", ci.ExtraJSON[0].Ref, sharedMember)
	}
}

// TestClassifyInputsThinArchiveRecursion pins that a thin archive
// consumed by ANOTHER thin archive expands transitively — necessary
// for correctness in general (nothing in this codebase currently
// constructs this shape; archive.go's own parseARArgs only accepts
// .o members today), verified here with a synthetic 3-level chain
// since no real fixture exercises depth > 1.
func TestClassifyInputsThinArchiveRecursion(t *testing.T) {
	l := thinTestLayout(t)

	innerMember := "/nix/store/" + strings.Repeat("2", 32) + "-tu-inner.c.o.drv"
	innerArchiveDrv := "/nix/store/" + strings.Repeat("3", 32) + "-ar-libinner.a.drv"
	outerArchiveDrv := "/nix/store/" + strings.Repeat("4", 32) + "-ar-libouter.a.drv"

	// libinner.a's own sidecar: just the one real member.
	if _, err := members.Write(l, expr.StoreBasename(innerArchiveDrv), []members.Record{
		{Kind: "drv", Ref: innerMember, Name: "inner.c.o"},
	}); err != nil {
		t.Fatal(err)
	}
	// libouter.a's own sidecar: references libinner.a's own drv (as if
	// it were consumed as one of libouter's members).
	if _, err := members.Write(l, expr.StoreBasename(outerArchiveDrv), []members.Record{
		{Kind: "drv", Ref: innerArchiveDrv, Name: "libinner.a"},
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	outerPath := filepath.Join(dir, "libouter.a")
	if err := os.WriteFile(outerPath, []byte(drvref.Body(outerArchiveDrv)), 0o644); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{outerPath}, "", l, "link", func() error {
		t.Fatal("should not passthrough")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	// libouter.a itself is the only primary input; libinner.a's own drv
	// ref (recorded as libouter's member) and libinner.a's OWN member,
	// pulled in transitively, are both dependency-only.
	if len(ci.JSON) != 1 || ci.JSON[0].Ref != outerArchiveDrv {
		t.Fatalf("primary jsonInputs = %+v, want exactly [outerArchiveDrv]", ci.JSON)
	}
	var gotRefs []string
	for _, ji := range ci.ExtraJSON {
		gotRefs = append(gotRefs, ji.Ref)
	}
	for _, want := range []string{innerArchiveDrv, innerMember} {
		found := false
		for _, got := range gotRefs {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("extraJSON missing %q via recursion; got refs %v", want, gotRefs)
		}
	}
}

// TestClassifyInputsNonThinArchiveUnaffected pins that an ordinary
// (non-thin) archive — every archive every existing fixture builds —
// is completely untouched by this mechanism: no sidecar was ever
// written for it, so the members.Read lookup is a guaranteed miss,
// and classifyInputs' output is identical to what it always was.
func TestClassifyInputsNonThinArchiveUnaffected(t *testing.T) {
	l := thinTestLayout(t)

	archiveDrv := "/nix/store/" + strings.Repeat("5", 32) + "-ar-libfoo.a.drv"
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "libfoo.a")
	if err := os.WriteFile(archivePath, []byte(drvref.Body(archiveDrv)), 0o644); err != nil {
		t.Fatal(err)
	}

	ci, err, ok := classifyInputs(&toolchain.Config{}, []string{archivePath}, "", l, "link", func() error {
		t.Fatal("should not passthrough")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(ci.JSON) != 1 {
		t.Fatalf("got %d jsonInputs, want exactly 1 (no member propagation for a non-thin archive): %+v", len(ci.JSON), ci.JSON)
	}
	if ci.JSON[0].Ref != archiveDrv {
		t.Errorf("Ref = %q, want %q", ci.JSON[0].Ref, archiveDrv)
	}
	if len(ci.ExtraJSON) != 0 {
		t.Errorf("got %d extraJSON, want 0 for a non-thin archive: %+v", len(ci.ExtraJSON), ci.ExtraJSON)
	}
}
