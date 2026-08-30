package batchpending

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBodyRoundTrip pins that what Body writes is what Path reads
// back — the writer (deferCompileToBatch) and reader (classifyInputs'
// fallback prologue, tryBatchArchive, resolveLibFlag) must agree.
func TestBodyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, rec := range []string{
		filepath.Join(dir, "sha1abcdef.json"),
		filepath.Join(dir, "another-record.json"),
	} {
		f := filepath.Join(dir, filepath.Base(rec)+".stub")
		if err := os.WriteFile(f, []byte(Body(rec)), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := Path(f); got != rec {
			t.Errorf("Path(Body(%q)) = %q, want the original", rec, got)
		}
		if !Is(f) {
			t.Errorf("Is() false for a stub we just wrote: %q", f)
		}
	}
}

// TestPathRejectsNonStubs pins that Path is safe to call on anything,
// including a real drvref stub — the two formats must never be
// mistaken for each other.
func TestPathRejectsNonStubs(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"ar archive", "!<arch>\n"},
		{"elf-ish", "\x7fELF\x02\x01\x01\x00"},
		{"plain text", "hello\n"},
		{"header without newline", "#!nixgg-batch-pending"},
		{"drvref stub, not batch-pending", "#!nixgg-drvref\n/nix/store/x.drv\n"},
		{"magic not at the start", "junk\n#!nixgg-batch-pending\n/x.json\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_"))
			if err := os.WriteFile(f, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := Path(f); got != "" {
				t.Errorf("Path(%q content) = %q, want \"\"", tc.name, got)
			}
			if Is(f) {
				t.Errorf("Is() true for non-stub %q", tc.name)
			}
		})
	}
}

// TestPathOnMissingFile pins that a nonexistent path is "" and not a
// panic.
func TestPathOnMissingFile(t *testing.T) {
	if got := Path(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Errorf("Path(missing) = %q, want \"\"", got)
	}
}

// TestHeaderShape pins the two properties other packages rely on: the
// magic ends in a newline, and it opens with `#!` so a stub mistaken
// for an executable produces a legible error.
func TestHeaderShape(t *testing.T) {
	if !strings.HasSuffix(Header, "\n") {
		t.Errorf("Header %q must end in a newline", Header)
	}
	if !strings.HasPrefix(Header, "#!") {
		t.Errorf("Header %q must start with #! so a mis-exec is legible", Header)
	}
	if strings.Count(Header, "\n") != 1 {
		t.Errorf("Header %q must be exactly one line", Header)
	}
}
