package shim

import (
	"testing"

	"github.com/tbereknyei/nixgg/internal/batchmember"
)

// TestDisambiguateOutNamesNoCollision confirms the common case — no
// two members share a basename — passes through unchanged, including
// preserving order and every other field.
func TestDisambiguateOutNamesNoCollision(t *testing.T) {
	in := []batchmember.MemberRecord{
		{Source: "sds.c", OutName: "sds.o"},
		{Source: "net.c", OutName: "net.o"},
	}
	got := disambiguateOutNames(in)
	if len(got) != 2 || got[0].OutName != "sds.o" || got[1].OutName != "net.o" {
		t.Errorf("no-collision case should pass through unchanged, got %+v", got)
	}
	if got[0].Source != "sds.c" || got[1].Source != "net.c" {
		t.Errorf("Source field lost across disambiguation: %+v", got)
	}
}

// TestDisambiguateOutNamesRealCollision pins the exact bug found
// against a real ffmpeg build: libavutil/cpu.c and
// libavutil/x86/cpu.c both compile to OutName "cpu.o". Without
// disambiguation, batchArchiveScript's shared $objroot/cpu.o silently
// drops one member's object before `ar` ever runs. The fix must keep
// both members (never drop one) and give them distinct OutNames.
func TestDisambiguateOutNamesRealCollision(t *testing.T) {
	in := []batchmember.MemberRecord{
		{Source: "libavutil/cpu.c", OutName: "cpu.o"},
		{Source: "libavutil/x86/cpu.c", OutName: "cpu.o"},
	}
	got := disambiguateOutNames(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 members preserved, got %d", len(got))
	}
	if got[0].OutName != "cpu.o" {
		t.Errorf("first member with a given basename should keep it unchanged, got %q", got[0].OutName)
	}
	if got[1].OutName == "cpu.o" {
		t.Errorf("second member must be renamed to avoid colliding with the first, still %q", got[1].OutName)
	}
	if got[1].OutName != "cpu-2.o" {
		t.Errorf("expected deterministic rename to cpu-2.o, got %q", got[1].OutName)
	}
	// The Source (compile input) must be untouched — only the OBJECT
	// name changes; picking the wrong field to rename would silently
	// compile the wrong file.
	if got[0].Source != "libavutil/cpu.c" || got[1].Source != "libavutil/x86/cpu.c" {
		t.Errorf("Source fields must stay exactly as given, got %+v", got)
	}
}

// TestDisambiguateOutNamesTripleCollision confirms three-way
// collisions (and beyond) each get a distinct suffix, not just the
// first duplicate.
func TestDisambiguateOutNamesTripleCollision(t *testing.T) {
	in := []batchmember.MemberRecord{
		{Source: "a/swscale.c", OutName: "swscale.o"},
		{Source: "b/swscale.c", OutName: "swscale.o"},
		{Source: "c/swscale.c", OutName: "swscale.o"},
	}
	got := disambiguateOutNames(in)
	want := []string{"swscale.o", "swscale-2.o", "swscale-3.o"}
	for i, w := range want {
		if got[i].OutName != w {
			t.Errorf("member %d: got OutName %q, want %q", i, got[i].OutName, w)
		}
	}
	seen := map[string]bool{}
	for _, m := range got {
		if seen[m.OutName] {
			t.Fatalf("duplicate OutName %q survived disambiguation: %+v", m.OutName, got)
		}
		seen[m.OutName] = true
	}
}

// TestDisambiguateOutNamesIndependentGroups confirms two SEPARATE
// basenames colliding independently (e.g. both "cpu.o" AND
// "swscale.o" appearing twice) are each disambiguated on their own —
// the numbering for one basename must not leak into another's.
func TestDisambiguateOutNamesIndependentGroups(t *testing.T) {
	in := []batchmember.MemberRecord{
		{Source: "libavutil/cpu.c", OutName: "cpu.o"},
		{Source: "libswscale/swscale.c", OutName: "swscale.o"},
		{Source: "libavutil/x86/cpu.c", OutName: "cpu.o"},
		{Source: "libswscale/x86/swscale.c", OutName: "swscale.o"},
	}
	got := disambiguateOutNames(in)
	want := []string{"cpu.o", "swscale.o", "cpu-2.o", "swscale-2.o"}
	for i, w := range want {
		if got[i].OutName != w {
			t.Errorf("member %d: got OutName %q, want %q", i, got[i].OutName, w)
		}
	}
}
