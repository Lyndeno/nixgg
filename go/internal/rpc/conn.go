package rpc

import (
	"fmt"
	"net"
	"strings"
)

// Conn is one worker-protocol session with a Nix daemon, reached over
// the Unix socket a builder-rpc-v0 sandbox exposes via NIX_REMOTE
// (unix://<path>) — the same socket `nix --offline ...` connects to
// when shelled out. Not pooled or reused across processes; nixgg's
// shim is a fresh OS process per compile, so one Conn's lifetime is
// one shim invocation. Still saves the CLI's own process-startup +
// config-load + connect cost on every call (measured ~20-30ms even
// for a no-op invocation) versus fork+exec'ing `nix` per operation.
type Conn struct {
	nc       net.Conn
	w        *wire
	version  int // negotiated (major<<8)|minor
	features map[string]bool
}

// Dial connects to the daemon socket named by NIX_REMOTE and performs
// the full client handshake (magic, version, feature exchange,
// affinity/reserve-space stubs, daemon version + trust status).
//
// remote must be "unix://<path>" — the only form builder-rpc-v0
// sandboxes set (see src/libstore/unix/build/derivation-builder.cc's
// own env["NIX_REMOTE"] = "unix://" + socketPath). Any other scheme
// (empty, "daemon", "auto") means this isn't running inside a sandbox
// that speaks the raw protocol on a known path, and callers should
// fall back to the nix CLI instead of calling Dial at all.
func Dial(remote string) (*Conn, error) {
	path, ok := strings.CutPrefix(remote, "unix://")
	if !ok {
		return nil, fmt.Errorf("rpc: NIX_REMOTE %q is not a unix:// socket", remote)
	}
	nc, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("rpc: dial %s: %w", path, err)
	}
	c := &Conn{nc: nc, w: newWire(nc), features: map[string]bool{}}
	if err := c.handshake(); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *Conn) Close() error { return c.nc.Close() }

// handshake replicates WorkerProto::BasicClientConnection::handshake +
// postHandshake exactly, client side: write our magic+version, flush,
// read daemon magic+version, exchange feature lists if both sides are
// >= 1.38, then (still part of the same round trip) write the two
// obsolete affinity/reserve-space stub values postHandshake sends,
// flush, and read back ClientHandshakeInfo (daemon version string +
// trust flag).
func (c *Conn) handshake() error {
	if err := c.w.writeUint64(workerMagic1); err != nil {
		return fmt.Errorf("rpc: write client magic: %w", err)
	}
	if err := c.w.writeUint64(uint64(protoMajor<<8 | protoMinor)); err != nil {
		return fmt.Errorf("rpc: write client version: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return fmt.Errorf("rpc: flush handshake: %w", err)
	}

	magic, err := c.w.readUint64()
	if err != nil {
		return fmt.Errorf("rpc: read daemon magic: %w", err)
	}
	if magic != workerMagic2 {
		return fmt.Errorf("rpc: protocol mismatch: daemon magic %#x", magic)
	}
	daemonVersionWire, err := c.w.readUint64()
	if err != nil {
		return fmt.Errorf("rpc: read daemon version: %w", err)
	}
	daemonVersion := int(daemonVersionWire) & 0xffff
	if daemonVersion>>8 != protoMajor {
		return fmt.Errorf("rpc: daemon protocol major version %d unsupported (want %d)", daemonVersion>>8, protoMajor)
	}

	negotiated := daemonVersion
	if own := protoMajor<<8 | protoMinor; own < negotiated {
		negotiated = own
	}
	c.version = negotiated

	if negotiated >= featureMinVersion {
		// The negotiated feature set is intersect(ours, daemon's) —
		// src/libstore/remote-store.cc's own RemoteStore::initConnection
		// explicitly adds add-to-store-scanning/submit-output to its
		// local feature set for exactly this reason (they aren't in
		// WorkerProto::latest by default, only added conditionally on
		// the daemon side). Advertising nothing here would negotiate
		// them away even if the daemon supports both.
		ourFeatures := []string{featureAddToStoreScanning, featureSubmitOutput}
		if err := c.w.writeStrings(ourFeatures); err != nil {
			return fmt.Errorf("rpc: write feature list: %w", err)
		}
		if err := c.w.flush(); err != nil {
			return fmt.Errorf("rpc: flush feature list: %w", err)
		}
		daemonFeatures, err := c.w.readStrings()
		if err != nil {
			return fmt.Errorf("rpc: read daemon features: %w", err)
		}
		want := map[string]bool{featureAddToStoreScanning: true, featureSubmitOutput: true}
		for _, f := range daemonFeatures {
			if want[f] {
				c.features[f] = true
			}
		}
	}

	// postHandshake's two obsolete stub writes (CPU affinity, reserve
	// space) — every protocol version this client supports is well
	// past both thresholds (1.14, 1.11), so always send them.
	if err := c.w.writeUint64(0); err != nil { // affinity: none
		return fmt.Errorf("rpc: write affinity stub: %w", err)
	}
	if err := c.w.writeUint64(0); err != nil { // reserveSpace: false
		return fmt.Errorf("rpc: write reserve-space stub: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return fmt.Errorf("rpc: flush post-handshake: %w", err)
	}

	// ClientHandshakeInfo: daemon version string (>= 1.33), then
	// optional trust flag (>= 1.35) as a readNum<uint8_t> tag: 0 =
	// unknown, 1 = trusted, 2 = not trusted. Every daemon we talk to
	// is well past both gates; read unconditionally rather than
	// threading the same two version checks through again.
	if _, err := c.w.readString(); err != nil { // daemon version string, unused
		return fmt.Errorf("rpc: read daemon version string: %w", err)
	}
	if _, err := c.w.readUint64(); err != nil { // trust tag, unused
		return fmt.Errorf("rpc: read trust flag: %w", err)
	}

	// RemoteStore::initConnection drains one more STDERR sequence
	// right after postHandshake (conn.processStderrReturn()) before
	// the connection is usable for any op — skipping this left the
	// daemon's post-handshake STDERR_LAST sitting unread, silently
	// shifting every subsequent read by one message.
	if err := c.w.drainStderr(); err != nil {
		return fmt.Errorf("rpc: post-handshake stderr: %w", err)
	}

	// SetOptions is deliberately never sent. On an ordinary daemon
	// connection RemoteStore::initConnection sends it unconditionally
	// (unless disable-set-options is negotiated, which this client
	// never requests) — but every real caller of this package IS a
	// builder-rpc-v0 sandbox's RecursiveSubmitted daemon, and
	// SetOptions isn't on that daemon's fixed op allowlist at all
	// (src/libstore/daemon.cc's own performOp throws "Operation 19
	// not allowed inside derivation" — confirmed directly against a
	// real sandbox build). Sending it would make every call fail, not
	// just this one being skippable as an optimization.

	return nil
}

// hasFeature reports whether the daemon advertised the named
// builder-rpc-v0 worker-protocol feature during handshake.
func (c *Conn) hasFeature(name string) bool { return c.features[name] }
