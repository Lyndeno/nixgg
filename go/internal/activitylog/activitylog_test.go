package activitylog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// resetState clears the package's own open-once bookkeeping between
// tests — Emit's real callers are one-shim-invocation-per-process, so
// "open the log file once, reuse for the process's lifetime" never
// needs resetting in production, but a test process calls Emit many
// times against different temp paths and must not reuse a stale
// handle from an earlier test.
func resetState(t *testing.T) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if logFile != nil {
		logFile.Close()
	}
	logFile = nil
	logPath = ""
	pathOnce = sync.Once{}
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("unmarshal line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	return lines
}

// TestEmitDisabledByDefault pins the whole feature's off-by-default
// contract: with NIXGG_LOG unset, Emit must not create a file at all
// — a build that never opted in gets zero activity-log overhead and
// zero filesystem side effects.
func TestEmitDisabledByDefault(t *testing.T) {
	resetState(t)
	os.Unsetenv("NIXGG_LOG")
	os.Unsetenv("NIXGG_SANDBOX")

	dir := t.TempDir()
	path := filepath.Join(dir, "activity.ndjson")
	Emit("compile", "thunk", Fields{"source": "foo.c"})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Emit with NIXGG_LOG unset created a file: %v", err)
	}
}

// TestEmitWritesEnvelopeAndFields pins the actual line shape: the
// common envelope (event, kind, ts, cwd) plus every field the caller
// passed, merged into one JSON object — matching the old bash
// version's own nixgg::emit schema so existing jq-based tooling still
// works unmodified.
func TestEmitWritesEnvelopeAndFields(t *testing.T) {
	resetState(t)
	os.Unsetenv("NIXGG_SANDBOX")
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.ndjson")
	t.Setenv("NIXGG_LOG", path)

	Emit("compile", "thunk", Fields{"source": "foo.c", "output": "foo.o"})

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	line := lines[0]
	if line["event"] != "compile" {
		t.Errorf("event = %v, want \"compile\"", line["event"])
	}
	if line["kind"] != "thunk" {
		t.Errorf("kind = %v, want \"thunk\"", line["kind"])
	}
	if line["source"] != "foo.c" || line["output"] != "foo.o" {
		t.Errorf("caller fields missing or wrong: %v", line)
	}
	if _, ok := line["ts"]; !ok {
		t.Error("missing ts field")
	}
	if _, ok := line["cwd"]; !ok {
		t.Error("missing cwd field")
	}
}

// TestEmitAppendsMultipleLines pins that successive Emit calls append
// (ndjson, not one-JSON-object-per-file) — a real build calls Emit
// once per shim invocation, many times per process tree.
func TestEmitAppendsMultipleLines(t *testing.T) {
	resetState(t)
	os.Unsetenv("NIXGG_SANDBOX")
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.ndjson")
	t.Setenv("NIXGG_LOG", path)

	Emit("compile", "thunk", Fields{"source": "a.c"})
	Emit("compile", "thunk", Fields{"source": "b.c"})
	Emit("link", "drv", Fields{"output": "app"})

	lines := readLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if lines[0]["source"] != "a.c" || lines[1]["source"] != "b.c" || lines[2]["event"] != "link" {
		t.Errorf("lines out of order or wrong content: %v", lines)
	}
}

// TestEmitNoOpUnderSandbox pins the core architectural constraint
// this package rests on: a builder-rpc-v0 sandbox's own filesystem
// writes never reach the host (confirmed directly against a real
// sandbox before writing this package), so Emit must never even try
// to write when NIXGG_SANDBOX=1 — not "try and fail silently", but
// skip the attempt entirely (no wasted work opening a file that, in
// the real sandbox case, would silently land in a mount nobody will
// ever read).
func TestEmitNoOpUnderSandbox(t *testing.T) {
	resetState(t)
	t.Setenv("NIXGG_SANDBOX", "1")
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.ndjson")
	t.Setenv("NIXGG_LOG", path)

	Emit("compile", "thunk", Fields{"source": "foo.c"})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Emit wrote a file under NIXGG_SANDBOX=1: %v", err)
	}
}

// TestEmitBadPathDoesNotPanic pins the best-effort contract: a
// NIXGG_LOG pointing at an unwritable/nonexistent-parent path must
// never panic or otherwise disrupt the caller — losing an activity-
// log line must never fail a real build.
func TestEmitBadPathDoesNotPanic(t *testing.T) {
	resetState(t)
	os.Unsetenv("NIXGG_SANDBOX")
	t.Setenv("NIXGG_LOG", "/nonexistent-dir-nixgg-test/activity.ndjson")

	Emit("compile", "thunk", Fields{"source": "foo.c"})
}
