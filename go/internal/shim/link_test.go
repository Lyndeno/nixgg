package shim

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tbereknyei/nixgg/internal/batchpending"
	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// TestParseLinkArgs pins the link-line parser: which tokens are inputs
// (objects/archives), which stay as flags, and which are dropped.
func TestParseLinkArgs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantOut    string
		wantInputs []string
		wantFlags  []string
		wantOK     bool
	}{
		{
			name:       "separated -o",
			args:       []string{"a.o", "b.o", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o", "b.o"},
			wantOK:     true,
		},
		{
			name:       "attached -o",
			args:       []string{"a.o", "-oprog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantOK:     true,
		},
		{
			name:       "archives are inputs too",
			args:       []string{"main.o", "libfoo.a", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"main.o", "libfoo.a"},
			wantOK:     true,
		},
		{
			name:       "-l flags stay flags when unresolvable",
			args:       []string{"a.o", "-lm", "-ldl", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantFlags:  []string{"-lm", "-ldl"},
			wantOK:     true,
		},
		{
			// -M* families are dep-file generation; meaningless in a link
			// thunk and they target paths outside the sandbox.
			name:       "dep-file flags dropped",
			args:       []string{"a.o", "-MD", "-MF", "dep.d", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantOK:     true,
		},
		{
			// CMake 4 emits this so ninja can track link deps; it makes ld
			// WRITE to a build-tree-relative path that doesn't exist in the
			// link drv's sandbox ("cannot open dependency file .../link.d").
			name:       "-Wl,--dependency-file= dropped (attached)",
			args:       []string{"a.o", "-Wl,--dependency-file=x/link.d", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantOK:     true,
		},
		{
			name:       "-Wl,--dependency-file dropped (separated)",
			args:       []string{"a.o", "-Wl,--dependency-file", "x/link.d", "-o", "prog"},
			wantOut:    "prog",
			wantInputs: []string{"a.o"},
			wantOK:     true,
		},
		{
			name:   "no inputs is not a link we model",
			args:   []string{"-o", "prog"},
			wantOK: false,
		},
		{
			name:   "no -o is not a link we model",
			args:   []string{"a.o"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, inputs, flags, _, ok := parseLinkArgs(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if out != tc.wantOut {
				t.Errorf("output = %q, want %q", out, tc.wantOut)
			}
			if !reflect.DeepEqual(inputs, tc.wantInputs) {
				t.Errorf("inputs = %q, want %q", inputs, tc.wantInputs)
			}
			if tc.wantFlags != nil && !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
			for _, f := range flags {
				if strings.Contains(f, "dependency-file") {
					t.Errorf("dependency-file survived into flags: %q", flags)
				}
				if strings.HasPrefix(f, "-M") {
					t.Errorf("dep-file flag survived into flags: %q", flags)
				}
			}
		})
	}
}

// TestResolveLibFlagOnlyClaimsOurArtifacts pins that `-l<name>` is
// promoted to an explicit input ONLY when the matching lib<name>.a in a
// -L dir is something nixgg produced. A vendored or system .a that we
// did not build must stay a `-l` flag so the linker resolves it normally.
//
// resolveLibFlag recognizes two markers: a symlink (native mode's thunk
// pointer) and a regular file starting with the drvref magic header
// (sandbox mode, since builder-rpc-v0 doesn't materialise .drv files
// into the sandbox so a symlink would dangle).
func TestResolveLibFlagOnlyClaimsOurArtifacts(t *testing.T) {
	dir := t.TempDir()

	// A drvref stub: what the archive shim writes in sandbox mode.
	stub := filepath.Join(dir, "libours.a")
	body := drvref.Body("/nix/store/" + strings.Repeat("a", 32) + "-ar-libours.a.drv")
	if err := os.WriteFile(stub, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// A plain archive we did not produce.
	foreign := filepath.Join(dir, "libforeign.a")
	if err := os.WriteFile(foreign, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveLibFlag("ours", []string{dir}); got != stub {
		t.Errorf("drvref stub not claimed: got %q, want %q", got, stub)
	}
	if got := resolveLibFlag("foreign", []string{dir}); got != "" {
		t.Errorf("foreign archive wrongly claimed as ours: %q — it would be "+
			"referenced as a drv input that does not exist", got)
	}
	if got := resolveLibFlag("absent", []string{dir}); got != "" {
		t.Errorf("nonexistent lib claimed: %q", got)
	}
}

// TestResolveLibFlagClaimsBatchPendingArtifacts is a regression test
// for a real gap found while implementing batch archives: a -lfoo
// resolving to a still-deferred batch member's own stub (see
// batchpending.Is) must be claimed the same way a drvref stub is —
// otherwise resolveLibFlag returns "" and the caller leaves a bare
// -lfoo flag, which resolves to nothing inside the sandbox even
// though the file exists and nixgg knows exactly what produced it.
func TestResolveLibFlagClaimsBatchPendingArtifacts(t *testing.T) {
	dir := t.TempDir()

	pending := filepath.Join(dir, "libbatched.a")
	recordPath := filepath.Join(dir, "record.json")
	if err := os.WriteFile(pending, []byte(batchpending.Body(recordPath)), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := resolveLibFlag("batched", []string{dir}); got != pending {
		t.Errorf("batch-pending stub not claimed: got %q, want %q", got, pending)
	}
}

// TestIsLinkInput pins which link-line tokens are files the linker
// consumes. Getting this wrong is silently wrong, not loudly wrong: an
// unrecognized token is filed under `flags` by parseLinkArgs and baked
// into the drv as a bare relative path that does not exist in the
// sandbox. Recognizing a token is what routes it through
// classify.Target, whose Regular/Absent verdicts trigger Passthrough.
func TestIsLinkInput(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"main.o", true},
		{"libfoo.a", true},
		{"mod.xo", true},   // redis's PIC objects for test modules
		{"thing.lo", true}, // libtool
		{"MAIN.O", true},   // ext match is case-insensitive
		{"sub/dir/main.o", true},

		// Shared libraries, plain and versioned. filepath.Ext reports
		// ".2" for the last one, which is why this needs its own check.
		{"libfoo.so", true},
		{"libfoo.so.1", true},
		{"libfoo.so.1.2", true},
		{"libfoo.so.1.2.3", true},
		{"/abs/path/libfoo.so.1", true},

		// Flags are never inputs. Without the leading-dash guard,
		// `-l:libexact.a` has filepath.Ext ".a" and is mistaken for an
		// archive — classify.Target then stats a file literally named
		// "-l:libexact.a", gets Absent, and passes through. Safe, but
		// only by accident; resolveLibFlag is the correct handler.
		{"-l:libexact.a", false},
		{"-lfoo", false},
		{"-o", false},
		{"-Wl,--as-needed", false},
		{"-L/usr/lib", false},

		// Not libraries despite superficial resemblance.
		{"libfoo.solid", false},
		{"libfoo.so.1.x", false}, // non-numeric version segment
		{"libfoo.so.", false},    // empty trailing segment
		{"notes.txt", false},
		{"main.c", false},
		{"", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := isLinkInput(tc.in); got != tc.want {
				t.Errorf("isLinkInput(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestLinkerScriptPath pins which link-line flag shapes are
// recognized as naming a caller-local linker script (see
// linkerScriptPath's own docstring for why these can never be
// accelerated), and which superficially similar flags are not.
func TestLinkerScriptPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"version-script attached", []string{"-Wl,--version-script=libcrypto.ld"}, "libcrypto.ld"},
		{"version-script build-tree path", []string{"main.o", "-Wl,--version-script=engines/afalg.ld", "-o", "afalg.so"}, "engines/afalg.ld"},
		{"-Wl,-T comma form", []string{"-Wl,-T,script.ld"}, "script.ld"},
		{"separate -T", []string{"-T", "script.ld"}, "script.ld"},
		{"attached -Tscript.ld", []string{"-Tscript.ld"}, "script.ld"},
		{"-Ttext= is an address, not a script", []string{"-Ttext=0x1000"}, ""},
		{"-Tdata= is an address, not a script", []string{"-Tdata=0x2000"}, ""},
		{"-Tbss= is an address, not a script", []string{"-Tbss=0x3000"}, ""},
		{"no linker script flag at all", []string{"main.o", "-o", "prog"}, ""},
		{"trailing -T with no value", []string{"main.o", "-T"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkerScriptPath(tc.args); got != tc.want {
				t.Errorf("linkerScriptPath(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestParseLinkArgsSharedLibIsAnInput pins that a positional shared
// library reaches `inputs`, not `flags`. As a flag it would be baked
// into the drv as a bare path with no corresponding staged file.
func TestParseLinkArgsSharedLibIsAnInput(t *testing.T) {
	_, inputs, flags, _, ok := parseLinkArgs(
		[]string{"main.o", "libfoo.so", "libbar.so.1.2", "-o", "prog"})
	if !ok {
		t.Fatal("parseLinkArgs returned !ok")
	}
	want := []string{"main.o", "libfoo.so", "libbar.so.1.2"}
	if !reflect.DeepEqual(inputs, want) {
		t.Errorf("inputs = %q, want %q", inputs, want)
	}
	for _, f := range flags {
		if strings.Contains(f, ".so") {
			t.Errorf("shared lib landed in flags (%q) — it would be baked into "+
				"the drv as a path that does not exist in the sandbox", flags)
		}
	}
}

// TestResolveLibFlagExactNameForm pins `-l:libfoo.a`, the spelling build
// systems use to pin a static archive when a shared one also exists (ld
// takes the name literally instead of expanding lib…/.a).
func TestResolveLibFlagExactNameForm(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "libexact.a")
	body := drvref.Body("/nix/store/" + strings.Repeat("a", 32) + "-ar-libexact.a.drv")
	if err := os.WriteFile(stub, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// A foreign archive under the exact-name form must NOT be claimed.
	foreign := filepath.Join(dir, "libforeign.a")
	if err := os.WriteFile(foreign, []byte("!<arch>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		arg  string
		want string
	}{
		{"exact name resolves to our stub", ":libexact.a", stub},
		{"plain name still works", "exact", stub},
		{"foreign archive not claimed", ":libforeign.a", ""},
		{"absent file", ":libnope.a", ""},
		{"bare -l: is not a filename", ":", ""},
		{"path separator is not a plain filename", ":sub/libexact.a", ""},
		{"empty name", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLibFlag(tc.arg, []string{dir}); got != tc.want {
				t.Errorf("resolveLibFlag(%q) = %q, want %q", tc.arg, got, tc.want)
			}
		})
	}
}

// TestStoreInputPreservesSubpath guards the composition step that caused a
// real LLVM link failure:
//
//	ld.bfd: cannot find /nix/store/…-zlib-1.3.2/libz.so
//
// LLVM's cmake puts an absolute positional shared library on the link
// line. Classification correctly reduces it to the store root — that is
// what builtins.storePath and inputs.srcs require — and the shim then
// composes the argv token as Ref+"/"+Name. Using the caller-visible
// basename for Name drops the intervening "lib/", producing a path that
// does not exist.
//
// This is a distinct failure from the one link_test already covers:
// isSharedLib correctly RECOGNISED libz.so as an input (that part worked).
// The bug was one layer later, in what path got written for it — which is
// why the earlier tests passed while a real build broke.
func TestStoreInputPreservesSubpath(t *testing.T) {
	const root = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-zlib-1.3.2"

	t.Run("subpath input keeps its directories", func(t *testing.T) {
		c := classify.Result{Kind: classify.Store, Ref: root, Sub: "lib/libz.so"}
		ni, ji := storeInput(c, "/build/llvm/libz.so")

		if ni.Ref != root {
			t.Errorf("native Ref = %q, want the store root %q", ni.Ref, root)
		}
		if ni.Name != "lib/libz.so" {
			t.Errorf("native Name = %q, want \"lib/libz.so\" — the serializers render\n"+
				"Ref+\"/\"+Name, so a bare basename links against a nonexistent file",
				ni.Name)
		}
		if ji.Name != "lib/libz.so" {
			t.Errorf("sandbox Name = %q, want \"lib/libz.so\"", ji.Name)
		}
		// inputs.srcs takes a basename, and must stay the ROOT's basename:
		// the sandbox mounts the whole store object, not one file in it.
		if want := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-zlib-1.3.2"; ji.Ref != want {
			t.Errorf("sandbox Ref = %q, want %q", ji.Ref, want)
		}
	})

	t.Run("direct child falls back to the basename", func(t *testing.T) {
		// Everything nixgg produces has this shape, and all 81 pinned drvs
		// depend on it: an empty Sub must yield exactly the old behavior.
		c := classify.Result{Kind: classify.Store, Ref: root, Sub: ""}
		ni, ji := storeInput(c, "/build/obj/main.o")
		if ni.Name != "main.o" {
			t.Errorf("native Name = %q, want \"main.o\"", ni.Name)
		}
		if ji.Name != "main.o" {
			t.Errorf("sandbox Name = %q, want \"main.o\"", ji.Name)
		}
	})

	t.Run("Sub wins over the caller-visible name", func(t *testing.T) {
		// A Makefile can reference a library through a differently-named
		// symlink. Sub describes where the bytes actually are, so it must
		// take precedence over what the caller called it.
		c := classify.Result{Kind: classify.Store, Ref: root, Sub: "lib/libz.so.1.3.2"}
		ni, _ := storeInput(c, "/build/deps/libz.so")
		if ni.Name != "lib/libz.so.1.3.2" {
			t.Errorf("native Name = %q, want the resolved Sub, not the caller's name", ni.Name)
		}
	})
}

// TestClassifyInputsSonameAliasUsesRealOutputName pins the end-to-end
// path for openssl's engines/*.so links, which reference libcrypto via
// the plain `ln -s libcrypto.so.3 libcrypto.so` alias openssl's own
// Makefile creates — not through -lcrypto. classify.Target resolves
// the alias and reports the referenced drv's REAL output basename via
// Sub; classifyInputs must use that, not the caller's own alias
// basename, or the emitted link line reaches for
// "<drv-out>/bin/libcrypto.so" — a file that never exists, since the
// drv's own output is named libcrypto.so.3 — and ld fails with
// "cannot find ...: No such file or directory".
func TestClassifyInputsSonameAliasUsesRealOutputName(t *testing.T) {
	drv := "/nix/store/" + strings.Repeat("a", 32) + "-bin-libcrypto.so.3.drv"

	dir := t.TempDir()
	real := filepath.Join(dir, "libcrypto.so.3")
	if err := os.WriteFile(real, []byte(drvref.Body(drv)), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "libcrypto.so")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}

	_, jsonInputs, err, ok := classifyInputs(&toolchain.Config{}, []string{alias}, "", paths.Layout{}, "link", func() error {
		t.Fatal("should not passthrough — the alias resolves to one of our own drvref stubs")
		return nil
	})
	if err != nil || !ok {
		t.Fatalf("classifyInputs failed: ok=%v err=%v", ok, err)
	}
	if len(jsonInputs) != 1 {
		t.Fatalf("got %d jsonInputs, want 1", len(jsonInputs))
	}
	if jsonInputs[0].Ref != drv {
		t.Errorf("Ref = %q, want %q", jsonInputs[0].Ref, drv)
	}
	if jsonInputs[0].Name != "libcrypto.so.3" {
		t.Errorf("Name = %q, want %q (the drv's real output name, not the alias %q)",
			jsonInputs[0].Name, "libcrypto.so.3", filepath.Base(alias))
	}
}

// TestResolveLibFlagFindsSharedLibs pins that `-lfoo` can resolve to a
// nixgg-produced libfoo.so, not only libfoo.a.
//
// The candidate list was hardcoded to lib<name>.a, so a project linking
// its own shared library the ordinary way — `-L. -lfoo` against a
// libfoo.so nixgg had just built — never matched. The flag stayed a bare
// `-lfoo` with no staged input, and the link died at
// `ld: cannot find -lfoo`.
//
// This is the third defect in this same area: 28cd274 taught the shim to
// recognise a positional .so, a later fix stopped dropping the /lib/
// subdirectory of a consumed .so, and the `-l` search path still only
// knew about archives. Worth stating so the next reader checks all three
// representations when touching library handling.
//
// Search order is load-bearing and verified against the real linker:
// ld tries lib<name>.so before lib<name>.a in each -L directory and takes
// the first hit. Claiming the archive first would silently link something
// different from what the caller's toolchain would have chosen.
func TestResolveLibFlagFindsSharedLibs(t *testing.T) {
	t.Run("plain -lfoo resolves a .so thunk", func(t *testing.T) {
		dir := t.TempDir()
		so := filepath.Join(dir, "libfoo.so")
		// Native mode marks an unrealised output with a symlink.
		if err := os.Symlink(filepath.Join(dir, "whatever.nix"), so); err != nil {
			t.Fatal(err)
		}
		if got := resolveLibFlag("foo", []string{dir}); got != so {
			t.Errorf("resolveLibFlag(\"foo\") = %q, want %q — a self-built shared "+
				"library is invisible to -l resolution, so the link fails at "+
				"`ld: cannot find -lfoo`", got, so)
		}
	})

	t.Run("plain -lfoo resolves a .so drvref stub", func(t *testing.T) {
		dir := t.TempDir()
		so := filepath.Join(dir, "libfoo.so")
		if err := os.WriteFile(so, []byte(drvref.Body("/nix/store/"+
			strings.Repeat("a", 32)+"-bin-libfoo.so.drv")), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveLibFlag("foo", []string{dir}); got != so {
			t.Errorf("sandbox-mode .so stub not resolved: got %q, want %q", got, so)
		}
	})

	t.Run("shared library wins over an archive in the same dir", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{"libfoo.so", "libfoo.a"} {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(drvref.Body(
				"/nix/store/"+strings.Repeat("b", 32)+"-x.drv")), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want := filepath.Join(dir, "libfoo.so")
		if got := resolveLibFlag("foo", []string{dir}); got != want {
			t.Errorf("resolveLibFlag = %q, want %q — ld searches .so first, so "+
				"claiming the archive links something the caller's toolchain "+
				"would not have chosen", got, want)
		}
	})

	t.Run("archive still resolves when there is no .so", func(t *testing.T) {
		dir := t.TempDir()
		a := filepath.Join(dir, "libfoo.a")
		if err := os.WriteFile(a, []byte(drvref.Body("/nix/store/"+
			strings.Repeat("c", 32)+"-ar-libfoo.a.drv")), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveLibFlag("foo", []string{dir}); got != a {
			t.Errorf("archive resolution regressed: got %q, want %q", got, a)
		}
	})

	t.Run("a foreign .so is still not claimed", func(t *testing.T) {
		// A system library that nixgg did not produce has neither a
		// thunk symlink nor a drvref header. Claiming it would put a
		// path in the drv that the sandbox never staged.
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "libfoo.so"),
			[]byte("\x7fELF not ours"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveLibFlag("foo", []string{dir}); got != "" {
			t.Errorf("resolveLibFlag claimed a foreign .so: %q", got)
		}
	})
}

// TestParseLinkArgsPreservesArchiveGroup pins that a linker archive group
// survives the input/flag separation.
//
// parseLinkArgs sorts tokens into inputs and flags, and buildScript emits
// all flags before all inputs. Group brackets are positional — they
// bracket whatever sits BETWEEN them — so the pair ended up adjacent in
// flags, spanning nothing:
//
//	caller:  cc m.o -Wl,--start-group libb.a liba.a -Wl,--end-group -o p
//	emitted: cc -Wl,--start-group -Wl,--end-group m.o libb.a liba.a -o p
//
// That silently defeats ld's multi-pass rescan. Reproduced against the
// real linker with two mutually-recursive archives: with the group it
// links, without it fails with `undefined reference to b_fn`.
//
// The fix records that a group was asked for and re-emits it around the
// whole input list. That widens the span — objects end up inside the
// group, which is harmless (also verified against ld) — because the
// caller's exact span is no longer expressible once inputs and flags have
// been separated.
func TestParseLinkArgsPreservesArchiveGroup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantGroup  bool
		wantInputs []string
		wantFlags  []string
	}{
		{
			name: "long spelling",
			args: []string{"m.o", "-Wl,--start-group", "libb.a", "liba.a",
				"-Wl,--end-group", "-o", "prog"},
			wantGroup:  true,
			wantInputs: []string{"m.o", "libb.a", "liba.a"},
			wantFlags:  nil,
		},
		{
			// ld accepts -Wl,-( / -Wl,-) as equivalent; verified.
			name: "paren spelling",
			args: []string{"m.o", "-Wl,-(", "libb.a", "liba.a", "-Wl,-)",
				"-o", "prog"},
			wantGroup:  true,
			wantInputs: []string{"m.o", "libb.a", "liba.a"},
			wantFlags:  nil,
		},
		{
			name:       "bare spelling, no -Wl prefix",
			args:       []string{"m.o", "--start-group", "liba.a", "--end-group", "-o", "prog"},
			wantGroup:  true,
			wantInputs: []string{"m.o", "liba.a"},
			wantFlags:  nil,
		},
		{
			name:       "no group leaves the flag list untouched",
			args:       []string{"m.o", "liba.a", "-O2", "-o", "prog"},
			wantGroup:  false,
			wantInputs: []string{"m.o", "liba.a"},
			wantFlags:  []string{"-O2"},
		},
		{
			name: "other flags still pass through alongside a group",
			args: []string{"-O2", "m.o", "-Wl,--start-group", "liba.a",
				"-Wl,--end-group", "-Wl,-E", "-o", "prog"},
			wantGroup:  true,
			wantInputs: []string{"m.o", "liba.a"},
			wantFlags:  []string{"-O2", "-Wl,-E"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, inputs, flags, group, ok := parseLinkArgs(tc.args)
			if !ok {
				t.Fatalf("parseLinkArgs failed on %q", tc.args)
			}
			if group != tc.wantGroup {
				t.Errorf("group = %v, want %v", group, tc.wantGroup)
			}
			if !reflect.DeepEqual(inputs, tc.wantInputs) {
				t.Errorf("inputs = %q, want %q", inputs, tc.wantInputs)
			}
			if !reflect.DeepEqual(flags, tc.wantFlags) {
				t.Errorf("flags = %q, want %q", flags, tc.wantFlags)
			}
			// The brackets must not survive in flags: that is the bug.
			for _, f := range flags {
				if isGroupBracket(f) {
					t.Errorf("group bracket %q left in flags — it would be emitted "+
						"before the inputs and span nothing", f)
				}
			}
		})
	}
}

// TestStoreInputPromotedArtifactKeepsItsSubdir guards the second half of
// the FHS change, which the first half's test did not cover.
//
// storeInput uses classify.Result.Sub when it has one. It does not have
// one for our OWN promoted outputs: `force` copies a realised artifact
// into the working tree as a real file, and the promoted registry records
// only the store ROOT — so Sub is empty and the artifact's FHS subdir has
// to be re-derived from its name.
//
// Missing that broke native-mode lua: liblua.a is at <root>/lib/liblua.a
// but was referenced as <root>/liblua.a, so luac failed with
//
//	ld.bfd: cannot find …-ar-liblua.a/liblua.a: No such file or directory
//
// Note this is NOT the same case as the earlier zlib subpath bug. There,
// Sub was correctly populated from a resolved symlink and the fix was to
// stop discarding it. Here Sub is legitimately empty and the subdir must
// be reconstructed. Two different causes, same symptom.
func TestStoreInputPromotedArtifactKeepsItsSubdir(t *testing.T) {
	const root = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-ar-liblua.a"

	t.Run("promoted archive gets lib/", func(t *testing.T) {
		// Sub empty: this is a promoted output, not a resolved symlink.
		c := classify.Result{Kind: classify.Store, Ref: root}
		ni, ji := storeInput(c, "/build/lua/src/liblua.a")
		if ni.Name != "lib/liblua.a" {
			t.Errorf("native Name = %q, want \"lib/liblua.a\" — the archive drv "+
				"wrote it under lib/, so a flat reference cannot resolve", ni.Name)
		}
		if ji.Name != "lib/liblua.a" {
			t.Errorf("sandbox Name = %q, want \"lib/liblua.a\"", ji.Name)
		}
	})

	t.Run("promoted object stays flat", func(t *testing.T) {
		c := classify.Result{Kind: classify.Store,
			Ref: "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-tu-main.o"}
		ni, _ := storeInput(c, "/build/obj/main.o")
		if ni.Name != "main.o" {
			t.Errorf("Name = %q, want \"main.o\" — compile outputs are flat", ni.Name)
		}
	})

	t.Run("an explicit Sub still wins", func(t *testing.T) {
		// A foreign dependency reached through a symlink: classify resolved
		// the real position, and that must take precedence over any rule
		// inferred from the filename.
		c := classify.Result{Kind: classify.Store,
			Ref: "/nix/store/cccccccccccccccccccccccccccccccc-zlib-1.3",
			Sub: "lib/libz.so",
		}
		ni, _ := storeInput(c, "/build/deps/libz.so")
		if ni.Name != "lib/libz.so" {
			t.Errorf("Name = %q, want the resolved Sub", ni.Name)
		}
	})
}
