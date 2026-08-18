// Package shim implements the shim entry points invoked when make (or
// any other build tool with our shims on PATH) executes cc/c++/ar.
//
// Every shim's job is: parse argv, build a Nix expression describing
// what should be produced, write that as a thunk, and symlink the
// output to the thunk. It never calls `nix build` (except for the
// autoconf-conftest carveout). All realisation happens later in
// `nixgg force`.
package shim

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/activitylog"
	"github.com/tbereknyei/nixgg/internal/dispatch"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/mode"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/scan"
	"github.com/tbereknyei/nixgg/internal/stage"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/thunk"
	"github.com/tbereknyei/nixgg/internal/toolchain"
	"github.com/tbereknyei/nixgg/internal/wrapperenv"
)

// realToolFor picks the sibling binary matching the caller's argv[0]
// role from the same bin/ dir NIXGG_REAL_CC points at. Nix's
// gcc-wrapper has `cc → gcc` (C mode) and `c++ → g++` (C++ mode) —
// invoking g++ on a `.c` source triggers cc-wrapper's isCxx=1 self-
// check (see wrapper's `bin/gcc = *++` test) and breaks C-only build
// systems. `cc` → `gcc`, `c++` → `g++`, unknown → the pinned RealCC.
func realToolFor(cfg *toolchain.Config, tool dispatch.Tool) string {
	base := tool.Basename()
	if base == "" {
		return cfg.RealCC
	}
	return filepath.Join(filepath.Dir(cfg.RealCC), base)
}

