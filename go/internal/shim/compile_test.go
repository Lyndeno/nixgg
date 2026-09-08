package shim

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/mode"
	"github.com/tbereknyei/nixgg/internal/scan"
)

// TestRewriteFlagsKeepsForceIncludes guards a bug that already shipped
// and was already fixed once, in 267722b.
//
// `-include <file>` names a file to textually include before the
// translation unit — it is not an include *directory*. Treating it as one
// meant the file was staged as a directory and the flag dropped, so the
// TU compiled without it: a silent miscompile, not a build failure.
//
// That commit's own message records why the integration test missed it:
// "no fixture uses -include, which is why the integration test never
// caught this." So the only thing standing between that bug and a
// reappearance is a unit test at this function, and there wasn't one —
// the fix's tests covered scan.go's extraction helpers and the cache
// round-trip, not rewriteFlags' own drop-then-append logic.
//
// Every case below asserts the full output slice rather than a
// Contains(), because the failure mode is positional: forceInc must land
// AFTER the staged -I flags, or a same-named header in an earlier
// directory wins.
//
// Note on what this can and cannot catch. Adding "-include" back to
// pathFlags is, on its own, a no-op: the pathFlags branch drops the flag
// and its argument exactly as the explicit case does, so output is
// unchanged and no test can see a difference. The original bug needed
// both halves — flag treated as a directory AND no forceInc replacement
// appended — and that combination does fail these tests. Verified by
// mutation, in both the one-sided and two-sided forms.
func TestRewriteFlagsKeepsForceIncludes(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		caller, staged, store, forceInc []string
		want                            []string
	}{
		{
			name:     "caller -include is replaced by the staged one",
			caller:   []string{"-O2", "-include", "config.h", "-Wall"},
			staged:   []string{"-I", "."},
			forceInc: []string{"-include", "config.h"},
			want:     []string{"-O2", "-Wall", "-I", ".", "-include", "config.h"},
		},
		{
			name:     "forceInc lands after staged include dirs",
			caller:   []string{"-include", "pch.h"},
			staged:   []string{"-I", "inc", "-I", "gen"},
			forceInc: []string{"-include", "pch.h"},
			// If forceInc came first, a pch.h in inc/ or gen/ would not
			// be the one already resolved by the scanner.
			want: []string{"-I", "inc", "-I", "gen", "-include", "pch.h"},
		},
		{
			name:     "several -include flags all survive",
			caller:   []string{"-include", "a.h", "-O1", "-include", "b.h"},
			staged:   []string{"-I", "."},
			forceInc: []string{"-include", "a.h", "-include", "b.h"},
			want:     []string{"-O1", "-I", ".", "-include", "a.h", "-include", "b.h"},
		},
		{
			name:     "include dirs are still dropped, both spellings",
			caller:   []string{"-I", "/abs", "-I/abs2", "-isystem", "/sys", "-O2"},
			staged:   []string{"-I", "."},
			forceInc: nil,
			want:     []string{"-O2", "-I", "."},
		},
		{
			name:     "no -include at all is unchanged apart from appends",
			caller:   []string{"-O2", "-Wall"},
			staged:   []string{"-I", "."},
			store:    []string{"-I", "/nix/store/x/include"},
			forceInc: nil,
			want:     []string{"-O2", "-Wall", "-I", ".", "-I", "/nix/store/x/include"},
		},
		{
			name: "-include as the final token doesn't run off the end",
			// A malformed line: -include with no argument. Must not panic
			// and must not consume a token that isn't there.
			caller:   []string{"-O2", "-include"},
			staged:   nil,
			forceInc: nil,
			want:     []string{"-O2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rewriteFlags(tc.caller, tc.staged, tc.store, tc.forceInc)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rewriteFlags mismatch\ncaller  : %q\nstaged  : %q\nforceInc: %q\ngot     : %q\nwant    : %q",
					tc.caller, tc.staged, tc.forceInc, got, tc.want)
			}
		})
	}
}

