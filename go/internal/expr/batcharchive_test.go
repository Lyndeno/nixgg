package expr

import (
	"fmt"
	"strings"
	"testing"
)

func testMembers() []BatchCompileMember {
	return []BatchCompileMember{
		{
			Tool: "cc", SrcTree: "../srcs/deps-hiredis-sds-o", SrcStore: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-deps-hiredis-sds-o",
			Source: "sds.c", OutName: "sds.o", Flags: []string{"-O2", "-Wall"},
		},
		{
			Tool: "cc", SrcTree: "../srcs/deps-hiredis-net-o", SrcStore: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-deps-hiredis-net-o",
			Source: "net.c", OutName: "net.o", Flags: []string{"-O2"},
		},
	}
}

// TestBatchArchiveMemberOrderPreserved pins that both the compile
// lines and the ar argv preserve the caller's own member order — a
// same-group archive's members are ordered by ar's own argv, and that
// order is semantically load-bearing (some archives are order-
// sensitive for symbol resolution, same reason link.go cares about
// -l-vs-input ordering).
func TestBatchArchiveMemberOrderPreserved(t *testing.T) {
	script := batchArchiveScript("/COREUTILS", "/AR", "rcs", "libhiredis.a", testMembers())

	sdsAt := strings.Index(script, `-c "sds.c"`)
	netAt := strings.Index(script, `-c "net.c"`)
	if sdsAt < 0 || netAt < 0 {
		t.Fatalf("missing a compile line entirely:\n%s", script)
	}
	if sdsAt > netAt {
		t.Errorf("sds.c compiled after net.c — member order not preserved:\n%s", script)
	}

	arAt := strings.Index(script, "ar D")
	arLine := script[arAt:]
	sdsObjAt := strings.Index(arLine, "sds.o")
	netObjAt := strings.Index(arLine, "net.o")
	if sdsObjAt < 0 || netObjAt < 0 || sdsObjAt > netObjAt {
		t.Errorf("ar argv order doesn't match member order:\n%s", arLine)
	}
}

// TestBatchArchiveScriptFlagsQuoted pins that a flag containing a
// shell-meaningful character is safely single-quoted, same escaping
// convention shellQuoteFlags already gives every other Kind.
func TestBatchArchiveScriptFlagsQuoted(t *testing.T) {
	members := []BatchCompileMember{
		{Tool: "cc", SrcStore: "/nix/store/x-src", Source: "a.c", OutName: "a.o",
			Flags: []string{"-DFOO='bar baz'"}},
	}
	script := batchArchiveScript("/COREUTILS", "/AR", "rcs", "lib.a", members)
	if !strings.Contains(script, `'-DFOO='\''bar baz'\'''`) {
		t.Errorf("flag containing a single quote not escaped the way shellQuoteFlags escapes it:\n%s", script)
	}
}

// TestBatchArchiveScriptToolAndPaths pins that each member's compile
// runs from inside its own SrcStore (sandbox mode), using its own
// Tool, and writes into a $objroot shared by every member — the
// mechanism that lets a later `ar` reference every object by a
// uniform, cd-independent path regardless of which SrcTree/SrcStore
// each member's own source lives under.
func TestBatchArchiveScriptToolAndPaths(t *testing.T) {
	script := batchArchiveScript("/COREUTILS", "/AR", "rcs", "libhiredis.a", testMembers())
	if !strings.Contains(script, `cd "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-deps-hiredis-sds-o"`) {
		t.Errorf("missing cd into first member's SrcStore:\n%s", script)
	}
	if !strings.Contains(script, `"cc"`) {
		t.Errorf("tool name not embedded:\n%s", script)
	}
	if !strings.Contains(script, `$objroot/sds.o`) || !strings.Contains(script, `$objroot/net.o`) {
		t.Errorf("compile outputs don't land under a shared $objroot:\n%s", script)
	}
}

