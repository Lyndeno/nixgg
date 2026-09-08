package mode

import "testing"

// TestFor pins the realise carveout's pattern set. Each entry exists
// because a real project tripped it, so a "cleanup" that narrows these
// silently breaks that project's configure step.
func TestFor(t *testing.T) {
	for _, tc := range []struct {
		path string
		want Mode
	}{
		// autoconf conftests: `if ./conftest; then ... fi`
		{"conftest.c", Realise},
		{"conftest", Realise},
		{"conftest.cpp", Realise},
		{"/tmp/build/conftest.c", Realise},

		// cmake compiler detection
		{"testCCompiler.c", Realise},
		{"testCXXCompilerABI_C.cpp", Realise},
		{"CMakeCCompilerId.c", Realise},
		{"CMakeCXXCompilerABI_CXX.cpp", Realise},

		// cmake Check* macros
		{"CheckFunctionExists.c", Realise},
		{"CheckIncludeFile.c", Realise},
		{"CheckCSourceCompiles.c", Realise},
		{"CheckCSourceRuns.c", Realise},
		{"CheckSymbolExists.c", Realise},
		{"CheckTypeSize.c", Realise},

		// cmake TryCompile scratch dirs, matched on the path not the base
		{"/b/CMakeFiles/CMakeScratch/x/src.c", Realise},
		{"/b/CMakeFiles/CMakeTmp/src.c", Realise},

		// Linux Kbuild's scripts/mod/empty.o: mk_elfconfig reads its raw
		// ELF header bytes synchronously, same probe shape as a conftest —
		// but this one is handled by a plain Passthrough directly in
		// compile.go's own isKbuildElfProbe, not by this package, because
		// mode.Realise's `nix build --file` mechanism is incompatible with
		// sandbox mode and this probe gains nothing from nixgg's graph
		// anyway. So For defers here, same as any ordinary source.
		{"scripts/mod/empty.c", Placeholder},
		{"scripts/mod/empty.o", Placeholder},
		{"/build/linux-6.12/scripts/mod/empty.c", Placeholder},

		// Linux Kbuild's arch/x86/realmode/rm/*.o: nm reads these
		// synchronously to build pasyms.h before realmode.elf links —
		// but same as empty.c/.o above, these are handled by a plain
		// Passthrough directly in compile.go's own isKbuildRealmodeObj,
		// not by this package, for the identical sandbox-mode reason.
		{"arch/x86/realmode/rm/header.S", Placeholder},
		{"arch/x86/realmode/rm/header.o", Placeholder},
		{"arch/x86/realmode/rm/trampoline_32.S", Placeholder},
		{"arch/x86/realmode/rm/trampoline_64.o", Placeholder},
		{"arch/x86/realmode/rm/stack.o", Placeholder},
		{"arch/x86/realmode/rm/reboot.o", Placeholder},
		{"/build/linux-6.12/arch/x86/realmode/rm/reboot.o", Placeholder},

		// Ordinary sources defer.
		{"main.c", Placeholder},
		{"src/util.cpp", Placeholder},
		{"parseutils.c", Placeholder},

		// Build-time codegen tools are indistinguishable by name from
		// any other binary — llvm-tblgen vs llvm-config. They defer, and
		// builds that exec one mid-build use a phase split instead. See
		// the package doc comment.
		{"llvm-tblgen", Placeholder},
		{"llvm-min-tblgen", Placeholder},
		{"protoc", Placeholder},

		// Near-misses that must NOT realise.
		{"testing.c", Placeholder},                           // "test" prefix, no "Compiler"
		{"Checkers.c", Placeholder},                          // "Check" prefix, no known suffix
		{"CMakeLists.txt", Placeholder},                      // "CMake" prefix, no "Compiler"
		{"my-conftest-helper.c", Placeholder},                // conftest not at the start
		{"empty.c", Placeholder},                             // bare "empty.c" outside scripts/mod/ is an ordinary TU
		{"scripts/mod/modpost.c", Placeholder},               // sibling in the same dir, not the probe file itself
		{"arch/x86/realmode/rm/realmode.lds.S", Placeholder}, // in the dir, but not a realmode-y member
		{"arch/x86/kernel/head_32.S", Placeholder},           // outside the realmode/rm/ dir entirely
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := For(tc.path); got != tc.want {
				t.Errorf("For(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestForLink pins the link-shim-only realise carveout — a narrower,
// separate function from For (see For's own docstring on why), added
// for Linux Kbuild link outputs an unshimmed tool reads back
// synchronously in the same recursive make: arch/x86/tools/relocs on
// realmode.elf, checkundef.sh's own `nm` call (plus a later
// objcopy/readelf pass) on vdso32.so.dbg/vdsox32.so.dbg, later
// Makefile rules exec-ing scripts/mod/modpost directly, objcopy's own
// -j .modinfo extraction from vmlinux.o, and link-vmlinux.sh's own
// nm/sorttable calls on the final vmlinux binary. Every OTHER link
// output must stay Placeholder, or the whole point of deferring
// (parallelism, batching into one `nixgg force`) is lost for ordinary
// binaries that nothing reads back synchronously.
func TestForLink(t *testing.T) {
	for _, tc := range []struct {
		path string
		want Mode
	}{
		{"arch/x86/realmode/rm/realmode.elf", Realise},
		{"/build/linux-6.12/arch/x86/realmode/rm/realmode.elf", Realise},
		{"arch/x86/entry/vdso/vdso32.so.dbg", Realise},
		{"arch/x86/entry/vdso/vdsox32.so.dbg", Realise},
		{"/build/linux-6.12/arch/x86/entry/vdso/vdso32.so.dbg", Realise},
		{"scripts/mod/modpost", Realise},
		{"/build/linux-6.12/scripts/mod/modpost", Realise},
		{"vmlinux.o", Realise},
		{"/build/linux-6.12/vmlinux.o", Realise},
		{"vmlinux", Realise},
		{"/build/linux-6.12/vmlinux", Realise},

		// Near-misses and ordinary links must NOT realise.
		{"realmode.elf", Placeholder}, // right basename, wrong directory
		{"arch/x86/realmode/rm/realmode.bin", Placeholder},
		{"arch/x86/entry/vdso/vdso32.so", Placeholder}, // the FINAL .so, not the .so.dbg
		{"scripts/mod/modpost.o", Placeholder},         // an object, not the linked tool itself
		{"foo-vmlinux.o", Placeholder},                 // right suffix, wrong basename
		{"foo-vmlinux", Placeholder},                   // right suffix, wrong basename
		{"a.out", Placeholder},
		{"conftest", Placeholder}, // link-style probes go through NIXGG_BYPASS, not this carveout
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := ForLink(tc.path); got != tc.want {
				t.Errorf("ForLink(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