// TestRewriteFlagsDropsCallerIncludePaths pins that a caller's
// `-include` path never survives verbatim. The caller's spelling is
// relative to its own cwd, which does not exist inside the sandbox; only
// the staged replacement in forceInc is valid there.
func TestRewriteFlagsDropsCallerIncludePaths(t *testing.T) {
	got := rewriteFlags(
		[]string{"-include", "../../outside/config.h", "-O2"},
		[]string{"-I", "."},
		nil,
		[]string{"-include", "config.h"},
	)
	for _, g := range got {
		if strings.Contains(g, "outside") {
			t.Errorf("caller's -include path leaked into the sandbox flags: %q", got)
		}
	}
}

// TestParseCompileArgsExplicitLanguage pins that `-x <lang>` overrides
// extension-based source detection.
//
// isSource matches on suffix, so a precompiled-header compile —
//
//	g++ -x c++-header -c pch.h -o pch.h.gch
//
// which is what CMake's target_precompile_headers and Qt's build emit —
// had no source by nixgg's reckoning, returned ok=false, and fell to
// Passthrough. The output was correct but the TU was never cached or
// distributed, and (before the diagnostics added earlier) said nothing.
func TestParseCompileArgsExplicitLanguage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantSource string
		wantOutput string
		wantOK     bool
	}{
		{
			name:       "c++ precompiled header",
			args:       []string{"-x", "c++-header", "-c", "pch.h", "-o", "pch.h.gch"},
			wantSource: "pch.h", wantOutput: "pch.h.gch", wantOK: true,
		},
		{
			name:       "c precompiled header",
			args:       []string{"-x", "c-header", "-c", "pch.h", "-o", "pch.h.gch"},
			wantSource: "pch.h", wantOutput: "pch.h.gch", wantOK: true,
		},
		{
			// -x also legitimises an unusual extension for a normal
			// compile, which is the same rule the driver applies.
			name:       "explicit language with an odd extension",
			args:       []string{"-x", "c++", "-c", "gen.inc", "-o", "gen.o"},
			wantSource: "gen.inc", wantOutput: "gen.o", wantOK: true,
		},
		{
			// Without -x, a .h is not a source and never was.
			name:   "header with no -x is still not a source",
			args:   []string{"-c", "pch.h", "-o", "pch.h.gch"},
			wantOK: false,
		},
		{
			// The -x value itself must not be mistaken for the source.
			name:       "the language token is not the source",
			args:       []string{"-x", "c++-header", "-c", "real.h"},
			wantSource: "real.h", wantOutput: "real.h.gch", wantOK: true,
		},
		{
			// Two sources is still unmodellable, -x or not.
			name:   "two sources under -x still bails",
			args:   []string{"-x", "c++", "-c", "a.cc", "b.cc"},
			wantOK: false,
		},
		{
			name:   "-x with no value bails rather than indexing past the end",
			args:   []string{"-c", "a.cc", "-x"},
			wantOK: false,
		},
		{
			// AC_LINK_IFELSE / AC_RUN_IFELSE compile a conftest straight to
			// a binary — no -c. This must bail here, into Passthrough,
			// never reaching mode.For/realiseAndLink: those probes run
			// under NIXGG_BYPASS=1 at configure time regardless, but this
			// is the second, independent reason it can't reach the
			// realise carveout. TestRealiseCarveoutOutputsAreAlwaysFlat's
			// unreachability claim rests on this holding.
			name:   "a no -c conftest link line is not a compile",
			args:   []string{"conftest.c", "-o", "conftest"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, out, _, flags, ok := parseCompileArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (args %q)", ok, tc.wantOK, tc.args)
			}
			if !ok {
				return
			}
			if src != tc.wantSource {
				t.Errorf("source = %q, want %q", src, tc.wantSource)
			}
			// Call the production helper rather than restating it: an
			// earlier version of this test reimplemented the rule here,
			// which made a mutation of the real logic invisible.
			if out == "" {
				out = defaultOutputName(src, flags)
			}
			if out != tc.wantOutput {
				t.Errorf("output = %q, want %q", out, tc.wantOutput)
			}
			// -x must survive into the sandbox flags: without it the
			// compiler would guess the language from the extension and
			// produce an object instead of a PCH.
			var sawX bool
			for i := 0; i+1 < len(flags); i++ {
				if flags[i] == "-x" {
					sawX = true
				}
			}
			if !sawX {
				t.Errorf("-x dropped from flags %q — the sandbox compile would "+
					"guess the language from the extension instead", flags)
			}
		})
	}
}

