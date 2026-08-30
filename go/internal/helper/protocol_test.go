package helper

import (
	"bytes"
	"net"
	"strings"
	"testing"
)

// TestFrameRoundTrip verifies writeFrame/readFrame agree with each
// other for every request/response shape the three ops use.
func TestFrameRoundTrip(t *testing.T) {
	cases := []request{
		{Op: "AddDerivation", Name: "tu-foo.o.drv", Contents: []byte("Derive([...])"), Refs: []string{"/nix/store/x"}},
		{Op: "AddToStoreScanning", Name: "src-foo", NarDump: []byte{0x00, 0xff, 0x01, 0xfe}},
		{Op: "SubmitOutput", DrvPath: "/nix/store/y.drv", Output: "out"},
	}

	for _, req := range cases {
		t.Run(req.Op, func(t *testing.T) {
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()

			done := make(chan struct{})
			go func() {
				defer close(done)
				var got request
				if err := readFrame(server, &got); err != nil {
					t.Errorf("server readFrame: %v", err)
					return
				}
				if got.Op != req.Op {
					t.Errorf("got op %q, want %q", got.Op, req.Op)
				}
				resp := response{StorePath: "/nix/store/result"}
				if err := writeFrame(server, &resp); err != nil {
					t.Errorf("server writeFrame: %v", err)
				}
			}()

			if err := writeFrame(client, &req); err != nil {
				t.Fatalf("client writeFrame: %v", err)
			}
			var resp response
			if err := readFrame(client, &resp); err != nil {
				t.Fatalf("client readFrame: %v", err)
			}
			if resp.StorePath != "/nix/store/result" {
				t.Fatalf("got %q, want /nix/store/result", resp.StorePath)
			}
			<-done
		})
	}
}

// TestFrameRoundTripPreservesBinaryContents pins that Contents/
// NarDump survive the JSON+base64 round trip byte-for-byte, including
// bytes that would be invalid UTF-8 if naively treated as a string —
// a real drv's ATerm text and a real NAR dump are both arbitrary
// bytes, not text.
func TestFrameRoundTripPreservesBinaryContents(t *testing.T) {
	want := []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd, 'D', 'e', 'r', 'i', 'v', 'e'}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := request{Op: "AddDerivation", Contents: want}
		if err := writeFrame(server, &req); err != nil {
			t.Errorf("writeFrame: %v", err)
		}
	}()

	var got request
	if err := readFrame(client, &got); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	<-done
	if string(got.Contents) != string(want) {
		t.Fatalf("got %v, want %v", got.Contents, want)
	}
}

// writeFrame must refuse a body its uint32 length prefix cannot
// describe, rather than silently wrapping.
//
// This is the bug a kernel build hit: NarDump is []byte, JSON encodes
// that as base64 (4/3 inflation), so a ~3 GiB whole-tree NAR produced a
// body over 4 GiB. uint32(len(body)) wrapped to a SMALL number, which
// then passed readFrame's maxFrameLen sanity check — the reader
// consumed a truncated prefix and failed with "unexpected end of JSON
// input" while the writer believed it had succeeded. The sanity limit
// was defeated by the very overflow it existed to catch, because it ran
// on the reader after the writer had already corrupted the length.
func TestWriteFrameRejectsOversizedBody(t *testing.T) {
	// The invariant that matters is not an arithmetic identity but a
	// round-trip one: a payload of exactly MaxPayloadBytes must SURVIVE
	// writeFrame. maxFrameLen*3/4 is precisely the payload whose base64
	// equals maxFrameLen, leaving no room for the JSON envelope, so the
	// cap reserves frameEnvelopeBytes. Asserting the old identity here
	// would re-pin the off-by-envelope bug.
	//
	// The check is on the marshalled body, so drive it through the real
	// encoder rather than fabricating a length.
	atCap := make([]byte, MaxPayloadBytes)
	var okSink bytes.Buffer
	if err := writeFrame(&okSink, request{Op: "AddToStoreScanning", NarDump: atCap}); err != nil {
		t.Fatalf("writeFrame rejected a payload of exactly MaxPayloadBytes (%d): %v — "+
			"selectBackendFor admits this size, so it would hard-fail instead of falling back",
			MaxPayloadBytes, err)
	}
	// A payload just past what the frame can carry.
	big := make([]byte, MaxPayloadBytes+(1<<20))
	var sink bytes.Buffer
	err := writeFrame(&sink, request{Op: "AddToStoreScanning", NarDump: big})
	if err == nil {
		t.Fatal("writeFrame accepted a body larger than maxFrameLen; it would wrap the length prefix")
	}
	if !strings.Contains(err.Error(), "exceeds limit") {
		t.Errorf("want an explicit size error, got %v", err)
	}
	if sink.Len() != 0 {
		t.Errorf("writeFrame wrote %d bytes before failing; a partial frame desynchronises the stream", sink.Len())
	}
}
