package sandbox

import (
	"os"
	"testing"

	"github.com/tbereknyei/nixgg/internal/aterm"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/rpc"
)

// TestATermAndRPCComposeAgainstRealDaemon proves the two new pieces
// (internal/aterm.Unparse, internal/rpc.Conn.AddDerivation) work
// TOGETHER against a real daemon socket — not just each verified in
// isolation. Same known-good fixture/expected path as
// internal/rpc/smoke_test.go and internal/aterm/aterm_test.go, but
// unparsing via aterm.Unparse instead of a hand-transcribed string,
// so a mismatch between the two packages' assumptions (e.g. a
// quoting difference) would show up here even if each package's own
// test suite passes independently.
//
//	NIXGG_RPC_SMOKE_SOCKET=/nix/var/nix/daemon-socket/socket go test ./internal/sandbox/ -run TestATermAndRPCComposeAgainstRealDaemon -v
func TestATermAndRPCComposeAgainstRealDaemon(t *testing.T) {
	sock := os.Getenv("NIXGG_RPC_SMOKE_SOCKET")
	if sock == "" {
		t.Skip("NIXGG_RPC_SMOKE_SOCKET not set")
	}

	drv := expr.JSONDrv{
		Name:    "tu-main.o",
		System:  "x86_64-linux",
		Builder: "/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15/bin/bash",
		Args: []string{
			"-c",
			"set -euo pipefail\nexport PATH=\"/nix/store/di26b1kkbammy0sj70nq5qzvfrh78wxl-coreutils-9.11/bin:/nix/store/l5qkpzsr4gxvksh45b3nhxbkyr5cviar-gcc-wrapper-15.3.0/bin\"\nmkdir -p \"$out\"\ncd \"$src\"\n\"c++\" '-O2' '-Wall' '-I.' -c \"$source\" -o \"$out/$outName\"\n",
		},
		Env: map[string]string{
			"_storeDeps":     "",
			"builder":        "/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15/bin/bash",
			"name":           "tu-main.o",
			"out":            "/1rz4g4znpzjwh1xymhjpm42vipw92pr73vdgl6xs1hycac8kf2n9",
			"outName":        "main.o",
			"outputHashAlgo": "sha256",
			"outputHashMode": "nar",
			"source":         "main.cc",
			"src":            "/nix/store/sn3gzwirv4mxvkwc7y15cvfslh93r080-main-16d9a98d6185",
			"system":         "x86_64-linux",
		},
		Inputs: expr.JSONDrvInputs{
			Drvs: map[string]expr.JSONDrvRef{},
			Srcs: []string{
				"bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15",
				"di26b1kkbammy0sj70nq5qzvfrh78wxl-coreutils-9.11",
				"l5qkpzsr4gxvksh45b3nhxbkyr5cviar-gcc-wrapper-15.3.0",
				"sn3gzwirv4mxvkwc7y15cvfslh93r080-main-16d9a98d6185",
			},
		},
		Outputs: map[string]expr.JSONOut{"out": {Method: "nar", HashAlgo: "sha256"}},
		Version: 4,
	}

	contents := aterm.Unparse(drv)

	conn, err := rpc.Dial("unix://" + sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	refs := []string{
		"/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15",
		"/nix/store/di26b1kkbammy0sj70nq5qzvfrh78wxl-coreutils-9.11",
		"/nix/store/l5qkpzsr4gxvksh45b3nhxbkyr5cviar-gcc-wrapper-15.3.0",
		"/nix/store/sn3gzwirv4mxvkwc7y15cvfslh93r080-main-16d9a98d6185",
	}
	path, err := conn.AddDerivation("tu-main.o.drv", []byte(contents), refs)
	if err != nil {
		t.Fatalf("AddDerivation: %v", err)
	}

	const wantPath = "8wk651b123fd3i02qm82w2h9h0i9gj8k-tu-main.o.drv"
	if got := path[len(path)-len(wantPath):]; got != wantPath {
		t.Fatalf("aterm.Unparse + rpc.AddDerivation produced %s, want suffix %s", path, wantPath)
	}
	t.Logf("added: %s (aterm + rpc compose correctly)", path)
}
