// Package activitylog emits one ndjson line per shim decision to
// NIXGG_LOG, when set — the Go rewrite's port of the old bash
// version's own nixgg::emit (see shims/_lib.sh, pre-Go-rewrite). Off
// by default: a build with NIXGG_LOG unset pays zero cost (Emit's
// first check is the env lookup, no JSON marshaling happens).
//
// Every event line has the same envelope — event, kind, ts (unix
// seconds, float, matching the old bash version's `date +%s.%N`),
// cwd — plus whatever event-specific fields the caller passes in
// Fields. This mirrors the old version's own jq-built schema exactly,
// so any existing tooling built against `.event`/`.kind` (a log
// analysis script, a build-time dashboard) doesn't need to change
// shape to consume the Go rewrite's log lines.
package activitylog

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/tbereknyei/nixgg/internal/sandbox"
)

var (
	mu       sync.Mutex
	pathOnce sync.Once
	logPath  string
	logFile  *os.File
)

// Fields is the event-specific payload Emit merges into the common
// envelope. A plain map, not a typed struct per event: the old bash
// version's own events differ in shape per (event, kind) pair (a
// "thunk" kind carries `thunk`+`inputs`; a "cache_hit" kind carries
// `built`+`thunk_id`; "passthrough" carries `argv`, sometimes
// `reason`+`input`) — a single Go struct covering every field any
// event ever needs would carry mostly-empty fields on every line,
// exactly the noise a log line format shouldn't have.
type Fields map[string]any

// Emit appends one ndjson line to NIXGG_LOG, if set. event is the
// shim family ("compile", "link", "ar", "batch"); kind is the
// decision this invocation made ("thunk", "drv", "passthrough",
// "cache_hit", "argv_cache_hit", ...) — same two-field taxonomy the
// old bash version used, so `jq 'select(.event=="compile" and
// .kind=="cache_hit")'`-style filtering still works unchanged.
//
// A no-op under sandbox.Enabled(): a builder-rpc-v0 sandbox's own
// filesystem writes never reach the host (confirmed directly — even
// a plain sandboxed derivation's write to an arbitrary /tmp/... path
// lands in the sandbox's own private mount, invisible once the build
// finishes), so NIXGG_LOG can only ever produce a real, readable file
// in native mode — the same mode the old bash version's own
// nixgg::emit ran in exclusively (sandbox mode didn't exist yet when
// it was written). Every call site stays symmetric across both modes
// rather than special-casing sandbox at each one; if a future sandbox
// transport exists (e.g. relaying events through the outer builder's
// own stdout, which Nix DOES capture), this is the one place to add
// it.
//
// Failure to open/write the log file is deliberately silent (best-
// effort — matches the old bash version's own `[[ -z "$_NIXGG_LOG"
// ]] && return 0` with no error path at all): losing an activity-log
// line must never fail a real build. A build with a typo'd NIXGG_LOG
// path should not behave differently from one with it unset.
func Emit(event, kind string, fields Fields) {
	if sandbox.Enabled() {
		return
	}
	path := os.Getenv("NIXGG_LOG")
	if path == "" {
		return
	}

	line := Fields{
		"event": event,
		"kind":  kind,
		"ts":    float64(time.Now().UnixNano()) / 1e9,
	}
	if cwd, err := os.Getwd(); err == nil {
		line["cwd"] = cwd
	}
	for k, v := range fields {
		line[k] = v
	}

	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	b = append(b, '\n')

	mu.Lock()
	defer mu.Unlock()
	f := openLogFile(path)
	if f == nil {
		return
	}
	// One Write call for the whole line: O_APPEND makes a single
	// write(2) atomic against other processes appending to the same
	// file concurrently (make -jN runs many shim invocations, each
	// its own process) — multiple smaller writes could interleave.
	_, _ = f.Write(b)
}

// openLogFile opens NIXGG_LOG once per process (shim invocations are
// short-lived, one process per compile/link/archive call, so "once
// per process" is "once per Emit call site actually reached" in
// practice) and reuses the handle for the process's own lifetime.
// pathOnce/logFile/logPath together guard against re-opening on every
// single Emit call, and against a change in NIXGG_LOG mid-process
// (impossible in practice — env is fixed per shim invocation — but
// guarding the mismatch is cheap and avoids a subtle bug if that
// assumption is ever wrong).
func openLogFile(path string) *os.File {
	pathOnce.Do(func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		logFile = f
		logPath = path
	})
	if logPath != path {
		return nil
	}
	return logFile
}