// Compile is the shim entrypoint for `cc -c ...`. It parses argv,
// stages source + headers, writes a thunk, and symlinks the output.
//
// tool is the caller's argv[0] role (cc / gcc / c++ / g++) — that name
// is what gets baked into the derivation's compile command, so
// `cc -c foo.c` produces a "cc" invocation inside the sandbox, not g++.
func Compile(tool dispatch.Tool, args []string, cfg *toolchain.Config, l paths.Layout) error {
	// Passthrough targets the sibling binary matching argv[0]:
	// cc→cc/gcc (C mode), c++→c++/g++ (C++ mode). Using
	// NIXGG_REAL_CC blindly would send `.c` compiles through g++,
	// which cc-wrapper's line-30 self-check maps to C++ mode and
	// breaks C-only build systems (redis's deps/hiredis: alloc.c
	// under g++ fails `-std=c99` + designated-initializer parsing).
	realTool := realToolFor(cfg, tool)
	if bypassed() {
		// No logf here: bypass mode exists for configure/cmake probes
		// that capture stderr byte-for-byte (autoconf's
		// ac_fn_c_check_header_preproc treats ANY non-empty stderr
		// from `gcc -E` as a failed check, exit code notwithstanding).
		// A "[nixgg] ..." diagnostic line here was silently flipping
		// HAVE_LIMITS_H/HAVE_FCNTL_H/etc. to "no" for every libiberty
		// probe even though gcc exited 0 — Passthrough's own contract
		// is that stdin/stdout/stderr stay untouched; logging here
		// broke that contract for exactly the callers that most need
		// it honored.
		return Passthrough(realTool, args)
	}
	source, output, flags, ok := parseCompileArgs(args)
	if !ok {
		// Not a single-TU compile; execv the real cc and hope. Say so:
		// otherwise a build where nothing is accelerated is
		// indistinguishable from one where everything is.
		logf("compile passthrough: not a single-TU compile (%s)", joinBase(args))
		activitylog.Emit("compile", "passthrough", activitylog.Fields{"argv": args})
		return Passthrough(realTool, args)
	}

	// Fill in a default output name if -o was omitted.
	if output == "" {
		output = defaultOutputName(source, flags)
	}

	logf("compile %s -> %s", source, output)

	// Resolve the real cc for scan-headers to match the caller's tool
	// role — same reason as the passthrough case above.
	scannerCC := realTool

	// 1. Discover headers.
	scanResult, err := scan.Run(l, scannerCC, source, flags)
	if err != nil {
		return err
	}

	// The dep-generation flags were stripped, so nothing else will write
	// this file, and some build systems run a tool over it and hard-fail
	// without it. scan already resolved the exact header set.
	if dep := requestedDepFile(args); dep != "" {
		if err := writeDepFile(dep, output, source, scanResult.Headers); err != nil {
			return err
		}
	}

	// 2. Stage source + headers into .nixgg/srcs/<tu-id>/.
	srcAbs, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	// The source's staged relpath is its position under the project
	// root — the same layout every header uses. This matches the bash
	// driver: sources aren't special.
	srcRel, err := filepath.Rel(scanResult.ProjectRoot, srcAbs)
	if err != nil {
		return err
	}

	// Opt-in batch classification (see internal/batch's package
	// docstring for the mechanism). Deferred to step 5 below, after
	// mode.For(source) is known — a conftest/cmake-probe TU
	// (mode.Realise) must never be deferred, since it needs a
	// synchronous, real build right now.
	//
	// Classify sees srcAbs (the TU's absolute path), NOT srcRel:
	// srcRel is relative to scanResult.ProjectRoot, which is
	// recomputed per compile call (see scan.go) and can collapse down
	// to a TU's own directory when nothing widens it — confirmed
	// directly against a real redis build, where compiling from
	// inside deps/hiredis/ made srcRel just "sds.c", never matching
	// "deps/**/*.c". Classify's own unanchored search only works if
	// it's given the real, full path to search within.
	batchGroup, batched := cfg.BatchGroups.Classify(srcAbs)

	entries := make([]stage.Entry, 0, 1+len(scanResult.Headers))
	entries = append(entries, stage.Entry{Abs: srcAbs, Rel: srcRel})
	for _, h := range scanResult.Headers {
		entries = append(entries, stage.Entry{Abs: h.Abs, Rel: h.Rel})
	}
	// The tu_id must uniquely identify this compile across the whole
	// project so cross-directory calls with the same output basename
	// (redis src/sds.o vs deps/hiredis/sds.o) don't collide on the
	// staging dir. Feed TUID the output path *relative to the
	// workspace root* — that captures the project-local ambiguity
	// without leaking the absolute path prefix into the drv hash.
	// Absolute prefixes differ between native (user's cwd) and
	// sandbox (/build/work) mode even for identical source; the
	// relative path is the same in both.
	absOut, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	tuKey := absOut
	if rel, err := filepath.Rel(scanResult.ProjectRoot, absOut); err == nil && !strings.HasPrefix(rel, "..") {
		tuKey = rel
	}
	tuID := stage.TUID(tuKey)
	if _, err := stage.Sources(l, tuID, entries); err != nil {
		return err
	}

	// 3. Assemble the sandbox flag list. Strip the caller's -I family
	// (both attached and separated forms) then re-add our staged -I
	// flags (relative to project root) and any store-prefixed -I flags
	// verbatim.
	sandboxFlags := rewriteFlags(flags, scanResult.StagedIFlags, scanResult.StoreIFlags,
		scanResult.StagedIncludeFlags)

	// 4. Build the expression.
	wrapperEnvJSON, err := wrapperenv.JSON()
	if err != nil {
		return err
	}
	storeDeps := storedeps.From(sandboxFlags, wrapperEnvJSON, cfg.KnownStorePaths)
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}

	// srcTree is a Nix path literal referring to the staging dir.
	// Absolute so the thunk file survives `cp` to a peer directory —
	// see expr.Input.Ref docstring.
	srcTreeLiteral := filepath.Join(l.Srcs, tuID)

	e := expr.Compile(expr.CompileParams{
		Helpers:    cfg.Helpers,
		Tool:       tool.Basename(),
		SrcTree:    srcTreeLiteral,
		Source:     srcRel,
		OutName:    filepath.Base(output),
		Flags:      sandboxFlags,
		StoreDeps:  storeDeps,
		WrapperEnv: wrapperEnv,
	})

	// 5. Dispatch on mode.
	if mode.For(source) == mode.Realise {
		return realiseAndLink(e, output, cfg, l)
	}

	if batched {
		return deferCompileToBatch(cfg, l, batchGroup, tuID, tool.Basename(), output, srcRel,
			srcTreeLiteral, sandboxFlags, storeDeps, wrapperEnv)
	}

	// Sandbox mode: submit a JSON drv directly to the outer daemon,
	// symlink the output at the returned drv path. No .nix thunk on
	// disk. Downstream link/archive shims will resolve this via
	// classify.Drv and reference it in inputs.drvs.
	if sandbox.Enabled() {
		return compileSandbox(cfg, l, tool, tuID, filepath.Base(output), output, srcRel, sandboxFlags, storeDeps, wrapperEnvJSON)
	}

	thunkPath, err := submitCompileThunk(l, e, output)
	if err != nil {
		return err
	}
	logf("  thunk:      %s", thunkPath)
	activitylog.Emit("compile", "thunk", activitylog.Fields{
		"tool": tool.Basename(), "source": source, "output": output, "thunk": thunkPath,
	})
	return nil
}

