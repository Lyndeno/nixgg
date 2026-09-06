// Package classify inspects a compile input symlink and reports what
// kind of nixgg artifact (if any) it references.
//
// A caller-visible target is one of:
//   - Store    → symlink resolves to /nix/store/…-tu-foo.o/…
//     (already realised; use the store path as a reference)
//   - Thunk    → symlink resolves to .../thunks/<id>.nix
//     (not yet realised; the link/ar thunk `import`s it)
//   - Drv      → symlink resolves to /nix/store/…-tu-foo.o.drv
//     (sandbox-mode: not yet realised; the link/ar drv
//     references it via inputs.drvs)
//   - Regular  → real file or symlink to something nixgg doesn't own
//     (passthrough — link/ar can't include it in a thunk)
//   - Absent   → the path doesn't exist at all
package classify

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/thunk"
)

// Kind labels a classification outcome.
type Kind int

const (
	Absent Kind = iota
	Regular
	Store
	Thunk
	Drv
)

func (k Kind) String() string {
	switch k {
	case Absent:
		return "absent"
	case Regular:
		return "regular"
	case Store:
		return "store"
	case Thunk:
		return "thunk"
	case Drv:
		return "drv"
	}
	return "?"
}

// Result carries the classification plus a reference that means
// different things per kind:
//   - Store: canonical /nix/store/<hash>-<name> root (alt-store prefix
//     stripped so the reference is portable).
//   - Thunk: absolute path to the .nix thunk file.
//   - Regular / Absent: empty.
type Result struct {
	Kind Kind
	Ref  string
	// Sub is the path of the target relative to Ref, set when
	// Kind == Store and the target lives BELOW the store root rather
	// than directly inside it — e.g. Ref=/nix/store/…-zlib with
	// Sub="lib/libz.so". Empty when the target is Ref/<basename>.
	//
	// Callers need both halves: Ref is what `builtins.storePath` and
	// inputs.srcs require (they take a root, not an arbitrary subpath),
	// while Ref+"/"+Sub is what belongs on the compiler's argv.
	// Reconstructing the argv from Ref plus the basename alone silently
	// loses the intermediate directories — which is how LLVM's
	// positional `…-zlib-1.3.2/lib/libz.so` became a link against a
	// nonexistent `…-zlib-1.3.2/libz.so`.
	Sub string
	// Err carries the reason a path could not be inspected, when Kind
	// fell back to Regular because of an Lstat failure rather than
	// because the file genuinely is an ordinary file nixgg doesn't own.
	//
	// Those two cases are behaviourally identical — both passthrough —
	// but they need different diagnostics. EACCES on a build output or
	// an ELOOP symlink chain reported as "isn't a nixgg symlink" sends
	// the reader looking for an ownership problem that isn't there.
	Err error
	// ThunkID is set when Kind == Store AND we know which thunk file
	// produced this output (only populated for promoted regular files
	// today — a real symlink → /nix/store/... doesn't carry that
	// association). Empty otherwise.
	ThunkID string
}

// ArgvPath returns the path that belongs on a compiler/linker command
// line for a Store result, given the caller-visible name it was found
// under. Ref alone is a directory; Ref+Sub is the file.
//
// Falls back to Ref/<name> when Sub is empty, which is the shape every
// nixgg-produced output has (a drv output dir containing one artifact).
func (r Result) ArgvPath(name string) string {
	if r.Sub != "" {
		return r.Ref + "/" + r.Sub
	}
	return r.Ref + "/" + name
}

// Reason describes why this classification means "nixgg can't model
// this input", for use in a passthrough diagnostic. It distinguishes a
// file nixgg simply doesn't own from one it could not inspect.
func (r Result) Reason() string {
	if r.Err != nil {
		return "stat failed: " + r.Err.Error()
	}
	return r.Kind.String()
}

