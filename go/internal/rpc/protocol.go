// Package rpc speaks the Nix daemon's worker protocol directly over the
// Unix socket the builder-rpc-v0 sandbox already exposes via NIX_REMOTE,
// replacing per-call `nix --offline derivation add` / `nix store add
// --scan` / `nix store submit-output` fork+execs with one persistent
// connection per shim invocation.
//
// Every wire format below was read directly out of the exact pinned
// nix-15793 source this flake builds (NixOS/nix@8307c48, protocol 1.39,
// PR #15793's builder-rpc-v0 work) — see:
//
//	src/libstore/worker-protocol-connection.cc  (handshake, processStderr)
//	src/libstore/remote-store.cc                (client-side op bodies)
//	src/libstore/daemon.cc                       (server-side op bodies)
//	src/libstore/common-protocol.cc              (StorePath = plain string)
//	src/libutil/serialise.cc                     (readError, FramedSink)
//
// Not a general worker-protocol client: only the three ops nixgg's
// sandbox shims need (AddToStore, AddToStoreScanning, SubmitOutput) plus
// enough of the handshake/STDERR machinery to reach them. No cgo, no
// third-party deps, matching the rest of this repo.
package rpc

const (
	workerMagic1 = 0x6e697863
	workerMagic2 = 0x6478696f

	// protoVersionNumber is this client's own worker-protocol version,
	// (major<<8)|minor. Sent during handshake; the negotiated version is
	// min(ours, daemon's). 1.38 is the minimum for the feature exchange
	// AddToStoreScanning/SubmitOutput's feature gating depends on.
	protoMajor = 1
	protoMinor = 39

	featureMinVersion = (1 << 8) | 38 // feature list exchange gated on this
)

// op is a WorkerProto::Op value. Only the ones this client sends/expects.
type op uint64

const (
	opIsValidPath        op = 1
	opAddToStore         op = 7
	opSetOptions         op = 19
	opSubmitOutput       op = 1000
	opAddToStoreScanning op = 1001
)

// STDERR wire messages (src/libstore/include/nix/store/worker-protocol.hh).
const (
	stderrNext          uint64 = 0x6f6c6d67
	stderrRead          uint64 = 0x64617461
	stderrWrite         uint64 = 0x64617416
	stderrLast          uint64 = 0x616c7473
	stderrError         uint64 = 0x63787470
	stderrStartActivity uint64 = 0x53545254
	stderrStopActivity  uint64 = 0x53544f50
	stderrResult        uint64 = 0x52534c54
)

// featureAddToStoreScanning / featureSubmitOutput are the daemon
// handshake feature names gating the two builder-rpc-v0-only ops.
const (
	featureAddToStoreScanning = "add-to-store-scanning"
	featureSubmitOutput       = "submit-output"
)

// contentAddressText / contentAddressFixedRecursive are the two
// ContentAddressMethod::renderWithAlgo prefixes this client needs:
// "text:" for a derivation's ATerm text (AddToStore), "fixed:r:" for a
// recursively-NAR-hashed directory dump (AddToStoreScanning) — both
// with "sha256" appended, matching every hash nixgg already uses
// elsewhere (see internal/expr's own sha256 use).
const (
	contentAddressText           = "text:sha256"
	contentAddressFixedRecursive = "fixed:r:sha256"
)
