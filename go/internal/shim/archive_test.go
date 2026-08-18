package shim

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseARArgs pins the `ar` command-line parser. It had no test at
// all, which matters because the shim's decision here is binary: model
// the archive as a derivation, or hand the whole invocation to the real
// `ar` untouched. Getting `archive` wrong doesn't fail loudly — it names
// the derivation's output after the wrong token.
func TestParseARArgs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantMods   string
		wantArch   string
		wantInputs []string
		wantOK     bool
	}{
		{
			name: "canonical rcs", args: []string{"rcs", "libfoo.a", "a.o", "b.o"},
			wantMods: "rcs", wantArch: "libfoo.a", wantInputs: []string{"a.o", "b.o"}, wantOK: true,
		},
		{
			// GNU accepts a leading dash; both spellings must produce the
			// same modifier string, since it lands verbatim in the drv.
			name: "leading dash is tolerated", args: []string{"-rcs", "libfoo.a", "a.o"},
			wantMods: "rcs", wantArch: "libfoo.a", wantInputs: []string{"a.o"}, wantOK: true,
		},
		{
			name: "cru", args: []string{"cru", "libbar.a", "x.o"},
			wantMods: "cru", wantArch: "libbar.a", wantInputs: []string{"x.o"}, wantOK: true,
		},
		{
			// D (deterministic) is already prepended by the emitter, but a
			// caller may pass it explicitly.
			name: "explicit D", args: []string{"Drcs", "libbaz.a", "y.o"},
			wantMods: "Drcs", wantArch: "libbaz.a", wantInputs: []string{"y.o"}, wantOK: true,
		},
		{
			name: "read-mode t is not modelled", args: []string{"t", "libfoo.a"},
			wantOK: false,
		},
		{
			name: "too few args", args: []string{"rcs"},
			wantOK: false,
		},
		{
			// A CREATING op with no members is real and must be modelled:
			// See TestParseARArgsEmptyCreatingArchive for why passing
			// these through breaks every ancestor archive.
			name: "creating op with no inputs", args: []string{"rcs", "libfoo.a"},
			wantOK: true, wantMods: "rcs", wantArch: "libfoo.a", wantInputs: nil,
		},
		{
			// A nested archive IS a legitimate member: build systems
			// list a subdirectory's archive
			// alongside its own objects.
			name: "nested .a member is modelled", args: []string{"rcs", "libfoo.a", "a.o", "sub.a"},
			wantOK: true, wantMods: "rcs", wantArch: "libfoo.a",
			wantInputs: []string{"a.o", "sub.a"},
		},
		{
			// Anything we still cannot model (a .lo, a response file)
			// must bail rather than silently drop the member.
			name: "unmodellable member bails", args: []string{"rcs", "libfoo.a", "a.o", "x.lo"},
			wantOK: false,
		},
		{
			name: "modifier outside the alphabet bails", args: []string{"rzz", "libfoo.a", "a.o"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a, in, ok := parseARArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (args %q)", ok, tc.wantOK, tc.args)
			}
			if !ok {
				return
			}
			if m != tc.wantMods {
				t.Errorf("modifiers = %q, want %q", m, tc.wantMods)
			}
			if a != tc.wantArch {
				t.Errorf("archive = %q, want %q", a, tc.wantArch)
			}
			if !reflect.DeepEqual(in, tc.wantInputs) {
				t.Errorf("inputs = %q, want %q", in, tc.wantInputs)
			}
		})
	}
}

