package shim

import (
	"reflect"
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
			name: "no inputs", args: []string{"rcs", "libfoo.a"},
			wantOK: false,
		},
		{
			// A non-.o member (another archive, a .lo, a response file)
			// means we can't model the member list; bail entirely rather
			// than silently dropping it.
			name: "non-.o input bails", args: []string{"rcs", "libfoo.a", "a.o", "sub.a"},
			wantOK: false,
		},
		{
			// T (thin archive) is a modelled modifier, same as any
			// other — meson emits exactly this for QEMU's internal
			// static libraries.
			name: "thin archive modifiers", args: []string{"csrDT", "libqemuutil.a", "a.o", "b.o"},
			wantMods: "csrDT", wantArch: "libqemuutil.a", wantInputs: []string{"a.o", "b.o"}, wantOK: true,
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
	t.Run("the accidental rejection", func(t *testing.T) {
		// Rejected, but because of the .o filter, not the grammar.
		if _, _, _, ok := parseARArgs([]string{"rN", "3", "archive.a", "obj.o"}); ok {
			t.Error("expected bail (via the non-.o check)")
		}
	})

	t.Run("the misparse the filter does not catch", func(t *testing.T) {
		m, a, in, ok := parseARArgs([]string{"rN", "2", "weird.o", "member.o"})
		if !ok {
			t.Skip("parseARArgs now rejects positional-count forms — " +
				"the grammar was fixed; delete this test and assert the fix instead")
		}
		if a != "2" {
			t.Errorf("archive = %q; this test exists to pin the known-wrong %q. "+
				"If it changed, the positional grammar was addressed.", a, "2")
		}
		t.Logf("known defect: mods=%q archive=%q inputs=%q", m, a, in)
	})
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
		{"csrDT", true},     // meson's own thin-archive modifiers (T = thin)
		{"T", true},         // bare thin flag
	} {
		if got := isARModifiers(tc.in); got != tc.want {
			t.Errorf("isARModifiers(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
