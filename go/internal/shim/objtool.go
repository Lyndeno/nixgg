package shim

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/classify"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/paths"
	"github.com/tbereknyei/nixgg/internal/sandbox"
	"github.com/tbereknyei/nixgg/internal/storedeps"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Objtool is the shim entrypoint for kbuild's `objtool <flags> foo.o`.
//
// WHY THIS EXISTS
//
// kbuild rewrites objects in place right after compiling them
// (scripts/Makefile.lib: `cmd_objtool = … ; $(objtool) $(objtool-args)
// $@`). Under nixgg the object is a drvref stub at that moment, so the
// real objtool fails with "gelf_getehdr failed: invalid `Elf' handle".
//
// With delay-objtool (CONFIG_X86_KERNEL_IBT or LTO) built-in objects
// escape this — objtool runs once on vmlinux.o instead — but
// SINGLE-OBJECT MODULES do not: Makefile.build's
// `objtool-enabled = $(if $(is-standard-object),$(if $(delay-objtool),
// $(is-single-obj-m),y))` still selects them per-TU, and a modular
// kernel has thousands.
//
// So model it as its own derivation: take the compile's drv as input,
// copy its object, rewrite the copy, and leave a fresh stub behind.
// Downstream ar/link shims already resolve stubs, so nothing else has
// to change.
//
// Reached only when the caller points kbuild's `objtool=` make variable
// at our shim — nixpkgs passes absolute tool paths specifically to
// defeat PATH interposition, so PATH alone would never route here.
func Objtool(args []string, cfg *toolchain.Config, l paths.Layout) error {
	real := os.Getenv("NIXGG_REAL_OBJTOOL")
	if bypassed() || !sandbox.Enabled() || real == "" {
		// Native mode has no transform helper, and without the real
		// binary there is nothing to delegate to. Say so when it is the
		// missing binary rather than a deliberate bypass: silently
		// running nothing would corrupt the object.
		if real == "" && !bypassed() {
			logf("objtool passthrough: NIXGG_REAL_OBJTOOL unset")
		}
		return Passthrough(objtoolFallback(real), args)
	}

	flags, object, ok := parseObjtoolArgs(args)
	if !ok {
		logf("objtool passthrough: no object operand (%s)", joinBase(args))
		return Passthrough(objtoolFallback(real), args)
	}

	// The object must be something we can name as a derivation input.
	// A stub names its producing drv; an already-realised store path is
	// equally fine. Anything else (a plain file the shims never made)
	// we cannot model, and passing through would rewrite a file that no
	// derivation knows about.
	t := classify.Target(object, altStorePrefix(cfg.Store), l)
	var in expr.JSONDrvInput
	switch t.Kind {
	case classify.Drv:
		in = expr.JSONDrvInput{Kind: "drv", Ref: t.Ref, Name: filepath.Base(object)}
	case classify.Store:
		in = expr.JSONDrvInput{Kind: "src", Ref: expr.StoreBasename(t.Ref), Name: filepath.Base(object)}
	default:
		logf("objtool passthrough: can't model input %s (%s)", object, t.Kind)
		return Passthrough(objtoolFallback(real), args)
	}

	logf("objtool %s", object)

	toolStore, err := storeAddTool(cfg, "objtool", real)
	if err != nil {
		return err
	}

	outName := filepath.Base(object)
	drv := expr.TransformJSON(expr.TransformJSONParams{
		Name:        "ot-" + outName,
		OutName:     outName,
		System:      cfg.System,
		Bash:        cfg.BashRoot,
		Coreutils:   cfg.CoreutilsRoot,
		ToolBin:     toolStore,
		Flags:       flags,
		Input:       in,
		StoreDeps:   storedeps.From(nil, "", cfg.KnownStorePaths),
		Placeholder: "/" + expr.OutPlaceholderNix32,
		ExtraSrcs: []string{
			baseNameOf(cfg.BashRoot),
			baseNameOf(cfg.CoreutilsRoot),
			baseNameOf(toolStore),
		},
	})
	drvPath, err := sandbox.DerivationAdd(cfg, drv)
	if err != nil {
		return err
	}
	if err := sandbox.PointOutputAtDrv(object, drvPath); err != nil {
		return err
	}
	logf("  drv:        %s", drvPath)
	return nil
}

// parseObjtoolArgs splits objtool's argv into flags and the object it
// operates on.
//
// objtool's grammar is `objtool <actions/options...> file.o` — the
// object is the sole non-flag operand and comes last. Its options are
// either bare long flags or `--opt=value` (see its own usage output),
// so none of them consume a following token, which keeps this parse
// honest without enumerating them.
func parseObjtoolArgs(args []string) (flags []string, object string, ok bool) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			continue
		}
		if object != "" {
			// A second operand means a shape we do not model.
			return nil, "", false
		}
		object = a
	}
	if object == "" {
		return nil, "", false
	}
	return flags, object, true
}

// storeAddTool puts a binary the wrapped project built itself into the
// store so a derivation can depend on it.
//
// kbuild compiles objtool during `make prepare`, inside the very
// sandbox we are running in, so it is an ordinary file rather than a
// store path. `nix store add` fixes that, and because the binary links
// only against store paths (elfutils/glibc/zlib/zstd) the scan records
// correct references.
//
// The result is memoised in a file beside the build root: every object
// in the build asks for the same binary, and a fork+exec per object
// would cost more than the compile derivations it is protecting.
func storeAddTool(cfg *toolchain.Config, name, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cacheDir := os.Getenv("NIX_BUILD_TOP")
	cache := ""
	if cacheDir != "" {
		// Key on the tool's own path: a build may store-add more than
		// one (objtool today, `ld` for single-object modules next).
		cache = filepath.Join(cacheDir,
			fmt.Sprintf(".gg-tool-%s-%x", name, sha256.Sum256([]byte(abs))))
		if b, err := os.ReadFile(cache); err == nil {
			if sp := strings.TrimSpace(string(b)); sp != "" {
				return sp, nil
			}
		}
	}
	sp, err := sandbox.StoreAddScan(cfg, name, abs)
	if err != nil {
		return "", fmt.Errorf("store-add %s: %w", name, err)
	}
	if cache != "" {
		// Best-effort: a failed write costs a re-add, not correctness.
		_ = os.WriteFile(cache, []byte(sp), 0o644)
	}
	return sp, nil
}

// objtoolFallback picks what Passthrough should exec. Prefer the real
// binary the caller told us about; otherwise fall back to the name so
// PATH resolution produces a recognisable "not found" rather than an
// empty-argv panic.
func objtoolFallback(real string) string {
	if real != "" {
		return real
	}
	return "objtool"
}
