package assemble

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tbereknyei/nixgg/internal/drvref"
)

func writeStub(t *testing.T, path, drvPath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(drvref.Body(drvPath)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWalkFindsStubsAtCorrectRelPaths(t *testing.T) {
	root := t.TempDir()
	writeStub(t, filepath.Join(root, "src", "hello.o"), "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-tu-hello.o.drv")
	writeStub(t, filepath.Join(root, "bin", "hello"), "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bin-hello.drv")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("not a stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	stubs, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(stubs) != 2 {
		t.Fatalf("expected 2 stubs, got %d: %+v", len(stubs), stubs)
	}
	got := map[string]string{}
	for _, s := range stubs {
		got[s.RelPath] = s.DrvPath
	}
	if got["src/hello.o"] != "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-tu-hello.o.drv" {
		t.Errorf("src/hello.o -> %q", got["src/hello.o"])
	}
	if got["bin/hello"] != "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bin-hello.drv" {
		t.Errorf("bin/hello -> %q", got["bin/hello"])
	}
}

func TestWalkSkipsNixSocket(t *testing.T) {
	root := t.TempDir()
	writeStub(t, filepath.Join(root, "bin", "hello"), "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bin-hello.drv")
	if err := os.WriteFile(filepath.Join(root, ".nix-socket"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	stubs, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(stubs) != 1 || stubs[0].RelPath != "bin/hello" {
		t.Fatalf("expected only bin/hello, got %+v", stubs)
	}
}

func TestWalkDeterministicOrder(t *testing.T) {
	root := t.TempDir()
	writeStub(t, filepath.Join(root, "z.o"), "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-z.drv")
	writeStub(t, filepath.Join(root, "a.o"), "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-a.drv")

	stubs, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(stubs) != 2 || stubs[0].RelPath != "a.o" || stubs[1].RelPath != "z.o" {
		t.Fatalf("expected lexical order [a.o z.o], got %+v", stubs)
	}
}

func TestStageForScanExcludesNixSocket(t *testing.T) {
	root := t.TempDir()
	writeStub(t, filepath.Join(root, "bin", "hello"), "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bin-hello.drv")
	if err := os.WriteFile(filepath.Join(root, ".nix-socket"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}

	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staged, ".nix-socket")); !os.IsNotExist(err) {
		t.Errorf(".nix-socket should not be staged, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "bin", "hello")); err != nil {
		t.Errorf("bin/hello should be staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "README.md")); err != nil {
		t.Errorf("README.md should be staged: %v", err)
	}
}

func TestStageForScanPreservesSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "libfoo.so.1.2.3"), []byte("elf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libfoo.so.1.2.3", filepath.Join(root, "libfoo.so")); err != nil {
		t.Fatal(err)
	}

	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(staged, "libfoo.so"))
	if err != nil {
		t.Fatalf("libfoo.so should remain a symlink: %v", err)
	}
	if target != "libfoo.so.1.2.3" {
		t.Errorf("libfoo.so -> %q, want libfoo.so.1.2.3", target)
	}
}

func TestStageForScanIsSelfExcluding(t *testing.T) {
	// The regression this pins: StageForScan used to accept a
	// caller-supplied dest, which — inside a real builder-rpc-v0
	// sandbox where $TMPDIR resolves under root — could itself land
	// under root. Copying root's entries into a destination that is
	// ALSO one of root's own entries recursed into itself at every
	// level until the kernel refused with "file name too long"
	// (confirmed directly building hello-dyndrv). StageForScan now
	// always stages at a FIXED, excluded name directly under root,
	// which by construction cannot be copied into itself: this test
	// pins that the staged tree contains no infinite ".gg-stage"
	// nesting no matter how many times StageForScan runs against the
	// same root.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staged, ".gg-stage")); !os.IsNotExist(err) {
		t.Errorf("staged tree should not contain a nested .gg-stage, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "a.txt")); err != nil {
		t.Errorf("a.txt should be staged: %v", err)
	}
}

// .nixgg is nixgg's own scratch dir. Capturing it into the assembled
// tree was the root of the kernel build's scaling failures: under shared
// staging it holds one symlink farm per TU (19,167 farms x ~225 symlinks
// on a kernel), so StageForScan copied millions of symlinks and
// `nix store add --scan` recorded a reference to every file object they
// pointed at — 34,370 of them in the final drv's closure, which is what
// exceeded fs.mount-max and nearly filled the disk.
//
// Neither Walk nor StageForScan may look inside it.
func TestNixggScratchDirIsExcluded(t *testing.T) {
	root := t.TempDir()
	// A staged farm at the depth it actually occurs: nixgg puts its
	// scratch dir at the project root, which on a kernel is
	// <root>/linux-6.18.41/build/.nixgg — NOT the top level. The first
	// version of this test used the top level and passed against code
	// that only filtered StageForScan's own ReadDir loop.
	farm := filepath.Join(root, "linux-6.18.41", "build", ".nixgg", "srcs", "tu0")
	if err := os.MkdirAll(farm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/aaa-hdr.h", filepath.Join(farm, "hdr.h")); err != nil {
		t.Fatal(err)
	}
	// A thunk, and a stub that must NOT be mistaken for build output.
	if err := os.WriteFile(filepath.Join(root, "linux-6.18.41", "build", ".nixgg", "scan-cache"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Real output alongside it.
	if err := os.WriteFile(filepath.Join(root, "linux-6.18.41", "build", "real.txt"), []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}

	staged, err := StageForScan(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(staged, "linux-6.18.41", "build", ".nixgg")); !os.IsNotExist(err) {
		t.Error(".nixgg was copied into the staged tree; its closure would pull in " +
			"every staged source object")
	}
	if _, err := os.Lstat(filepath.Join(staged, "linux-6.18.41", "build", "real.txt")); err != nil {
		t.Errorf("real build output was not staged: %v", err)
	}
}
