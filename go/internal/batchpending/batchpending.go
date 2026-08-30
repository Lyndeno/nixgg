// Package batchpending defines the on-disk marker a compile shim
// writes at a caller-visible output path when the TU has been
// DEFERRED into an opt-in batch group (see internal/batch) instead of
// submitted as its own derivation. It answers a different question
// than internal/drvref's stub: drvref means "already submitted — here
// is the drv"; this stub means "not yet even submitted — here is
// where to find what's needed to build it."
//
// Keeping the two formats separate (rather than adding a case to
// drvref) means neither format's existing readers/writers need to
// change, and there is no risk of one format's reader accidentally
// treating the other's stub as its own.
//
// # Why a file and not a symlink
//
// Same reasoning as drvref: nothing exists yet at the deferred
// member's eventual store path (it may never even become a real
// output, if the member is later resolved into an ordinary per-TU
// derivation instead — see internal/shim's resolvePendingMember), so
// there is nothing a symlink could safely point at that would survive
// a `test -e` the way make's own prerequisite checks require.
//
// # Format
//
//	#!nixgg-batch-pending\n
//	<absolute path to the member record file>\n
//
// The referenced file is an internal/batchmember.MemberRecord,
// written by the same compile invocation immediately before this
// stub.
package batchpending

import (
	"os"
	"strings"
)

// Header is the magic first line of every batch-pending stub,
// newline included.
const Header = "#!nixgg-batch-pending\n"

// Body renders a complete stub referencing the member record at
// recordPath.
func Body(recordPath string) string {
	return Header + recordPath + "\n"
}

// maxSize bounds how much of a file we will read while looking for
// the header. Stubs are two short lines; anything larger is a real
// artifact and we should not slurp it.
const maxSize = 4096

// Path returns the member-record path recorded in the stub at
// `path`, or "" if `path` is not a batch-pending stub. Safe to call
// on any file: a real object or archive simply returns "".
func Path(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, maxSize)
	n, _ := f.Read(buf)
	body := string(buf[:n])
	if !strings.HasPrefix(body, Header) {
		return ""
	}
	rest := body[len(Header):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return rest
}

// Is reports whether `path` is a batch-pending stub. Equivalent to
// Path(path) != "" but clearer at call sites that only need the
// boolean.
func Is(path string) bool { return Path(path) != "" }
