package helper

import (
	"net"
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
