package helper

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// Worker-protocol magic values, hardcoded here rather than imported
// (internal/rpc keeps them unexported — they're protocol constants,
// not secrets, safe to duplicate in a test fake).
const (
	fakeWorkerMagic1 = 0x6e697863
	fakeWorkerMagic2 = 0x6478696f
	fakeProtoVersion = (1 << 8) | 39 // matches internal/rpc's own protoMajor/protoMinor
	fakeStderrLast   = 0x616c7473
)

func readU64(r io.Reader) (uint64, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

func writeU64(w io.Writer, v uint64) error {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	_, err := w.Write(b[:])
	return err
}

// readPaddedString reads one length-prefixed, 8-byte-zero-padded
// string — the worker protocol's own string encoding (see
// internal/rpc/wire.go's readString, duplicated here rather than
// exported for a test fake to import).
func readPaddedString(r io.Reader) (string, error) {
	n, err := readU64(r)
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	if pad := (8 - n%8) % 8; pad > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(pad)); err != nil {
			return "", err
		}
	}
	return string(buf), nil
}

func writeStrList(w io.Writer, ss []string) error {
	if err := writeU64(w, uint64(len(ss))); err != nil {
		return err
	}
	for _, s := range ss {
		if err := writeU64(w, uint64(len(s))); err != nil {
			return err
		}
		if _, err := io.WriteString(w, s); err != nil {
			return err
		}
		if pad := (8 - len(s)%8) % 8; pad > 0 {
			if _, err := w.Write(make([]byte, pad)); err != nil {
				return err
			}
		}
	}
	return nil
}

// fakeDaemon speaks just enough of the worker protocol's handshake
// (magic, version, feature exchange, affinity/reserve stubs, empty
// version string + unknown trust, one STDERR_LAST) to let
// internal/rpc.Dial succeed against it, then idles until the client
// disconnects. Exists so Pool tests can exercise the REAL Dial path
// (capacity bookkeeping, concurrent Get behavior) without a live Nix
// daemon — internal/rpc's own smoke tests already cover full
// protocol correctness against a real daemon; this fake only needs
// to be a valid enough peer for the handshake to complete.
func fakeDaemon(t *testing.T) (sockPath string, stop func()) {
	t.Helper()
	dir := t.TempDir()
	sockPath = dir + "/fake.sock"

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("fakeDaemon: listen: %v", err)
	}

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveFakeHandshake(c)
		}
	}()

	return sockPath, func() { ln.Close() }
}

func serveFakeHandshake(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))

	magic, err := readU64(c)
	if err != nil || magic != fakeWorkerMagic1 {
		return
	}
	if _, err := readU64(c); err != nil { // client version, ignored
		return
	}

	if err := writeU64(c, fakeWorkerMagic2); err != nil {
		return
	}
	if err := writeU64(c, fakeProtoVersion); err != nil {
		return
	}

	// Feature exchange (both sides >= 1.38): internal/rpc.Conn always
	// sends its 2 features (add-to-store-scanning, submit-output) at
	// this point — drain them by count, then respond with an empty
	// feature list (Pool tests don't exercise those two ops).
	n, err := readU64(c)
	if err != nil {
		return
	}
	for i := uint64(0); i < n; i++ {
		if _, err := readPaddedString(c); err != nil {
			return
		}
	}
	if err := writeStrList(c, nil); err != nil {
		return
	}

	// postHandshake stubs: affinity, reserve-space.
	if _, err := readU64(c); err != nil {
		return
	}
	if _, err := readU64(c); err != nil {
		return
	}

	// ClientHandshakeInfo: daemon version string (empty), trust tag
	// (0 = unknown).
	if err := writeU64(c, 0); err != nil { // empty string length
		return
	}
	if err := writeU64(c, 0); err != nil { // trust: unknown
		return
	}

	// Post-handshake STDERR drain: just STDERR_LAST, nothing else.
	if err := writeU64(c, fakeStderrLast); err != nil {
		return
	}

	// Idle until the client closes — Pool.Get has already returned
	// successfully by this point; nothing else to do for a
	// bookkeeping-only test.
	io.Copy(io.Discard, c)
}
