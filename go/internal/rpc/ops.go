package rpc

import "fmt"

// AddDerivation registers a derivation's ATerm text with the daemon,
// replacing `nix --offline derivation add`'s fork+exec. Mirrors
// RemoteStore::addCAToStore's protocol >= 1.25 branch specialised to
// the one shape nixgg needs: content-addressed as text, no references
// beyond the drv's own srcs/drv inputs (already folded into `refs` by
// the caller — see internal/expr's own reference-collection logic),
// never repairing.
//
// contents is the drv's ATerm-format text (what Store::writeDerivation
// hashes and uploads) — the same bytes `nix derivation add` would send,
// computed by the caller from the same JSON->Derivation parse nixgg's
// existing expr package already does for the CLI path.
//
// Deliberately does not replicate Store::writeDerivation's own
// isValidPath-before-upload short-circuit: getting Nix's exact
// makeStorePath/compressHash formula wrong would silently upload every
// time instead of failing loudly, and the daemon's own LocalStore does
// the equivalent check server-side regardless (addToStoreFromDump ->
// LocalStore's CA short-circuit) — this client only pays one avoidable
// round trip per call, not a correctness risk.
func (c *Conn) AddDerivation(name string, contents []byte, refs []string) (storePath string, err error) {
	if err := c.w.writeUint64(uint64(opAddToStore)); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: write op: %w", err)
	}
	if err := c.w.writeString(name); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: write name: %w", err)
	}
	if err := c.w.writeString(contentAddressText); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: write cam: %w", err)
	}
	if err := c.w.writeStrings(refs); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: write refs: %w", err)
	}
	if err := c.w.writeUint64(0); err != nil { // repair: false
		return "", fmt.Errorf("rpc: AddDerivation: write repair: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: flush request: %w", err)
	}
	if err := c.w.writeFramed(contents); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: upload contents: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: flush upload: %w", err)
	}
	if err := c.w.drainStderr(); err != nil {
		return "", fmt.Errorf("rpc: AddDerivation: %w", err)
	}
	return c.readValidPathInfoPath()
}

// AddToStoreScanning uploads a directory as a NAR, letting the daemon
// scan it for references to already-present store objects — the
// builder-rpc-v0/recursive-nix-only op behind `nix store add --scan`.
// Requires the daemon to have advertised the add-to-store-scanning
// handshake feature; returns an error immediately if not, same as the
// real client does before ever writing the request.
//
// narDump must already be a complete NAR-format dump of the directory
// (see internal/stage or wherever nixgg currently shells out `nix
// store add --scan` from — it already has the staged directory on
// disk and needs a NAR encoder, not a new one written from scratch
// here; this function only owns the wire protocol).
func (c *Conn) AddToStoreScanning(name string, narDump []byte) (storePath string, err error) {
	if !c.hasFeature(featureAddToStoreScanning) {
		return "", fmt.Errorf("rpc: AddToStoreScanning: daemon does not support add-to-store-scanning (not in a builder-rpc-v0/recursive-nix derivation?)")
	}
	if err := c.w.writeUint64(uint64(opAddToStoreScanning)); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: write op: %w", err)
	}
	if err := c.w.writeString(name); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: write name: %w", err)
	}
	if err := c.w.writeString(contentAddressFixedRecursive); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: write cam: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: flush request: %w", err)
	}
	if err := c.w.writeFramed(narDump); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: upload NAR: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: flush upload: %w", err)
	}
	if err := c.w.drainStderr(); err != nil {
		return "", fmt.Errorf("rpc: AddToStoreScanning: %w", err)
	}
	return c.readValidPathInfoPath()
}

