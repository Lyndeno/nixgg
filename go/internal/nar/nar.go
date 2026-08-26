// Package nar encodes a directory tree into Nix's NAR (Nix ARchive)
// format — the same bytes `nix store add --scan` computes internally
// before uploading, and the exact payload
// internal/rpc.Conn.AddToStoreScanning's framed upload expects.
//
// Every rule here was read directly out of the pinned nix-15793
// source (NixOS/nix@8307c48): src/libutil/archive.cc's
// SourceAccessor::dumpPath (the NAR writer) and
// src/libutil/serialise.cc's writeString/writePadding (the string
// framing every NAR field uses — length-prefixed, 8-byte zero-padded,
// same shape internal/rpc's own wire.go implements for the worker
// protocol, kept as a separate small copy here rather than an
// unexported cross-package import).
package nar

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const magic = "nix-archive-1"

// narWriter buffers NAR-format field writes: length-prefixed,
// 8-byte-zero-padded strings — writeString's own shape in
// serialise.cc, reused verbatim (not the worker protocol's uint64
// LITTLE-endian length prefix by coincidence: it's the same
// `writeString` function in the same codebase, called from both).
type narWriter struct {
	w   *bufio.Writer
	err error
}

func (n *narWriter) str(s string) {
	if n.err != nil {
		return
	}
	var lenBuf [8]byte
	putUint64LE(lenBuf[:], uint64(len(s)))
	if _, err := n.w.Write(lenBuf[:]); err != nil {
		n.err = err
		return
	}
	if _, err := n.w.WriteString(s); err != nil {
		n.err = err
		return
	}
	if pad := (8 - len(s)%8) % 8; pad > 0 {
		var zeros [8]byte
		if _, err := n.w.Write(zeros[:pad]); err != nil {
			n.err = err
		}
	}
}

func putUint64LE(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}

// Dump encodes root (a regular file, directory, or symlink) as a NAR
// and writes it to w. Mirrors SourceAccessor::dumpPath exactly,
// including its directory-entry sort order (lexicographic by name —
// archive.cc iterates a std::map<std::string,std::string>) and its
// depth cap (64, matching narMaxDepth — nixgg's own staged source
// trees never approach this, but an unbounded recursion here would be
// a real DoS surface for anything that does).
func Dump(w io.Writer, root string) error {
	bw := bufio.NewWriter(w)
	nw := &narWriter{w: bw}
	nw.str(magic)
	if err := dumpNode(nw, root, 0); err != nil {
		return err
	}
	if nw.err != nil {
		return nw.err
	}
	return bw.Flush()
}

const maxDepth = 64

func dumpNode(nw *narWriter, path string, depth int) error {
	if depth >= maxDepth {
		return fmt.Errorf("nar: path %q exceeds maximum NAR directory depth of %d", path, maxDepth)
	}

	info, err := os.Lstat(path)
	if err != nil {
		return err
	}

	nw.str("(")

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		nw.str("type")
		nw.str("symlink")
		nw.str("target")
		nw.str(target)

	case info.IsDir():
		nw.str("type")
		nw.str("directory")
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		sort.Strings(names)
		for _, name := range names {
			nw.str("entry")
			nw.str("(")
			nw.str("name")
			nw.str(name)
			nw.str("node")
			if err := dumpNode(nw, filepath.Join(path, name), depth+1); err != nil {
				return err
			}
			nw.str(")")
		}

	case info.Mode().IsRegular():
		nw.str("type")
		nw.str("regular")
		if info.Mode()&0o111 != 0 {
			nw.str("executable")
			nw.str("")
		}
		if err := dumpFileContents(nw, path, info); err != nil {
			return err
		}

	default:
		return fmt.Errorf("nar: %q has an unsupported file type (%v)", path, info.Mode())
	}

	nw.str(")")
	return nil
}

// dumpFileContents writes a regular file's "contents" field: the
// literal tag string, its length as a NAR field, then the raw bytes
// padded to an 8-byte boundary — archive.cc's own dumpContents.
func dumpFileContents(nw *narWriter, path string, info fs.FileInfo) error {
	nw.str("contents")
	if nw.err != nil {
		return nw.err
	}

	size := info.Size()
	var lenBuf [8]byte
	putUint64LE(lenBuf[:], uint64(size))
	if _, err := nw.w.Write(lenBuf[:]); err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	written, err := io.Copy(nw.w, f)
	if err != nil {
		return err
	}
	if written != size {
		return fmt.Errorf("nar: %q changed size while reading (stat %d, read %d)", path, size, written)
	}

	if pad := (8 - int(size)%8) % 8; pad > 0 {
		var zeros [8]byte
		if _, err := nw.w.Write(zeros[:pad]); err != nil {
			return err
		}
	}
	return nil
}
