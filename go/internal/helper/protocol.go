package helper

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// The shim<->helper protocol is deliberately NOT the Nix worker
// protocol — it only needs to carry three ops' arguments/results, and
// its own overhead (~162µs for a bare local-socket round trip,
// measured) is negligible next to the ~4.3ms daemon handshake it
// exists to amortize, so simplicity (one JSON envelope, length-
// prefixed) beats a hand-rolled binary format here.

// request is what a shim sends: op selects which internal/rpc.Conn
// method to call, and only the fields that op needs are populated.
type request struct {
	Op string `json:"op"` // "AddDerivation" | "AddToStoreScanning" | "SubmitOutput"

	// AddDerivation
	Name     string   `json:"name,omitempty"`
	Contents []byte   `json:"contents,omitempty"`
	Refs     []string `json:"refs,omitempty"`

	// AddToStoreScanning
	NarDump []byte `json:"narDump,omitempty"`

	// SubmitOutput
	DrvPath string `json:"drvPath,omitempty"`
	Output  string `json:"output,omitempty"`
}

// response carries either a result path (AddDerivation/
// AddToStoreScanning) or nothing (SubmitOutput), plus an error string
// if the call failed. JSON, not a Go error, because this crosses a
// process boundary.
type response struct {
	StorePath string `json:"storePath,omitempty"`
	Err       string `json:"err,omitempty"`
}

// writeFrame/readFrame carry one JSON-encoded message as a
// uint32-length-prefixed byte string over a plain io.ReadWriter. Not
// reusing internal/rpc's own wire helpers deliberately: those encode
// the Nix worker protocol's specific conventions (uint64 lengths,
// 8-byte padding) for a reason that doesn't apply here, and this
// protocol has no need to match any external format.
func writeFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("helper: encode frame: %w", err)
	}
	// Refuse a body the length prefix cannot describe. uint32(len(body))
	// silently WRAPS past 4 GiB, and the wrapped value is small, so it
	// passes readFrame's maxFrameLen check — the reader then consumes a
	// truncated prefix and fails with "unexpected end of JSON input"
	// while the writer believes it succeeded. Checking here, against the
	// same limit the reader enforces, makes the failure loud and local.
	//
	// Reachable in practice: NarDump is []byte, which JSON encodes as
	// base64 (4/3 inflation), so a ~3 GiB NAR overflows. A kernel's
	// whole-tree stage at assemble time is exactly that big.
	if len(body) > maxFrameLen {
		return fmt.Errorf("helper: frame body %d bytes exceeds limit %d", len(body), maxFrameLen)
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(body)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return fmt.Errorf("helper: write frame length: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("helper: write frame body: %w", err)
	}
	if f, ok := w.(*bufio.Writer); ok {
		return f.Flush()
	}
	return nil
}

// maxFrameLen bounds a single frame — nixgg's own drv/NAR payloads
// are small (real ones seen: tens of bytes to tens of KB; see
// ARCHITECTURE.md's own link-line-length numbers for the largest
// scripts this project produces), so a multi-hundred-MB frame is
// certainly a desynchronised protocol, not a legitimate payload.
const maxFrameLen = 512 * 1024 * 1024

// frameEnvelopeBytes reserves room for everything in a frame that is
// NOT the payload: the method name, the store name, the JSON braces,
// quotes and field labels. 4 KiB is far more than any real envelope
// (method and name are short strings) and costs nothing — it only
// shifts the helper/direct-RPC boundary by 4 KiB out of 384 MiB.
const frameEnvelopeBytes = 4096

// MaxPayloadBytes is the largest raw []byte (NAR dump, drv contents) a
// caller may hand the helper. Derived from maxFrameLen rather than
// guessed, so the two cannot drift: JSON encodes []byte as base64,
// which inflates by 4/3.
//
// The envelope allowance is not cosmetic. maxFrameLen*3/4 is EXACTLY
// the payload whose base64 encoding equals maxFrameLen, so the
// surrounding {"method":…,"name":…,"dump":…} bytes push the marshalled
// frame over — and writeFrame then rejects it. Since selectBackendFor
// admits anything <= this constant, a payload in the top sliver of the
// range would be routed to the helper and hard-fail instead of falling
// back to the direct-RPC path the whole mechanism exists to reach.
//
// Callers with a bigger payload must not use the helper — see
// sandbox.StoreAddScan. Pooling exists to amortize the daemon handshake
// across MANY SMALL calls; a single multi-GB upload pays one handshake
// either way, so routing it through the helper buys nothing and costs an
// extra base64 round-trip through a socket.
const MaxPayloadBytes = (maxFrameLen - frameEnvelopeBytes) * 3 / 4

func readFrame(r io.Reader, v any) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return fmt.Errorf("helper: read frame length: %w", err)
	}
	n := binary.LittleEndian.Uint32(lenBuf[:])
	if n > maxFrameLen {
		return fmt.Errorf("helper: frame length %d exceeds sanity limit", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("helper: read frame body: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("helper: decode frame: %w", err)
	}
	return nil
}