// TestPCHDefaultOutputName pins gcc's naming rule for a precompiled
// header, which differs from the object rule: .gch is appended to the
// source's FULL name, so pch.h becomes pch.h.gch, not pch.gch. Verified
// against gcc by compiling a header with no -o.
func TestPCHDefaultOutputName(t *testing.T) {
	if !isHeaderLang("c++-header") {
		t.Fatal("c++-header must be recognised as a header language")
	}
	for _, lang := range []string{"c-header", "c++-header",
		"objective-c-header", "objective-c++-header"} {
		if !isHeaderLang(lang) {
			t.Errorf("isHeaderLang(%q) = false, want true", lang)
		}
	}
	for _, lang := range []string{"c", "c++", "assembler", ""} {
		if isHeaderLang(lang) {
			t.Errorf("isHeaderLang(%q) = true, want false — a normal compile "+
				"must still get the .o naming rule", lang)
		}
	}
	if got := langOf([]string{"-O2", "-x", "c++-header", "-Wall"}); got != "c++-header" {
		t.Errorf("langOf = %q, want \"c++-header\"", got)
	}
	if got := langOf([]string{"-O2"}); got != "" {
		t.Errorf("langOf with no -x = %q, want \"\"", got)
	}
}

// TestDefaultOutputName covers the -o-omitted path directly. An earlier
// version of the PCH test restated this rule inline instead of calling
// the real function, so a mutation that gave precompiled headers the .o
// naming rule passed clean. Call the production code.
func TestDefaultOutputName(t *testing.T) {
	for _, tc := range []struct {
		source string
		flags  []string
		want   string
	}{
		{"a.cc", nil, "a.o"},
		{"src/b.c", nil, "b.o"},
		{"a.b.cc", nil, "a.b.o"},
		{"noext", nil, "noext.o"},
		// The header rule: extension kept, .gch appended.
		{"pch.h", []string{"-x", "c++-header"}, "pch.h.gch"},
		{"inc/pch.hpp", []string{"-x", "c++-header"}, "pch.hpp.gch"},
		{"pch.h", []string{"-x", "c-header"}, "pch.h.gch"},
		// A non-header -x keeps the object rule.
		{"gen.inc", []string{"-x", "c++"}, "gen.o"},
	} {
		if got := defaultOutputName(tc.source, tc.flags); got != tc.want {
			t.Errorf("defaultOutputName(%q, %q) = %q, want %q",
				tc.source, tc.flags, got, tc.want)
		}
	}
}

// TestOnlyDashXLegitimisesAnOddSource pins that the "any token can be the
// source" relaxation is gated on -x specifically, not on any two-argument
// flag. -Xlinker and -Xassembler go through the same parser branch, and
// their values say nothing about the source language.
//
// Without the guard, `cc -c -Xassembler --foo bar.unknown -o out.o` would
// treat bar.unknown as a source and try to model a TU nixgg cannot
// reason about.
func TestOnlyDashXLegitimisesAnOddSource(t *testing.T) {
	// -Xassembler present, no real source: must NOT adopt the odd token.
	if _, _, _, _, ok := parseCompileArgs([]string{
		"-c", "-Xassembler", "--noexecstack", "mystery.dat", "-o", "out.o",
	}); ok {
		t.Error("an -Xassembler value legitimised a non-source token as the " +
			"compile source; only -x names a language")
	}
	// -Xlinker likewise.
	if _, _, _, _, ok := parseCompileArgs([]string{
		"-c", "-Xlinker", "-z", "mystery.dat", "-o", "out.o",
	}); ok {
		t.Error("-Xlinker legitimised a non-source token as the compile source")
	}
	// And the real thing still works.
	if src, _, _, _, ok := parseCompileArgs([]string{
		"-x", "c++-header", "-c", "pch.h", "-o", "pch.h.gch",
	}); !ok || src != "pch.h" {
		t.Errorf("-x path broken: src=%q ok=%v", src, ok)
	}
}

