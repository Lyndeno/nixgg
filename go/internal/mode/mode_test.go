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
		{"testing.c", Placeholder},            // "test" prefix, no "Compiler"
		{"Checkers.c", Placeholder},           // "Check" prefix, no known suffix
		{"CMakeLists.txt", Placeholder},       // "CMake" prefix, no "Compiler"
		{"my-conftest-helper.c", Placeholder}, // conftest not at the start
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := For(tc.path); got != tc.want {
				t.Errorf("For(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// The kernel's lib/test_fortify/ compiles code that must NOT compile —
// kbuild expects the error, captures stderr to a .log, and greps the
// object for __write_overflow. Accelerating those turns an expected
// diagnostic into a hard build failure, and Realise is no better since
// `nix build` on a failing compile fails too.
func TestForFortifyTestsPassThrough(t *testing.T) {
	for _, p := range []string{
		"../lib/test_fortify/write_overflow-memcpy.c",
		"lib/test_fortify/read_overflow-memcpy.c",
		"/build/linux-6.18.41/lib/test_fortify/write_overflow_field-memset.c",
	} {
		if got := For(p); got != Passthrough {
			t.Errorf("For(%q) = %v, want Passthrough", p, got)
		}
	}

	// A path merely mentioning fortify is not the carveout: only the
	// directory kbuild puts these in.
	for _, p := range []string{
		"lib/string_helpers.c",
		"include/linux/fortify-string.h",
		"lib/fortify_helpers.c",
	} {
		if got := For(p); got != Placeholder {
			t.Errorf("For(%q) = %v, want Placeholder", p, got)
		}
	}
}

// arch/x86/realmode/rm generates a header by piping `nm` output through
// sed. That consumes objects and produces TEXT, so there is no artifact
// to model and the answer is needed inline — the only correct handling
// is to let the real compiler produce real objects.
func TestForRealmodePassesThrough(t *testing.T) {
	for _, p := range []string{
		"../arch/x86/realmode/rm/header.S",
		"arch/x86/realmode/rm/trampoline_64.S",
		"/build/linux-6.18.41/arch/x86/realmode/rm/wakemain.c",
	} {
		if got := For(p); got != Passthrough {
			t.Errorf("For(%q) = %v, want Passthrough", p, got)
		}
	}
	// arch/x86/boot/startup is NOT in the carveout: its objcopy step is
	// modelled, and its nm-using siblings act on the linked image at a
	// later stage.
	if got := For("../arch/x86/boot/startup/gdt_idt.c"); got != Placeholder {
		t.Errorf("boot/startup = %v, want Placeholder", got)
	}
}

// libstub and the vDSO join realmode for the same underlying reason:
// their per-object rules read object bytes inline. libstub's
// cmd_stubcopy greps objdump output to decide whether to fail the
// build; the vDSO does a full `ld -T <script>` link, which shim.LD
// does not model.
func TestForInlineInspectionDirsPassThrough(t *testing.T) {
	for _, p := range []string{
		"../drivers/firmware/efi/libstub/alignedmem.c",
		"drivers/firmware/efi/libstub/efi-stub-helper.c",
		"../arch/x86/entry/vdso/vdso-note.S",
		"arch/x86/entry/vdso/vclock_gettime.c",
	} {
		if got := For(p); got != Passthrough {
			t.Errorf("For(%q) = %v, want Passthrough", p, got)
		}
	}
	// Neighbouring directories must stay accelerated — the carveouts are
	// specific subtrees, not whole subsystems.
	for _, p := range []string{
		"../drivers/firmware/efi/efi.c",
		"../arch/x86/entry/common.c",
	} {
		if got := For(p); got != Placeholder {
			t.Errorf("For(%q) = %v, want Placeholder", p, got)
		}
	}
}