// submitCompileThunk writes e's thunk, symlinks output at it, and
// records the symlink — native mode's per-TU submission, factored out
// so a later individually-resolved batch member (see
// ResolvePendingMember) can reach the identical code path Compile's
// own native branch uses, without duplicating it.
func submitCompileThunk(l paths.Layout, e, output string) (thunkPath string, err error) {
	id := thunk.Compute(e)
	thunkPath, err = thunk.Write(l, id, e)
	if err != nil {
		return "", err
	}
	if err := thunk.LinkPlaceholder(l, output, thunkPath); err != nil {
		return "", err
	}
	if err := thunk.RecordSymlink(l, id, output); err != nil {
		return "", err
	}
	return thunkPath, nil
}

// compileSandbox handles NIXGG_SANDBOX=1: emit a JSON drv describing
// this compile, hand it to `nix derivation add`, symlink the output
// at the returned drv path.
//
// The staged src tree lives at l.Srcs/<tuID> on disk. In sandbox
// mode we still write it there (via stage.Sources earlier), then
// upload it to the store via `nix store add --scan` so the resulting
// store path is a self-contained input. See #14.
func compileSandbox(
	cfg *toolchain.Config, l paths.Layout,
	tool dispatch.Tool, tuID, outName, output, srcRel string,
	flags []string, storeDeps []string, wrapperEnvJSON string,
) error {
	// Upload the staged src tree to the store. Use `tuID` as the
	// store-path name so this matches what native mode produces when
	// its .nix thunk gets instantiated — same content, same name,
	// same store path, same drv hash. See ARCHITECTURE.md on drv
	// equivalence between modes.
	srcStore, err := sandbox.StoreAddScan(cfg, tuID, filepath.Join(l.Srcs, tuID))
	if err != nil {
		return fmt.Errorf("stage src to store: %w", err)
	}
	wrapperEnv, err := decodeStringMap(wrapperEnvJSON)
	if err != nil {
		return err
	}
	return submitCompileSandboxDrv(cfg, tool.Basename(), outName, output, srcRel, srcStore, flags, storeDeps, wrapperEnv)
}

