package rpc

import (
	"os"
	"strings"
	"testing"
)

// TestDialAmbientDaemon is a manual integration smoke test against a
// REAL Nix daemon socket — not part of the normal go test run
// (requires NIXGG_RPC_SMOKE_SOCKET pointing at a live socket, so CI
// and sandboxed test runs skip it automatically). Verifies the
// handshake alone; run with:
//
//	NIXGG_RPC_SMOKE_SOCKET=/nix/var/nix/daemon-socket/socket go test ./internal/rpc/ -run TestDialAmbientDaemon -v
func TestDialAmbientDaemon(t *testing.T) {
	sock := os.Getenv("NIXGG_RPC_SMOKE_SOCKET")
	if sock == "" {
		t.Skip("NIXGG_RPC_SMOKE_SOCKET not set")
	}
	c, err := Dial("unix://" + sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	t.Logf("negotiated version %d.%d, features=%v", c.version>>8, c.version&0xff, c.features)
}

// TestAddDerivationAmbientDaemon round-trips a REAL nixgg-produced
// derivation's exact ATerm bytes (captured from a live sandbox build
// of .#hello's tu-main.o.drv — see this test's own comment for the
// capture command) through the real daemon socket via AddToStore (op
// 7), the same call `nix derivation add` makes minus the fork+exec,
// and asserts the resulting store path matches the ALREADY-KNOWN
// path Nix itself computed for these exact bytes. A wire-format
// mistake in this client would either error outright or (worse,
// silently) produce a different path; comparing against a real,
// independently-verified drv closes that gap completely — this is
// not just "it didn't error."
func TestAddDerivationAmbientDaemon(t *testing.T) {
	sock := os.Getenv("NIXGG_RPC_SMOKE_SOCKET")
	if sock == "" {
		t.Skip("NIXGG_RPC_SMOKE_SOCKET not set")
	}
	c, err := Dial("unix://" + sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	// Captured via:
	//   nix build --store 'local?root=/tmp/x' .#hello
	//   nix derivation show --store 'local?root=/tmp/x' <tu-main.o.drv path>
	// then reading the raw ATerm bytes straight from the store path
	// (`cat /tmp/x/nix/store/<hash>-tu-main.o.drv`) — the exact bytes
	// this client must reproduce the same store path for.
	const wantPath = "8wk651b123fd3i02qm82w2h9h0i9gj8k-tu-main.o.drv"
	contents := "Derive([(\"out\",\"\",\"r:sha256\",\"\")],[],[\"/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15\",\"/nix/store/di26b1kkbammy0sj70nq5qzvfrh78wxl-coreutils-9.11\",\"/nix/store/l5qkpzsr4gxvksh45b3nhxbkyr5cviar-gcc-wrapper-15.3.0\",\"/nix/store/sn3gzwirv4mxvkwc7y15cvfslh93r080-main-16d9a98d6185\"],\"x86_64-linux\",\"/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15/bin/bash\",[\"-c\",\"set -euo pipefail\\nexport PATH=\\\"/nix/store/di26b1kkbammy0sj70nq5qzvfrh78wxl-coreutils-9.11/bin:/nix/store/l5qkpzsr4gxvksh45b3nhxbkyr5cviar-gcc-wrapper-15.3.0/bin\\\"\\nmkdir -p \\\"$out\\\"\\ncd \\\"$src\\\"\\n\\\"c++\\\" '-O2' '-Wall' '-I.' -c \\\"$source\\\" -o \\\"$out/$outName\\\"\\n\"],[(\"_storeDeps\",\"\"),(\"builder\",\"/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15/bin/bash\"),(\"name\",\"tu-main.o\"),(\"out\",\"/1rz4g4znpzjwh1xymhjpm42vipw92pr73vdgl6xs1hycac8kf2n9\"),(\"outName\",\"main.o\"),(\"outputHashAlgo\",\"sha256\"),(\"outputHashMode\",\"nar\"),(\"source\",\"main.cc\"),(\"src\",\"/nix/store/sn3gzwirv4mxvkwc7y15cvfslh93r080-main-16d9a98d6185\"),(\"system\",\"x86_64-linux\")])"
	refs := []string{
		"/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15",
		"/nix/store/di26b1kkbammy0sj70nq5qzvfrh78wxl-coreutils-9.11",
		"/nix/store/l5qkpzsr4gxvksh45b3nhxbkyr5cviar-gcc-wrapper-15.3.0",
		"/nix/store/sn3gzwirv4mxvkwc7y15cvfslh93r080-main-16d9a98d6185",
	}

	path, err := c.AddDerivation("tu-main.o.drv", []byte(contents), refs)
	if err != nil {
		t.Fatalf("AddDerivation: %v", err)
	}
	if !strings.HasSuffix(path, wantPath) {
		t.Fatalf("AddDerivation produced %s, want suffix %s — wire format mismatch", path, wantPath)
	}
	t.Logf("added: %s (matches known-good path)", path)
}
