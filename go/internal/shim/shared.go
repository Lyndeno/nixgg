package shim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// sharedStagingEnabled reports whether staged trees should be symlink
// farms into per-file store objects rather than hardlinked copies.
//
// Gated while the approach is being proven. The decisive unknown is
// whether `nix store add --scan` records a symlink TARGET as a
// reference: if it only scans file contents, the header objects never
// become inputs of the staged tree, so they are not mounted in the
// inner build sandbox and every compile fails on a missing header.
func sharedStagingEnabled() bool {
	return os.Getenv("NIXGG_SHARED_STAGE") == "1"
}

// storeShared puts one file in the store and returns its path,
// memoised so a header included by thousands of translation units is
// added once per build.
//
// The memo is a directory of one file per key rather than a single
// index: shims run concurrently under `make -j`, and one-file-per-key
// with an atomic rename needs no locking.
//
// Keyed on path+mtime+size rather than content. Hashing every header of
// every TU would be millions of hashes; within a single build the
// inputs do not change underneath us, and `nix store add` hashes the
// content anyway to decide the store path — so identical content still
// collapses to one object even when two different paths hold it.
func storeShared(cfg *toolchain.Config, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s|%d|%d", abs, st.ModTime().UnixNano(), st.Size())
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:16])

	dir := filepath.Join(cacheRoot(), ".gg-shared")
	cache := filepath.Join(dir, name)
	if b, err := os.ReadFile(cache); err == nil {
		if sp := strings.TrimSpace(string(b)); sp != "" {
			return sp, nil
		}
	}

	sp, err := sandbox.StoreAddScan(cfg, filepath.Base(abs), abs)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		tmp := cache + ".tmp"
		if os.WriteFile(tmp, []byte(sp), 0o644) == nil {
			_ = os.Rename(tmp, cache) // atomic; losing a race is harmless
		}
	}
	return sp, nil
}

// cacheRoot picks a directory that lives as long as the build does.
// $NIX_BUILD_TOP inside a sandbox, else the system temp dir.
func cacheRoot() string {
	if v := os.Getenv("NIX_BUILD_TOP"); v != "" {
		return v
	}
	return os.TempDir()
}