// submitCompileSandboxDrv is compileSandbox's logic from AFTER the
// src tree is already uploaded and wrapper env decoded — factored
// out so a resolved batch member (whose src tree was uploaded once,
// at defer time, by deferCompileToBatch, and whose WrapperEnv is
// already a decoded map in its MemberRecord) can reach the same
// drv-assembly/submission code without paying for a second,
// redundant upload of unchanged content or a pointless
// decode-then-reencode-then-redecode of the same map.
func submitCompileSandboxDrv(
	cfg *toolchain.Config, toolName, outName, output, srcRel, srcStore string,
	flags []string, storeDeps []string, wrapperEnv map[string]string,
) error {
	// Resolve toolchain roots for the drv. cfg carries the store
	// paths we bootstrapped from NIXGG_COMPILER_ROOT / _BASH_ROOT /
	// _COREUTILS_ROOT.
	bash := cfg.BashRoot
	coreutils := cfg.CoreutilsRoot
	compiler := cfg.CompilerRoot

	// Compute the drv's own $out placeholder. `builtins.placeholder
	// "out"` is sha256("nix-output:out") base32'd. Every derivation
	// gets the same value; the caOutputPlaceholder we compute for
	// referring downstream is different.
	outPlaceholder := "/" + expr.OutPlaceholderNix32

	// Assemble the JSON drv.
	drv := expr.CompileJSON(expr.CompileJSONParams{
		Name:        "tu-" + outName,
		OutName:     outName,
		System:      cfg.System,
		Bash:        bash,
		Coreutils:   coreutils,
		Compiler:    compiler,
		Tool:        toolName,
		SrcStore:    srcStore,
		Source:      srcRel,
		Flags:       flags,
		StoreDeps:   storeDeps,
		Placeholder: outPlaceholder,
		Srcs: []string{
			baseNameOf(bash),
			baseNameOf(coreutils),
			baseNameOf(compiler),
			baseNameOf(srcStore),
		},
		Env: wrapperEnv,
	})

	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(output, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s", drvPath)
	activitylog.Emit("compile", "drv", activitylog.Fields{
		"tool": toolName, "source": srcRel, "output": output, "drv": drvPath,
	})
	return nil
}

// decodeStringMap parses `{"K1": "V1", ...}` into a Go map.
func decodeStringMap(s string) (map[string]string, error) {
	if s == "" || s == "{}" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("decode wrapperEnv: %w", err)
	}
	return m, nil
}

// baseNameOf strips the /nix/store/ prefix so we get a hash+name
// basename suitable for a JSONDrv.Inputs.Srcs entry.
func baseNameOf(p string) string {
	return expr.StoreBasename(p)
}

// parseCompileArgs identifies the source + output + non-path flags.
// Returns ok=false if the invocation isn't a single-TU `-c` compile —
// in that case the caller passes through to the real cc.
func parseCompileArgs(args []string) (source, output string, flags []string, ok bool) {
	hasDashC := false
	// Non-empty once `-x <lang>` has been seen: from that point a
	// non-flag token is the source whatever its extension.
	explicitLang := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c":
			hasDashC = true
		case a == "-o":
			if i+1 >= len(args) {
				return "", "", nil, false
			}
			output = args[i+1]
			i++
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			output = a[2:]
		// Dep-file generation flags — drop them. They target paths
		// outside our sandbox (relative to make's cwd), so gcc inside
		// the derivation would try to open cachedObjs/… and fail.
		// scan-headers already gave us the header list we need.
		case a == "-M" || a == "-MM" || a == "-MG" || a == "-MP" || a == "-MD" || a == "-MMD":
			// no-op (single-arg forms)
		case a == "-MF" || a == "-MT" || a == "-MQ":
			// two-arg forms; skip the value too
			if i+1 >= len(args) {
				return "", "", nil, false
			}
			i++
		// Same flags, spelled as preprocessor passthrough. kbuild emits
		// `-Wp,-MMD,./.<obj>.o.d` on every compile; the path is relative
		// to make's cwd exactly like the bare forms above, and the
		// derivation's cwd is a read-only store path. scan.StripWpDep is
		// shared with the scanner so both agree on what counts as
		// dependency plumbing.
		case strings.HasPrefix(a, "-Wp,"):
			if kept, ok := scan.StripWpDep(a); ok {
				flags = append(flags, kept)
			}
		case a == "-x" || a == "-Xlinker" || a == "-Xassembler":
			// Two-arg forms with values that aren't sources; keep both.
			if i+1 >= len(args) {
				return "", "", nil, false
			}
			flags = append(flags, a, args[i+1])
			// `-x <lang>` overrides extension-based language detection,
			// so the source that follows need not have a known suffix.
			// The canonical case is a precompiled header:
			//
			//	g++ -x c++-header -c pch.h -o pch.h.gch
			//
			// isSource rejects .h, so without this the whole TU fell to
			// Passthrough — correct output, never cached or distributed.
			if a == "-x" {
				explicitLang = args[i+1]
			}
			i++
		case isSource(a):
			if source != "" {
				// Multiple sources — we don't model that in a single TU.
				return "", "", nil, false
			}
			source = a
		case explicitLang != "" && source == "" && !strings.HasPrefix(a, "-"):
			// A bare token after `-x <lang>`: the source, by the driver's
			// own rules, even though its extension says nothing.
			source = a
		default:
			flags = append(flags, a)
		}
	}
	if !hasDashC || source == "" {
		return "", "", nil, false
	}
	return source, output, flags, true
}