// readValidPathInfoPath reads a ValidPathInfo response (StorePath +
// UnkeyedValidPathInfo, per src/libstore/worker-protocol.cc's own
// Serialise<ValidPathInfo>/Serialise<UnkeyedValidPathInfo>::read) and
// returns just the path — every other field (deriver, narHash,
// references, registrationTime, narSize, ultimate, sigs, ca) is
// daemon bookkeeping neither AddDerivation nor AddToStoreScanning's
// caller needs.
func (c *Conn) readValidPathInfoPath() (string, error) {
	path, err := c.w.readString() // StorePath: plain printed path text
	if err != nil {
		return "", fmt.Errorf("rpc: read ValidPathInfo path: %w", err)
	}
	if err := c.skipUnkeyedValidPathInfo(); err != nil {
		return "", err
	}
	return path, nil
}

// skipUnkeyedValidPathInfo discards deriver, narHash, references,
// registrationTime, narSize, and (protocol >= 1.16, true for every
// daemon this client talks to) ultimate/sigs/ca.
func (c *Conn) skipUnkeyedValidPathInfo() error {
	if _, err := c.w.readString(); err != nil { // deriver (optional StorePath, "" if none)
		return fmt.Errorf("rpc: read deriver: %w", err)
	}
	if _, err := c.w.readString(); err != nil { // narHash
		return fmt.Errorf("rpc: read narHash: %w", err)
	}
	if _, err := c.w.readStrings(); err != nil { // references
		return fmt.Errorf("rpc: read references: %w", err)
	}
	if _, err := c.w.readUint64(); err != nil { // registrationTime
		return fmt.Errorf("rpc: read registrationTime: %w", err)
	}
	if _, err := c.w.readUint64(); err != nil { // narSize
		return fmt.Errorf("rpc: read narSize: %w", err)
	}
	if _, err := c.w.readUint64(); err != nil { // ultimate (bool as uint64)
		return fmt.Errorf("rpc: read ultimate: %w", err)
	}
	if _, err := c.w.readStrings(); err != nil { // sigs
		return fmt.Errorf("rpc: read sigs: %w", err)
	}
	if _, err := c.w.readString(); err != nil { // ca
		return fmt.Errorf("rpc: read ca: %w", err)
	}
	return nil
}

// SubmitOutput registers drvPath as the currently-running outer
// derivation's `output` (always "out" for nixgg's own use — mirrors
// go/internal/sandbox's existing SubmitOutput signature), replacing
// `nix store submit-output`'s fork+exec. Only valid inside a
// builder-rpc-v0 sandbox with the outer drv's requiredSystemFeatures
// set accordingly; the daemon enforces this itself and returns a
// protocol error if not, same as the CLI would.
//
// path is always sent as SingleDerivedPath::Opaque (tag 0) — a plain
// already-built .drv StorePath. nixgg never submits a Built path (a
// "drv^output" reference to another not-yet-resolved derivation's
// output); if a future caller needs that, it needs its own tagged
// encoding, not a silent fallthrough here.
func (c *Conn) SubmitOutput(drvPath, output string) error {
	if !c.hasFeature(featureSubmitOutput) {
		return fmt.Errorf("rpc: SubmitOutput: daemon does not support submit-output (not in a derivation with the builder-rpc-v0 feature?)")
	}
	if err := c.w.writeUint64(uint64(opSubmitOutput)); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: write op: %w", err)
	}
	if err := c.w.writeUint64(0); err != nil { // SingleDerivedPath::Opaque tag
		return fmt.Errorf("rpc: SubmitOutput: write path tag: %w", err)
	}
	if err := c.w.writeString(drvPath); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: write path: %w", err)
	}
	if err := c.w.writeString(output); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: write output name: %w", err)
	}
	if err := c.w.flush(); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: flush request: %w", err)
	}
	if err := c.w.drainStderr(); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: %w", err)
	}
	// RemoteStore::submitOutput reads a trailing readInt() after
	// STDERR drains clean — an unused result code (upstream's own
	// C++ client discards it too; see remote-store.cc's call site).
	if _, err := c.w.readUint64(); err != nil {
		return fmt.Errorf("rpc: SubmitOutput: read result: %w", err)
	}
	return nil
}
