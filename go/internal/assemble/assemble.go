// Package assemble walks a build tree left behind by a dynDrvStdenv
// phase-1 buildPhase and finds every drvref stub the nixgg shims wrote
// in place of a real artifact.
//
// dynDrvStdenv (unlike mkNixggBuild) has no single target — the tree
// can have dozens of shimmed outputs — so stubs are discovered by
// walking, not by argument parsing.
package assemble

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tbereknyei/nixgg/internal/drvref"
)

// Stub is one discovered drvref stub: its path relative to the walked
// root, and the drv that will produce its real content.
type Stub struct {
	// RelPath is slash-separated and relative to root, e.g.
	// "src/hello.o" — never absolute.
	RelPath string
	// DrvPath is the full /nix/store/<hash>-<name>.drv path recorded
	// in the stub — see drvref.Path.
	DrvPath string
}

// skipNames are sandbox-infrastructure entries that are never real
// build output. ".nix-socket" is builder-rpc-v0's own unix socket —
// `nix store add --scan` can't ingest a socket. ".gg-stage" is
// StageForScan's own working directory.
//
// ".nixgg" is nixgg's own scratch dir — staged source trees, thunks and
// scan caches. It is scaffolding, never output, and capturing it was
// quietly the most expensive thing the assembly did.
//
// Under shared staging .nixgg/srcs holds one symlink farm per TU. On a
// kernel that is 19,167 farms of ~225 symlinks — about 4.3 MILLION
// symlinks — which StageForScan copied one at a time and `nix store add
// --scan` then ingested, recording a reference to every one of the
// 34,370 distinct file objects they point at.
//
// Three separate failures traced back here, and none of them looked like
// it at the time:
//
//   - The final drv's closure was 82,477 paths, 34,370 of them source
//     files, which blew past fs.mount-max when Nix bind-mounted the lot.
//     The .o outputs were never the problem — their own closure is 1.
//     The captured TREE was dragging every staged header along.
//   - The assembly consumed >22 GB copying and NAR-ing those symlinks,
//     which is what put the build within minutes of filling the disk.
//   - Both costs scale with TU count, so they were invisible on every
//     example build and unavoidable on a kernel.
//
// Excluding it is not a workaround. Phase 2 re-runs make under the
// shims and recreates whatever it needs; the staged sources are already
// in the store as derivation inputs; and sandbox mode marks outputs with
// drvref stub FILES, so nothing in the tree points into .nixgg. Only the
// scan cache is lost, which costs a little time and no correctness.
var skipNames = map[string]bool{
	".nix-socket": true,
	".gg-stage":   true,
	".nixgg":      true,
}

// Walk finds every drvref stub under root, in deterministic
// (lexical) order so the resulting JSON drv hash doesn't depend on
// directory-read ordering.
func Walk(root string) ([]Stub, error) {
	var stubs []Stub
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skipNames[d.Name()] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		ref := drvref.Path(path)
		if ref == "" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		stubs = append(stubs, Stub{RelPath: filepath.ToSlash(rel), DrvPath: ref})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stubs, nil
}

// StageForScan copies root into a fresh directory INSIDE root itself
// (".gg-stage"), excluding skipNames entries, and returns the staged
// path.
//
// Must be inside root, not under an os.MkdirTemp("", ...) dest:
// $TMPDIR inside a builder-rpc-v0 sandbox resolves under root, so an
// externally-supplied dest could itself be a descendant of root,
// making the copy recurse into itself. A fixed, excluded name directly
// under root can't be an ancestor of root, so this can't happen.
//
// `nix store add --scan` also can't ingest root directly: it leaves a
// live .nix-socket (NIX_REMOTE points at it) that --scan rejects, and
// that socket can't be deleted first — the caller's own subsequent
// nix store add/derivation add/submit-output calls go through it.
func StageForScan(root string) (string, error) {
	staged := filepath.Join(root, ".gg-stage")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if skipNames[e.Name()] {
			continue
		}
		if err := copyRecursive(filepath.Join(root, e.Name()), filepath.Join(staged, e.Name())); err != nil {
			return "", err
		}
	}
	return staged, nil
}

// copyRecursive copies a file, directory, or symlink from src to dst,
// preserving symlinks (SONAME alias chains like libfoo.so ->
// libfoo.so.1.2.3 must stay symlinks, not become copies).
func copyRecursive(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			// skipNames applies at EVERY depth, not just the root.
			//
			// StageForScan's own loop filters only its top-level
			// ReadDir, which is not where these live: nixgg's scratch
			// dir sits at the project root chosen by paths.Resolve, so
			// on a kernel it is <src>/linux-6.18.41/build/.nixgg — three
			// levels down, and copied in full by this function.
			//
			// Walk already skips by name at any depth (it uses WalkDir),
			// so the two halves of the assembly disagreed about what
			// counts as build output until this check existed.
			if skipNames[e.Name()] {
				continue
			}
			if err := copyRecursive(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	default:
		return copyFile(src, dst, info.Mode().Perm())
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