// isSource returns true if a token looks like a source file that the
// driver would compile into a single .o. Matches the bash driver's
// extension set.
func isSource(a string) bool {
	switch strings.ToLower(filepath.Ext(a)) {
	case ".c", ".cc", ".cpp", ".cxx", ".s":
		return true
	}
	// Uppercase .C, .S are also sources (C++ / preprocessed asm) — keep
	// case-sensitive check for those.
	ext := filepath.Ext(a)
	if ext == ".C" || ext == ".S" {
		return true
	}
	return false
}

// rewriteFlags produces the sandbox-flag list. Strip -I/-isystem/etc
// pairs (both forms) since our staged -I flags cover the same
// directories in the sandbox's layout; then append staged + store.
//
// `-include <file>` is handled separately via forceInc rather than being
// stripped: its value is a header to prepend to the TU, not a directory
// to search, so dropping it changes the preprocessor state the caller
// asked for. scan.StagedIncludeFlags supplies the re-pointed form.
// They go last so the -I flags they may resolve against are already in
// effect.
func rewriteFlags(caller, staged, store, forceInc []string) []string {
	pathFlags := map[string]bool{
		"-I": true, "-isystem": true, "-iquote": true,
		"-idirafter": true,
	}
	var out []string
	for i := 0; i < len(caller); i++ {
		a := caller[i]
		switch {
		case pathFlags[a]:
			if i+1 < len(caller) {
				i++
			}
			continue
		case strings.HasPrefix(a, "-I") && len(a) > 2:
			continue
		// Drop the caller's `-include <file>`; forceInc carries the
		// staged-relative replacement appended below.
		case a == "-include":
			if i+1 < len(caller) {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	out = append(out, staged...)
	out = append(out, store...)
	out = append(out, forceInc...)
	return out
}

// realiseAndLink is the (rare) realise-mode carveout: build the thunk
// synchronously via `nix build --file <tmp>.nix` and re-target the
// output symlink. Used for autoconf conftests and cmake probes.
func realiseAndLink(exprBody, output string, cfg *toolchain.Config, l paths.Layout) error {
	// Write to a tempfile alongside the real thunks so relative-path
	// imports resolve. Reuse the id-based path to keep the file if the
	// same expression comes back later.
	id := thunk.Compute(exprBody)
	thunkPath, err := thunk.Write(l, id, exprBody)
	if err != nil {
		return err
	}
	built, err := nixBuildFile(cfg, thunkPath)
	if err != nil {
		return err
	}
	// Re-point output at /nix/store/... (via the alt-store's on-disk
	// prefix if present).
	//
	// Flat basename is correct here, not an oversight: mode.Realise is
	// compile-only by design (see mode.For's docstring — link/archive
	// never reach this function), and compile outputs are the one Kind
	// FHS placement (expr.ArtifactSubdir) deliberately leaves flat. If
	// that ever changes, this needs the same expr.ArtifactSubdir lookup
	// storeInput and PromoteToStore already use — this is the third spot
	// that class of bug lives in, just currently inert.
	// See TestRealiseCarveoutOutputsAreAlwaysFlat for the pinned check.
	src := altStoreOnDisk(cfg.Store, built) + "/" + filepath.Base(output)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("expected %s after build: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	_ = os.Remove(output)
	if err := os.Symlink(src, output); err != nil {
		return err
	}
	if err := thunk.RecordSymlink(l, id, output); err != nil {
		return err
	}
	logf("  built:      %s", built)
	return nil
}

func nixBuildFile(cfg *toolchain.Config, thunkPath string) (string, error) {
	cmd := exec.Command(cfg.Nix, "build", "-L", "--no-link", "--print-out-paths", "--file", thunkPath)
	cmd.Env = append(os.Environ(),
		"NIX_REMOTE=",
		"NIX_CONFIG=experimental-features = nix-command flakes ca-derivations\nstore = "+cfg.Store+"\n",
	)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return "", fmt.Errorf("nix build --file %s: %w\n%s", thunkPath, err, stderr)
	}
	// Last non-empty line of stdout.
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] != "" {
			return lines[i], nil
		}
	}
	return "", fmt.Errorf("nix build returned no output")
}

// altStoreOnDisk maps a canonical /nix/store/... path to its on-disk
// location under the alt store's root (e.g. /tmp/nixgg-store/nix/store/...).
// Returns the path unchanged for a system store.
func altStoreOnDisk(storeURL, canonical string) string {
	// storeURL looks like "local?root=/tmp/nixgg-store" for alt stores.
	const prefix = "local?root="
	if strings.HasPrefix(storeURL, prefix) {
		return strings.TrimPrefix(storeURL, prefix) + canonical
	}
	return canonical
}

// logf emits a one-line `[nixgg]` diagnostic on stderr. Kept minimal
// so we don't clutter build output. Callers pass a format string as
// they would to fmt.Fprintf.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[nixgg]   "+format+"\n", args...)
}

