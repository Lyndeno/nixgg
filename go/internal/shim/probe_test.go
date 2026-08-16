package shim

import (
	"os"
	"path/filepath"
	"testing"
)

// Build systems ask the compiler questions by compiling /dev/null:
//
//	$(CC) -Werror $(FLAGS) <option> -c -x c /dev/null -o "$$TMP"
//
// kbuild's cc-option and as-option (scripts/Makefile.compiler) key
// purely on the exit status, so ANY error the shim raises is silently
// reinterpreted as "the compiler does not support this option" and the
// build carries on with a degraded flag set. The kernel dropped
// -DCC_USING_FENTRY that way and died ~45 minutes later at a #error in
// asm/ftrace.h, nowhere near the cause.
//
// So a non-regular source must never be modelled. isRegularFile is the
// guard; pin its contract on the shapes that actually reach it.
func TestIsRegularFile(t *testing.T) {
	dir := t.TempDir()

	real := filepath.Join(dir, "real.c")
	if err := os.WriteFile(real, []byte("int x;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.c")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling.c")
	if err := os.Symlink(filepath.Join(dir, "nope.c"), dangling); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"ordinary source", real, true},
		// Shared staging symlinks staged sources into the store, so a
		// symlink to a regular file has to keep counting as one.
		{"symlink to a regular file", link, true},
		// The case that broke the kernel: a character device has no NAR
		// representation, so `nix store add` fails on it.
		{"/dev/null", "/dev/null", false},
		{"directory", dir, false},
		{"missing file", filepath.Join(dir, "absent.c"), false},
		{"dangling symlink", dangling, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRegularFile(tc.path); got != tc.want {
				t.Errorf("isRegularFile(%s) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