// TestRealiseCarveoutOutputsAreAlwaysFlat pins that every COMPILE-side
// realise-mode probe's default output stays flat (compile-shaped, per
// expr.ArtifactSubdir), which is what lets compile.go's own
// realiseAndLink call site pass "" as the subdir unconditionally.
//
// realiseAndLink no longer derives the subdir from the output's own
// name at all (it used to, via expr.ArtifactSubdir — but that guessed
// wrong for Kbuild's own vmlinux.o, a LINK output that happens to be
// named like a compile one). Each of realiseAndLink's two call sites
// now states its own Kind's real placement explicitly (compile.go: ""
// always; link.go: "bin" always, per outSubdir()) — so this test's
// claim is purely about these compile-side probes' own filenames,
// pinned because a probe accidentally producing a non-flat output
// would silently break compile.go's own "" call site.
func TestRealiseCarveoutOutputsAreAlwaysFlat(t *testing.T) {
	for _, source := range []string{
		"conftest.c", "conftest.cpp",
		"testCCompiler.c", "CMakeCXXCompilerId.c",
		"CheckFunctionExists.c", "CheckIncludeFile.c",
	} {
		if mode.For(source) != mode.Realise {
			t.Fatalf("%q no longer matches mode.Realise — update this test's "+
				"fixture list, don't just delete the case", source)
		}
		out := defaultOutputName(source, nil)
		if sub := expr.ArtifactSubdir(out); sub != "" {
			t.Errorf("source %q compiles to %q by default, whose ArtifactSubdir "+
				"is %q, want flat (\"\") for a compile-side realise probe",
				source, out, sub)
		}
	}

	// A link-style probe (AC_LINK_IFELSE / AC_RUN_IFELSE, output
	// "conftest" with no extension) is NOT a hole in this guard: it never
	// reaches Compile at all. Those invocations have no -c, so
	// parseCompileArgs returns ok=false and the call falls to Passthrough
	// before mode.For is ever consulted — confirmed by reading
	// parseCompileArgs' hasDashC gate. mode.For's own docstring makes the
	// same claim for a different reason (bypassed() short-circuits first,
	// since these run at configure time); both are true simultaneously,
	// and either one is enough to keep realiseAndLink from ever seeing a
	// bare "conftest". This is deliberately NOT asserted with an
	// ArtifactSubdir check the way the .o cases above are, because
	// ArtifactSubdir("conftest") is genuinely "bin" — the guard here is
	// unreachability, not a flat name, and the two should not be
	// conflated.
}