// defaultOutputName is what the compiler would write to when -o is
// omitted. Two rules, and they differ in a way that is easy to get wrong:
//
//	a.cc  -> a.o          extension replaced
//	pch.h -> pch.h.gch    extension kept, .gch appended
//
// Verified against gcc by compiling a header with no -o.
func defaultOutputName(source string, flags []string) string {
	base := filepath.Base(source)
	if isHeaderLang(langOf(flags)) {
		return base + ".gch"
	}
	if dot := strings.LastIndexByte(base, '.'); dot > 0 {
		base = base[:dot]
	}
	return base + ".o"
}

// langOf returns the value of the last `-x <lang>` in flags, or "" if
// there is none. parseCompileArgs keeps both tokens, so the language the
// caller asked for is recoverable without widening its signature.
func langOf(flags []string) string {
	lang := ""
	for i := 0; i+1 < len(flags); i++ {
		if flags[i] == "-x" {
			lang = flags[i+1]
		}
	}
	return lang
}

// isHeaderLang reports whether a `-x <lang>` value names a header
// language, i.e. this compile produces a precompiled header rather than
// an object file. The output-naming rule differs: gcc appends .gch to the
// source's full name instead of replacing its extension with .o.
func isHeaderLang(lang string) bool {
	switch lang {
	case "c-header", "c++-header", "objective-c-header", "objective-c++-header":
		return true
	}
	return false
}

// requestedDepFile returns the dependency-file path the caller asked the
// compiler to write, or "" if it asked for none.
//
// Two spellings reach us. `-MF <path>` is a separate argv token; kbuild
// instead uses the preprocessor-passthrough form, where the filename is
// a comma element: `-Wp,-MMD,./.main.o.d`.
func requestedDepFile(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-MF" && i+1 < len(args) {
			return args[i+1]
		}
		if !strings.HasPrefix(a, "-Wp,") {
			continue
		}
		parts := strings.Split(a[len("-Wp,"):], ",")
		for j := 0; j < len(parts); j++ {
			switch parts[j] {
			case "-MMD", "-MD", "-MF":
				if j+1 < len(parts) {
					return parts[j+1]
				}
			}
		}
	}
	return ""
}

// writeDepFile emits a make-format dependency fragment for one TU.
//
// Plain `target: prereq…`: consumers only parse the target and its
// prerequisites. No -MP phony targets — they tolerate deleted headers,
// and a stale entry costs at most one extra rebuild.
func writeDepFile(path, output, source string, headers []scan.Header) error {
	var b strings.Builder
	b.WriteString(output)
	b.WriteString(": ")
	b.WriteString(source)
	for _, h := range headers {
		b.WriteString(" \\\n  ")
		b.WriteString(h.Abs)
	}
	b.WriteString("\n")

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("depfile dir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write depfile %s: %w", path, err)
	}
	return nil
}