// Target classifies a single path.
//
// altStorePrefix is the on-disk root of the alt store (e.g.
// "/tmp/nixgg-store"), or "" for a system store. We strip it when
// reporting store paths so the reference is always the canonical
// /nix/store/... form that Nix expects.
//
// l is the paths.Layout — needed to consult the "promoted" registry,
// which records regular-file targets that came from a store output.
// Force copies bytes into the working tree (rather than symlinking)
// so make's mtime dependency check works, but downstream link/ar
// shims still need to identify the file's store origin. Pass a
// zero-value Layout to skip the registry check.
func Target(path, altStorePrefix string, l paths.Layout) Result {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{Kind: Absent}
		}
		// Permission denied, symlink loop, I/O error. Treat as Regular
		// (passthrough is the safe action) but keep the cause so the
		// caller can say what actually happened.
		return Result{Kind: Regular, Err: err}
	}
	if info.Mode()&os.ModeSymlink == 0 {
		// Regular file on disk. Could be:
		//   1. A sandbox-mode drvref stub: magic header + drv path.
		//      Written by sandbox.PointOutputAtDrv (see docstring).
		//   2. A "promoted" store output (force copied bytes here).
		//   3. A foreign dependency already living directly under
		//      /nix/store/ — not a symlink at all, just an ordinary
		//      file at its final resolved location. meson (QEMU) and
		//      some CMake generators emit an absolute store path as a
		//      literal positional link argument rather than going
		//      through a -L/-l pair or a SONAME symlink chain — e.g.
		//      "/nix/store/…-zlib-1.3.2-static/lib/libz.a" appearing
		//      verbatim on the link line. Every other route to a
		//      foreign /nix/store/ dependency (an ordinary symlink,
		//      below) already classifies as Store; treating this one
		//      differently just because there's no symlink hop would
		//      silently degrade the WHOLE link to Passthrough (see
		//      classifyInputs' own docstring on why one unmodelable
		//      input takes down the entire step) — exactly the
		//      failure QEMU's own libz.a hit before this case existed.
		//   4. Genuinely a regular file (produced by some other,
		//      unshimmed step in the caller's own build tree).
		if ref := drvref.Path(path); ref != "" {
			return Result{Kind: Drv, Ref: ref}
		}
		if l.Promoted != "" {
			if info := thunk.LookupPromoted(l, path); info != nil {
				return Result{Kind: Store, Ref: info.StorePath, ThunkID: string(info.ThunkID)}
			}
		}
		canonical := path
		if altStorePrefix != "" && strings.HasPrefix(path, altStorePrefix+"/nix/store/") {
			canonical = strings.TrimPrefix(path, altStorePrefix)
		}
		if strings.HasPrefix(canonical, "/nix/store/") {
			root, sub := splitStorePath(canonical)
			return Result{Kind: Store, Ref: root, Sub: sub}
		}
		return Result{Kind: Regular}
	}

	dest, err := readlinkFollow(path)
	if err != nil {
		// A symlink we cannot resolve at all: a loop (ELOOP), or a
		// component we lack permission to traverse. Same shape as the
		// Lstat failure above — keep the cause.
		return Result{Kind: Regular, Err: err}
	}

	// Strip alt-store on-disk prefix to reveal canonical /nix/store/...
	canonical := dest
	if altStorePrefix != "" && strings.HasPrefix(dest, altStorePrefix+"/nix/store/") {
		canonical = strings.TrimPrefix(dest, altStorePrefix)
	}
	// Sandbox-mode: symlink → /nix/store/<hash>-<name>.drv. Reported
	// as Drv so link/archive shims include it under inputs.drvs and
	// reference it via the CA output placeholder. Must be checked
	// BEFORE the generic /nix/store/ branch because .drv paths *are*
	// under /nix/store.
	if strings.HasPrefix(canonical, "/nix/store/") && strings.HasSuffix(canonical, ".drv") {
		return Result{Kind: Drv, Ref: canonical}
	}
	if strings.HasPrefix(canonical, "/nix/store/") {
		root, sub := splitStorePath(canonical)
		return Result{Kind: Store, Ref: root, Sub: sub}
	}
	if strings.HasSuffix(dest, ".nix") {
		return Result{Kind: Thunk, Ref: dest}
	}
	// Sandbox mode again: the symlink resolved to one of our own drvref
	// stubs rather than into the store. This is what a SONAME alias chain
	// looks like — libfoo.so -> libfoo.so.1.2.3, ordinary libtool and
	// autotools output — where the target is the stub nixgg wrote for the
	// real link output.
	//
	// Without this, such an input classifies as Regular, the link shim's
	// default case fires, and the whole link silently degrades to an
	// unaccelerated Passthrough. The direct-regular-file branch above has
	// always checked this; the symlink path did not.
	if ref := drvref.Path(dest); ref != "" {
		// Sub carries the referenced drv's REAL output basename
		// (dest's own basename — e.g. "libcrypto.so.3"), not the
		// caller's alias name ("libcrypto.so"). Without this, the
		// emitted link line reaches for "<drv-out>/bin/libcrypto.so"
		// — a file that never exists, since the drv's own output is
		// named libcrypto.so.3 — and ld fails with "cannot find ...:
		// No such file or directory". Confirmed directly against
		// openssl's engines/*.so, which link against the plain
		// `ln -s libcrypto.so.3 libcrypto.so` alias its own Makefile
		// creates.
		return Result{Kind: Drv, Ref: ref, Sub: filepath.Base(dest)}
	}
	return Result{Kind: Regular}
}

// splitStorePath splits a store path into its /nix/store/<hash>-<name>
// root — the part `builtins.storePath` and inputs.srcs accept — and the
// remainder below it.
//
// sub is "" when p IS the root, and otherwise the relative path inside
// it: ("…-zlib", "lib/libz.so") for "…-zlib/lib/libz.so". Callers that
// put the target on a command line must use root+"/"+sub, not root plus
// the basename; see Result.Sub.
func splitStorePath(p string) (root, sub string) {
	rest := strings.TrimPrefix(p, "/nix/store/")
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		return "/nix/store/" + rest[:slash], rest[slash+1:]
	}
	return "/nix/store/" + rest, ""
}

// readlinkFollow returns the final resolved target. We use EvalSymlinks
// because a symlink can chain (e.g. output → thunk → nothing yet). If
// the chain leads to a nonexistent path we fall back to the raw target.
func readlinkFollow(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	return os.Readlink(path)
}
