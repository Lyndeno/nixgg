package assemble

import (
	"fmt"
	"strings"
	"testing"
)

func stubsN(n int) []Stub {
	s := make([]Stub, n)
	for i := range s {
		s[i] = Stub{
			RelPath: fmt.Sprintf("drivers/net/ethernet/sub%d/file%d.o", i%50, i),
			DrvPath: fmt.Sprintf("/nix/store/%032d-tu-file%d.o.drv", i, i),
		}
	}
	return s
}

// Small builds must stay on the single-drv path byte for byte: chunking
// exists only to make impossible builds possible, and changing the drv
// for builds that already work would invalidate every cached artifact
// and break the native/sandbox equivalence fixtures for no reason.
func TestScriptFitsKeepsSmallBuildsUnchanged(t *testing.T) {
	if !ScriptFits(stubsN(3), MaxScriptBytes) {
		t.Error("a 3-stub build must fit in one drv")
	}
	if ScriptFits(stubsN(20000), MaxScriptBytes) {
		t.Error("a 20k-stub build must NOT be reported as fitting")
	}
}

// Every chunk's rendered script has to stay under the kernel's
// MAX_ARG_STRLEN, which is the entire point. Checked against the real
// emitter output rather than the estimate, so a drift between
// stubLineCost and BuildChunk cannot hide here.
func TestChunkScriptsStayUnderKernelArgLimit(t *testing.T) {
	const maxArgStrLen = 128 * 1024 // 32 pages, fixed in the kernel
	chunks := ChunkStubs(stubsN(22000), MaxScriptBytes)
	if len(chunks) < 2 {
		t.Fatalf("expected many chunks, got %d", len(chunks))
	}
	total := 0
	for i, c := range chunks {
		total += len(c)
		drv := BuildChunk(ChunkParams{
			Name: "t", System: "x86_64-linux",
			Bash: "/nix/store/b-bash", Coreutils: "/nix/store/c-coreutils",
			Stubs: c,
		})
		script := drv.Args[1]
		if len(script) >= maxArgStrLen {
			t.Errorf("chunk %d script is %d bytes, over the %d-byte per-argument limit",
				i, len(script), maxArgStrLen)
		}
	}
	if total != 22000 {
		t.Errorf("chunks cover %d stubs, want 22000 — stubs were dropped or duplicated", total)
	}
}

// Partitioning must be a pure function of the stub list: the chunk drvs'
// hashes depend on it, so any nondeterminism would make an identical
// build produce different derivations.
func TestChunkStubsIsDeterministic(t *testing.T) {
	s := stubsN(5000)
	a, b := ChunkStubs(s, MaxScriptBytes), ChunkStubs(s, MaxScriptBytes)
	if len(a) != len(b) {
		t.Fatalf("chunk counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if len(a[i]) != len(b[i]) || a[i][0].RelPath != b[i][0].RelPath {
			t.Fatalf("chunk %d differs between runs", i)
		}
	}
}

// A stub whose own line exceeds the budget still has to land somewhere,
// or packing would loop forever or silently drop it.
func TestChunkStubsAlwaysPlacesOversizeStub(t *testing.T) {
	big := Stub{RelPath: strings.Repeat("a", 200000) + ".o", DrvPath: "/nix/store/x-tu-big.o.drv"}
	chunks := ChunkStubs([]Stub{big}, MaxScriptBytes)
	if len(chunks) != 1 || len(chunks[0]) != 1 {
		t.Fatalf("oversize stub was not placed: %d chunks", len(chunks))
	}
}

// The overlay drv must reference every chunk, or files go missing from
// the assembled tree with no error.
func TestBuildOverlayReferencesEveryChunk(t *testing.T) {
	paths := []string{
		"/nix/store/aaa-c0.drv", "/nix/store/bbb-c1.drv", "/nix/store/ccc-c2.drv",
	}
	drv := BuildOverlay(BuildParams{
		Name: "t", System: "x86_64-linux",
		Bash: "/nix/store/b-bash", Coreutils: "/nix/store/c-coreutils",
		TreeSrc: "ddd-tree",
	}, paths)
	if len(drv.Inputs.Drvs) != len(paths) {
		t.Errorf("overlay has %d input drvs, want %d", len(drv.Inputs.Drvs), len(paths))
	}
	if n := strings.Count(drv.Args[1], "cp -a "); n != len(paths)+1 { // +1 for the tree
		t.Errorf("overlay script has %d cp lines, want %d", n, len(paths)+1)
	}
}