// TestBatchArchiveJSONSrcsDedup pins that BatchArchiveJSON's
// inputs.srcs is the union of every member's own SrcStore basename
// plus the archive's own StoreDeps and ExtraSrcs, deduplicated — a
// store path referenced by two members (or by both a member and the
// archive's own StoreDeps) must appear exactly once, or `nix
// derivation add` would receive a duplicate-looking entry.
func TestBatchArchiveJSONSrcsDedup(t *testing.T) {
	members := testMembers()
	// A StoreDep that happens to coincide with one member's own
	// SrcStore basename — must not be duplicated in the result.
	dupStoreDep := "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-deps-hiredis-sds-o"

	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-libhiredis.a", OutName: "libhiredis.a",
		System: "x86_64-linux", Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils",
		AR: "/nix/store/binutils", ARFlags: "rcs",
		Members:   members,
		StoreDeps: []string{dupStoreDep},
		ExtraSrcs: []string{"bash", "coreutils"},
	})

	seen := map[string]int{}
	for _, s := range drv.Inputs.Srcs {
		seen[s]++
	}
	for s, n := range seen {
		if n > 1 {
			t.Errorf("srcs entry %q appears %d times, want at most once", s, n)
		}
	}
	if seen["deps-hiredis-sds-o"] == 0 && seen[StoreBasename(dupStoreDep)] == 0 {
		t.Errorf("expected the deduplicated store path to still appear once; got srcs=%v", drv.Inputs.Srcs)
	}
	for _, want := range []string{"bash", "coreutils", StoreBasename(members[0].SrcStore), StoreBasename(members[1].SrcStore)} {
		if seen[want] == 0 {
			t.Errorf("missing expected srcs entry %q; got %v", want, drv.Inputs.Srcs)
		}
	}
}

// TestBatchArchiveJSONSingleOutput pins the scope decision this whole
// Kind rests on: exactly one output, named "out", same shape as an
// ordinary KindArchive derivation — this is what lets the combined
// derivation's result flow through thunk.LinkPlaceholder /
// sandbox.PointOutputAtDrv unmodified.
func TestBatchArchiveJSONSingleOutput(t *testing.T) {
	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-lib.a", OutName: "lib.a", System: "x86_64-linux",
		Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils", AR: "/nix/store/binutils",
		Members: testMembers(),
	})
	if len(drv.Outputs) != 1 {
		t.Fatalf("got %d outputs, want exactly 1: %+v", len(drv.Outputs), drv.Outputs)
	}
	if _, ok := drv.Outputs["out"]; !ok {
		t.Errorf(`outputs map has no "out" key: %+v`, drv.Outputs)
	}
}

// TestBatchArchiveJSONNeverReferencesSiblingDrv pins the scope
// decision that motivates this Kind's simplicity (see package
// docstring): a batch-archive's inputs.drvs is always empty — every
// input is a plain staged source tree, never a not-yet-realized
// sibling drv/thunk.
func TestBatchArchiveJSONNeverReferencesSiblingDrv(t *testing.T) {
	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-lib.a", OutName: "lib.a", System: "x86_64-linux",
		Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils", AR: "/nix/store/binutils",
		Members: testMembers(),
	})
	if len(drv.Inputs.Drvs) != 0 {
		t.Errorf("Inputs.Drvs = %+v, want empty", drv.Inputs.Drvs)
	}
}

// TestBatchArchiveJSONScriptPassedAsFile pins that the combined
// script never lands directly in Args — it goes through
// Env["batchScript"] + passAsFile, same mechanism and same reason as
// assemble.Build's own Env["buildScript"] fix. Args must stay a
// short, fixed `source "$batchScriptPath"` regardless of member
// count.
func TestBatchArchiveJSONScriptPassedAsFile(t *testing.T) {
	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-lib.a", OutName: "lib.a", System: "x86_64-linux",
		Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils", AR: "/nix/store/binutils",
		Members: testMembers(),
	})
	if len(drv.Args) != 2 || drv.Args[0] != "-c" || drv.Args[1] != `source "$batchScriptPath"` {
		t.Fatalf(`Args = %v, want ["-c", "source \"$batchScriptPath\""]`, drv.Args)
	}
	if drv.Env["passAsFile"] != "batchScript" {
		t.Errorf(`Env["passAsFile"] = %q, want "batchScript"`, drv.Env["passAsFile"])
	}
	if !strings.Contains(drv.Env["batchScript"], `-c "sds.c"`) {
		t.Errorf("Env[\"batchScript\"] missing the actual compile script:\n%s", drv.Env["batchScript"])
	}
}