// TestIsKbuildElfProbe pins the Passthrough carveout for Kbuild's
// scripts/mod/empty.o — see its call site in Compile for why this one
// probe bypasses nixgg's graph entirely (mode.Realise's `nix build
// --file` is incompatible with sandbox mode; this probe has no
// headers and gains nothing from CA-hashing anyway) rather than using
// mode.Realise's synchronous-build carveout the way a real autoconf/
// cmake probe still does.
func TestIsKbuildElfProbe(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"scripts/mod/empty.c", true},
		{"scripts/mod/empty.o", true},
		{"/build/linux-6.12/scripts/mod/empty.c", true},
		{"empty.c", false},               // bare, outside scripts/mod/: an ordinary TU
		{"scripts/mod/modpost.c", false}, // sibling in the same dir, not the probe itself
		{"conftest.c", false},            // a different probe entirely, still mode.Realise's
	} {
		if got := isKbuildElfProbe(tc.source); got != tc.want {
			t.Errorf("isKbuildElfProbe(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestIsKbuildRealmodeObj pins the Passthrough carveout for Kbuild's
// arch/x86/realmode/rm/{header,trampoline_32,trampoline_64,stack,
// reboot}.o — moved here from mode.go's own isKbuildRealmodeObj for
// the identical sandbox-mode reason isKbuildElfProbe was: confirmed
// directly against a real sandboxed build, routed through
// mode.Realise this hit the same "no substituter" failure empty.o
// did.
func TestIsKbuildRealmodeObj(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"arch/x86/realmode/rm/header.S", true},
		{"arch/x86/realmode/rm/header.o", true},
		{"arch/x86/realmode/rm/trampoline_32.S", true},
		{"arch/x86/realmode/rm/trampoline_64.o", true},
		{"arch/x86/realmode/rm/stack.o", true},
		{"arch/x86/realmode/rm/reboot.o", true},
		{"/build/linux-6.12/arch/x86/realmode/rm/reboot.o", true},
		{"arch/x86/realmode/rm/realmode.lds.S", false}, // in the dir, but not a realmode-y member
		{"arch/x86/kernel/head_32.S", false},           // outside the realmode/rm/ dir entirely
	} {
		if got := isKbuildRealmodeObj(tc.source); got != tc.want {
			t.Errorf("isKbuildRealmodeObj(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestIsKbuildVDSO32Obj pins the Passthrough carveout for Kbuild's
// arch/x86/entry/vdso/vdso32/{note,system_call,sigreturn,
// vclock_gettime,vgetcpu}.o. Confirmed directly against a real
// sandboxed build: with these Passthrough'd, vdso32.so.dbg's own link
// falls to RealiseThunkArgsAndPassthrough's Passthrough (a real,
// unshimmed `ld`) instead of mode.ForLink's sandbox-incompatible
// realiseAndLink — the same emergent fix realmode.elf's link already
// gets from isKbuildRealmodeObj above.
func TestIsKbuildVDSO32Obj(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"arch/x86/entry/vdso/vdso32/note.S", true},
		{"arch/x86/entry/vdso/vdso32/system_call.S", true},
		{"arch/x86/entry/vdso/vdso32/sigreturn.o", true},
		{"arch/x86/entry/vdso/vdso32/vclock_gettime.o", true},
		{"arch/x86/entry/vdso/vdso32/vgetcpu.c", true},
		{"/build/linux-6.12/arch/x86/entry/vdso/vdso32/vgetcpu.o", true},
		{"arch/x86/entry/vdso/vdso32/vdso32.lds.S", false}, // in the dir, but not a vobjs32-y member
		{"arch/x86/entry/vdso/vclock_gettime.c", false},    // the 64-bit sibling, outside vdso32/
	} {
		if got := isKbuildVDSO32Obj(tc.source); got != tc.want {
			t.Errorf("isKbuildVDSO32Obj(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestParseCompileArgsCapturesDepfile pins depfile-path recovery for
// the three shapes a compile invocation can request dependency
// output in: Kbuild's comma-joined `-Wp,-MMD,<path>` (the actual form
// scripts/Makefile.lib uses — NOT bare -MD/-MF, which is what an
// earlier, wrong analysis assumed), the autotools-style explicit
// `-MF <path>`, and bare `-MD`/`-MMD` with no `-MF` at all (gcc's own
// documented default: the object's own path with .o replaced by .d).
//
// This path is what lets writeSynthesizedDepfile (see Compile) put a
// substitute .d file exactly where Kbuild's `cmd_and_fixdep` macro
// will look for it — get the path wrong and fixdep still hard-fails.
func TestParseCompileArgsCapturesDepfile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        []string
		wantDepfile string
	}{
		{
			name:        "Kbuild's real form: -Wp,-MMD,<path>",
			args:        []string{"-Wp,-MMD,kernel/.foo.o.d", "-c", "foo.c", "-o", "foo.o"},
			wantDepfile: "kernel/.foo.o.d",
		},
		{
			name:        "-Wp with extra trailing option after the path",
			args:        []string{"-Wp,-MMD,kernel/.foo.o.d,-MP", "-c", "foo.c", "-o", "foo.o"},
			wantDepfile: "kernel/.foo.o.d",
		},
		{
			name:        "explicit -MF",
			args:        []string{"-MD", "-MF", "foo.d", "-c", "foo.c", "-o", "foo.o"},
			wantDepfile: "foo.d",
		},
		{
			name: "bare -MD with no -MF falls back to gcc's own default: obj with .d",
			args: []string{"-MD", "-c", "foo.c", "-o", "foo.o"},
			// gcc's default depfile is next to the OBJECT (foo.o -> foo.d),
			// not the source — confirmed against gcc's own docs.
			wantDepfile: "foo.d",
		},
		{
			name:        "no dep flags at all: no depfile captured",
			args:        []string{"-c", "foo.c", "-o", "foo.o"},
			wantDepfile: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, depfile, _, ok := parseCompileArgs(tc.args)
			if !ok {
				t.Fatalf("parseCompileArgs bailed on %q", tc.args)
			}
			if depfile != tc.wantDepfile {
				t.Errorf("depfile = %q, want %q (args %q)", depfile, tc.wantDepfile, tc.args)
			}
		})
	}
}

// TestParseCompileArgsWpFlagsDoNotLeakIntoSandboxFlags pins that
// -Wp,-MMD,... never reaches the sandbox compile's own flag list: the
// path it carries is meaningless inside the sandbox (relative to
// make's cwd, not the staged tree), and passing it through would make
// the real compiler try to write there and fail.
func TestParseCompileArgsWpFlagsDoNotLeakIntoSandboxFlags(t *testing.T) {
	_, _, _, flags, ok := parseCompileArgs([]string{
		"-Wp,-MMD,kernel/.foo.o.d", "-O2", "-c", "foo.c", "-o", "foo.o",
	})
	if !ok {
		t.Fatal("parseCompileArgs bailed unexpectedly")
	}
	for _, f := range flags {
		if strings.HasPrefix(f, "-Wp,") {
			t.Errorf("-Wp,... flag leaked into sandbox flags: %q", flags)
		}
	}
	if !reflect.DeepEqual(flags, []string{"-O2"}) {
		t.Errorf("flags = %q, want just [-O2]", flags)
	}
}

// TestWriteSynthesizedDepfileSatisfiesRealFixdep runs the REAL Linux
// kernel `fixdep` binary (built from upstream scripts/basic/fixdep.c,
// vendored under testdata/fixdep for exactly this test — see
// testdata/fixdep/README) against a depfile produced by
// writeSynthesizedDepfile, using a real on-disk header instead of a
// hand-typed path list.
//
// This is the same experiment run manually during research (see the
// project's own plan-mode notes on the fixdep gap): fixdep doesn't
// care whether a .d file's dependency list came from genuine `-MD`
// compiler output or was synthesized from scan's own header list — it
// only requires that every listed path be real and readable, which is
// exactly what scan.Run always produces. Guards against a regression
// in writeSynthesizedDepfile's own Makefile-rule syntax silently
// breaking Kbuild's `cmd_and_fixdep` step.
func TestWriteSynthesizedDepfileSatisfiesRealFixdep(t *testing.T) {
	fixdepBin := buildFixdep(t)

	dir := t.TempDir()
	header := filepath.Join(dir, "header.h")
	if err := os.WriteFile(header, []byte("#ifdef CONFIG_FOO\nint x;\n#endif\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "foo.c")
	if err := os.WriteFile(source, []byte(`#include "header.h"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	depfile := filepath.Join(dir, "foo.o.d")
	output := filepath.Join(dir, "foo.o")

	if err := writeSynthesizedDepfile(depfile, output, source, []scan.Header{
		{Abs: header, Rel: "header.h"},
	}); err != nil {
		t.Fatalf("writeSynthesizedDepfile: %v", err)
	}

	cmd := exec.Command(fixdepBin, depfile, "foo.o", "cc -c foo.c -o foo.o")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("real fixdep rejected the synthesized depfile: %v\n%s", err, stderr)
	}
	cmdOut := string(out)
	if !strings.Contains(cmdOut, "savedcmd_foo.o := cc -c foo.c -o foo.o") {
		t.Errorf("fixdep output missing expected savedcmd_ line:\n%s", cmdOut)
	}
	if !strings.Contains(cmdOut, "include/config/FOO") {
		t.Errorf("fixdep did not extract CONFIG_FOO from the synthesized header "+
			"dependency — it never read the header, meaning the synthesized "+
			"depfile's path wasn't recognized as real:\n%s", cmdOut)
	}
}

// buildFixdep compiles the vendored, real upstream fixdep.c (see
// testdata/fixdep/) with the host's cc, skipping the test if no C
// compiler is available on PATH — CI's go-vet/go-test job runs with
// CGO_ENABLED=0 but still has a real `cc` on PATH for this, same as
// TestBatchArchiveScriptThinArchiveSurvivesObjectDeletion's own
// "ar not on PATH" skip in internal/expr/batcharchive_test.go.
func buildFixdep(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		cc, err = exec.LookPath("gcc")
	}
	if err != nil {
		t.Skip("no C compiler on PATH to build the real fixdep binary")
	}
	bin := filepath.Join(t.TempDir(), "fixdep")
	cmd := exec.Command(cc, "-I", "testdata/fixdep", "-o", bin, "testdata/fixdep/fixdep.c")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building real fixdep: %v\n%s", err, out)
	}
	return bin
}
