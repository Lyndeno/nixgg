// Package members defines the on-disk sidecar an archive shim writes
// when it builds a THIN archive (`ar --thin`/`T`) — the one archive
// shape where the caller's own `.a` file doesn't embed its members'
// bytes, so a later consumer (a link, or another archive) can't rely
// on the archive's own drv output alone; it also needs each member
// declared as one of ITS OWN inputs, or Nix won't mount that member's
// store path into the consumer's own sandbox.
//
// # Why this is safe under nixgg's model
//
// A thin archive stores each member's file PATH, not its bytes. Every
// member nixgg's archive shim ever hands to `ar` — thin or not — is
// already resolved to a permanent, immutable /nix/store/... path (or
// a CA output-placeholder Nix substitutes for one before the build
// script runs) before `ar` is invoked at all; see
// go/internal/expr/derivation.go's KindArchive case. A thin archive
// built from those same paths stores references that remain valid
// forever, from any sandbox, for as long as the referenced store path
// exists — which is exactly what Nix's own derivation-input
// declaration model already guarantees for the lifetime of any build
// that declares it as an input. Verified directly (see this
// package's own commit message / the design doc this shipped with):
// an absolute-path thin archive linked successfully from a completely
// unrelated directory after the archive-creation sandbox was deleted;
// only a RELATIVE member path is fragile (GNU ar resolves it against
// the archive file's own location, not cwd) — and nixgg never
// produces one.
//
// # Why not extend drvref instead
//
// go/internal/drvref's wire format is deliberately bounded (~4096
// bytes) and shared by three unrelated readers that all depend on it
// staying a tiny, single-purpose marker. A thin archive can have
// hundreds of members — QEMU's libqemuutil.a has ~280 — which would
// blow well past that bound. This package is a separate, unbounded-
// size sidecar instead, the same way go/internal/batchpending and
// go/internal/batchmember are two separate formats for two separate
// metadata shapes rather than one overloaded format.
//
// # Filename
//
// Written to .nixgg/members/<key>.json, where <key> is the SAME id
// the archive's own thunk (native mode, thunk.Compute) or drv path
// (sandbox mode, expr.StoreBasename) already produces — so a later
// consumer that resolves this archive via classify.Target can compute
// the identical lookup key with no extra state to thread through.
package members

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/paths"
)

// Record is one archive member, in the same {Kind, Ref, Name}
// vocabulary as expr.Input (native) / expr.JSONDrvInput (sandbox) —
// deliberately identical so a caller can convert straight from
// whichever of those slices classifyInputs already built, with no
// translation layer.
type Record struct {
	Kind string
	Ref  string
	Name string
}

func path(l paths.Layout, key string) string {
	return filepath.Join(l.Members, key+".json")
}

// Write persists recs as the member list for the thin archive keyed
// by key, using temp-file-then-rename (same idiom as thunk.Write and
// batchmember.Write) so a half-written file is never observed by a
// concurrent reader.
func Write(l paths.Layout, key string, recs []Record) (string, error) {
	if err := os.MkdirAll(l.Members, 0o755); err != nil {
		return "", err
	}
	body, err := json.Marshal(recs)
	if err != nil {
		return "", fmt.Errorf("members: encode records: %w", err)
	}
	dst := path(l, key)
	tmp, err := os.CreateTemp(l.Members, key+".tmp.*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// Read looks up the member list for key. ok is false (with a nil
// error) iff no sidecar exists for key at all — the normal case for
// every archive that isn't thin, and the signal classifyInputs uses
// to decide whether an input needs member-propagation.
func Read(l paths.Layout, key string) (recs []Record, ok bool, err error) {
	body, err := os.ReadFile(path(l, key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("members: read %s: %w", key, err)
	}
	if err := json.Unmarshal(body, &recs); err != nil {
		return nil, false, fmt.Errorf("members: decode %s: %w", key, err)
	}
	return recs, true, nil
}
