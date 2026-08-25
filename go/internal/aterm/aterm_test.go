package aterm

import (
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/expr"
)

// TestUnparseMatchesRealDrv verifies Unparse's output is byte-for-byte
// identical to a REAL .drv file produced by an actual nixgg sandbox
// build, not just "parses without error." The fixture below is
// go/internal/rpc/smoke_test.go's own captured tu-main.o.drv (same
// real build, same known-good bytes) — reused here specifically so a
// single capture backs both the wire-protocol test and this
// serialization test.
//
// Captured via:
//
//	rm -rf /tmp/x && mkdir -p /tmp/x
//	nix build --store 'local?root=/tmp/x' /path/to/nixgg#hello
//	cat /tmp/x/nix/store/8wk651b123fd3i02qm82w2h9h0i9gj8k-tu-main.o.drv
func TestUnparseMatchesRealDrv(t *testing.T) {
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
		Outputs: map[string]expr.JSONOut{
			"out": {Method: "nar", HashAlgo: "sha256"},
		},
		Version: 4,
	}

	const want = "Derive([(\"out\",\"\",\"r:sha256\",\"\")],[],[\"/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15\",\"/nix/store/di26b1kkbammy0sj70nq5qzvfrh78wxl-coreutils-9.11\",\"/nix/store/l5qkpzsr4gxvksh45b3nhxbkyr5cviar-gcc-wrapper-15.3.0\",\"/nix/store/sn3gzwirv4mxvkwc7y15cvfslh93r080-main-16d9a98d6185\"],\"x86_64-linux\",\"/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15/bin/bash\",[\"-c\",\"set -euo pipefail\\nexport PATH=\\\"/nix/store/di26b1kkbammy0sj70nq5qzvfrh78wxl-coreutils-9.11/bin:/nix/store/l5qkpzsr4gxvksh45b3nhxbkyr5cviar-gcc-wrapper-15.3.0/bin\\\"\\nmkdir -p \\\"$out\\\"\\ncd \\\"$src\\\"\\n\\\"c++\\\" '-O2' '-Wall' '-I.' -c \\\"$source\\\" -o \\\"$out/$outName\\\"\\n\"],[(\"_storeDeps\",\"\"),(\"builder\",\"/nix/store/bwry105g7v5jspr41bx9x3fcfqsmfkq2-bash-interactive-5.3p15/bin/bash\"),(\"name\",\"tu-main.o\"),(\"out\",\"/1rz4g4znpzjwh1xymhjpm42vipw92pr73vdgl6xs1hycac8kf2n9\"),(\"outName\",\"main.o\"),(\"outputHashAlgo\",\"sha256\"),(\"outputHashMode\",\"nar\"),(\"source\",\"main.cc\"),(\"src\",\"/nix/store/sn3gzwirv4mxvkwc7y15cvfslh93r080-main-16d9a98d6185\"),(\"system\",\"x86_64-linux\")])"

	got := Unparse(drv)
	if got != want {
		t.Fatalf("Unparse mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestUnparseWithInputDrvs exercises writeInputDrvs's code path,
// confirmed against a REAL end-to-end sandbox build: .#hello's
// link step (bin-hello) has a real, non-empty inputs.drvs entry
// for each compile TU it links against (tu-main.o.drv,
// tu-util.o.drv) — the compile TUs hadn't been submitted/resolved
// yet when the link step's own JSON drv was built. Confirmed
// directly that expr.go's toJSON keys this map by BASENAME
// (StoreBasename(in.Ref)), not a full path — an earlier draft of
// this file assumed full paths per JSONDrvInputs' own (since
// corrected) docstring, and the real build's "not an absolute path"
// daemon error caught the mismatch immediately.
func TestUnparseWithInputDrvs(t *testing.T) {
	drv := expr.JSONDrv{
		Name:    "bin-example",
		System:  "x86_64-linux",
		Builder: "/bin/bash",
		Args:    []string{"-c", "true"},
		Env:     map[string]string{},
		Inputs: expr.JSONDrvInputs{
			Drvs: map[string]expr.JSONDrvRef{
				"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-ar-libb.a.drv": {Outputs: []string{"out"}},
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-ar-liba.a.drv": {Outputs: []string{"out"}},
			},
			Srcs: []string{},
		},
		Outputs: map[string]expr.JSONOut{"out": {Method: "nar", HashAlgo: "sha256"}},
		Version: 4,
	}

	got := Unparse(drv)
	const wantInputDrvs = `[("/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-ar-liba.a.drv",["out"]),("/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-ar-libb.a.drv",["out"])]`
	if !strings.Contains(got, wantInputDrvs) {
		t.Fatalf("Unparse missing expected inputDrvs section:\n got: %s\nwant substring: %s", got, wantInputDrvs)
	}
}