// TestParseARArgsPositionalCountIsMisparsed documents a real defect
// rather than asserting correct behaviour, because fixing it properly
// means teaching parseARArgs ar's positional grammar and that is a
// separate change.
//
// `a`, `b`, `i` and `N` all take a positional argument that follows the
// modifier string: `ar rN <count> <archive> <member>...`. parseARArgs
// treats args[1] as the archive unconditionally, so the count is taken as
// the archive name.
//
// The docstring on parseARArgs claims `ar rN 3 archive.a obj` is
// rejected. It is — but not for the stated reason. It bails because
// "archive.a" lacks a .o suffix and hits the non-.o input check, i.e. by
// accident of an unrelated filter. Change the member names so every
// trailing token ends in .o and the misparse goes through:
//
//	ar rN 2 weird.o member.o
//	  -> modifiers="rN" archive="2" inputs=["weird.o" "member.o"]
//
// The output derivation would be named after "2" and the real archive
// would be treated as a member. This is reachable only from a caller
// using ar's positional-count forms, which no fixture and no example
// build does — hence documented and pinned, not fixed here.
func TestParseARArgsPositionalCountIsMisparsed(t *testing.T) {
	for _, args := range [][]string{
		{"rN", "3", "archive.a", "obj.o"},
		// The form the old .o filter could NOT catch: every trailing
		// token ends in .o, so the count was taken as the archive name
		// and the real archive became a member.
		{"rN", "2", "weird.o", "member.o"},
		{"rb", "existing.o", "libfoo.a", "new.o"},
		{"ri", "existing.o", "libfoo.a", "new.o"},
		{"ra", "existing.o", "libfoo.a", "new.o"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, _, _, ok := parseARArgs(args); ok {
				t.Errorf("positional-argument modifier accepted; "+
					"args[1] is not the archive here (args %q)", args)
			}
		})
	}
}

// TestIsARModifiers pins the alphabet check. It is a set membership test
// with no positional grammar, which is exactly why the misparse above is
// possible — worth stating so a reader doesn't assume more rigour here
// than exists.
func TestIsARModifiers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"rcs", true},
		{"cru", true},
		{"Drcs", true},
		{"t", true},
		{"", false},
		{"rz", false},       // z is not in the alphabet
		{"libfoo.a", false}, // a real filename must not read as modifiers
		{"2", false},        // a positional count must not read as modifiers
		{"rN", true},        // accepted, and that is what enables the misparse
	} {
		if got := isARModifiers(tc.in); got != tc.want {
			t.Errorf("isARModifiers(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The recursive-archive case: a thin archive whose members include
// both objects and a nested archive. Both used to fall through to
// passthrough, where the real ar met a drvref stub.
func TestParseARArgsNestedThinArchive(t *testing.T) {
	mods, archive, inputs, ok := parseARArgs([]string{
		"cDPrST", "lib/math/all.a",
		"lib/math/div64.o", "lib/math/gcd.o",
		"lib/math/tests/all.a",
	})
	if !ok {
		t.Fatalf("parseARArgs rejected a nested thin-archive invocation")
	}
	if mods != "cDPrST" {
		t.Errorf("modifiers = %q, want %q", mods, "cDPrST")
	}
	if archive != "lib/math/all.a" {
		t.Errorf("archive = %q", archive)
	}
	if len(inputs) != 3 || inputs[2] != "lib/math/tests/all.a" {
		t.Errorf("inputs = %q, want the nested .a retained", inputs)
	}
}

func TestIsARModifiersAcceptsThin(t *testing.T) {
	if !isARModifiers("cDPrST") {
		t.Error("cDPrST rejected; T (thin archive) must be accepted")
	}
	// Still reject things that are plainly not a modifier string.
	if isARModifiers("all.a") {
		t.Error("a filename was accepted as a modifier string")
	}
}

// A creating invocation with no members is legitimate. Leaving those to
// passthrough makes them plain files, which makes every ANCESTOR archive
// unmodellable in turn.
func TestParseARArgsEmptyCreatingArchive(t *testing.T) {
	mods, archive, inputs, ok := parseARArgs([]string{"cDPrST", "drivers/cache/all.a"})
	if !ok {
		t.Fatal("empty creating archive rejected; this cascades to every parent")
	}
	if mods != "cDPrST" || archive != "drivers/cache/all.a" {
		t.Errorf("mods/archive = %q/%q", mods, archive)
	}
	if len(inputs) != 0 {
		t.Errorf("inputs = %q, want none", inputs)
	}
}

// Read-only operations also have no members and must still bail —
// the discriminator is the creating modifier, not the member count.
func TestParseARArgsEmptyReadOpStillBails(t *testing.T) {
	for _, args := range [][]string{
		{"t", "aggregate.a"},
		{"p", "libfoo.a"},
		{"x", "libfoo.a"},
	} {
		if _, _, _, ok := parseARArgs(args); ok {
			t.Errorf("read-only op %q was modelled", args)
		}
	}
}