// TestBatchArchiveJSONArgsStaySmallAtScale pins the actual fix for
// ffmpeg's "Argument list too long" failure batching libavcodec (350+
// members, 1MB+ combined script): with enough members, Args must
// stay a short, fixed string no matter how large the real script
// grows — confirmed directly against MAX_ARG_STRLEN (131072).
func TestBatchArchiveJSONArgsStaySmallAtScale(t *testing.T) {
	members := make([]BatchCompileMember, 900) // more than ffmpeg's largest real archive
	for i := range members {
		members[i] = BatchCompileMember{
			Tool:     "cc",
			SrcStore: fmt.Sprintf("/nix/store/%032x-src%d", i, i),
			Source:   fmt.Sprintf("file%d.c", i),
			OutName:  fmt.Sprintf("file%d.o", i),
			Flags:    []string{"-O2", "-DHAVE_AV_CONFIG_H", "-std=c17"},
		}
	}
	drv := BatchArchiveJSON(BatchArchiveJSONParams{
		Name: "batch-libavcodec.a", OutName: "libavcodec.a", System: "x86_64-linux",
		Bash: "/nix/store/bash", Coreutils: "/nix/store/coreutils", AR: "/nix/store/binutils",
		ARFlags: "rcs", Members: members,
	})

	const maxArgStrlen = 131072 // Linux MAX_ARG_STRLEN — this is exactly the bug
	argvSize := 0
	for _, a := range drv.Args {
		argvSize += len(a)
	}
	if argvSize > 4096 {
		t.Errorf("Args total %d bytes across %d members — want a small, fixed size regardless of member count", argvSize, len(members))
	}
	if argvSize >= maxArgStrlen {
		t.Fatalf("Args total %d bytes exceeds MAX_ARG_STRLEN (%d) — this is exactly the bug", argvSize, maxArgStrlen)
	}
	if len(drv.Env["batchScript"]) < maxArgStrlen {
		t.Fatalf("test setup didn't actually exercise the bug: Env[\"batchScript\"] is only %d bytes, want > %d", len(drv.Env["batchScript"]), maxArgStrlen)
	}
}

// TestBatchArchiveNativeExprShape pins the native-mode expression's
// gross structure: it imports batchArchiver.nix, carries a members
// list with an unquoted srcTree path literal per member (Nix must
// resolve it, not Go), and each member's own compileLine is present
// as an indented string.
func TestBatchArchiveNativeExprShape(t *testing.T) {
	e := BatchArchive(BatchArchiveParams{
		Helpers: "/nix/store/helpers", OutName: "libhiredis.a", ARFlags: "rcs",
		Members: testMembers(),
	})
	if !strings.HasPrefix(e, "import /nix/store/helpers/batchArchiver.nix {\n") {
		t.Fatalf("expression doesn't import batchArchiver.nix:\n%s", e)
	}
	if !strings.Contains(e, "srcTree = ../srcs/deps-hiredis-sds-o;") {
		t.Errorf("first member's srcTree not present as an unquoted path literal:\n%s", e)
	}
	if !strings.Contains(e, `-c "sds.c"`) {
		t.Errorf("first member's compileLine missing:\n%s", e)
	}
	if strings.Contains(e, "SrcStore") || strings.Contains(e, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Errorf("native-mode expression leaked a sandbox-only SrcStore value:\n%s", e)
	}
}
