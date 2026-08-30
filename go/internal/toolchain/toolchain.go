// Package toolchain reads the NIXGG_* environment variables that identify
// the pinned toolchain (compiler, bash, coreutils, nix, helper store path).
//
// These are set once by the shell (via `nixgg env` or the flake's
// env-shell), then read by every shim invocation. A missing var is a hard
// error — we refuse to synthesize a compile expression with unresolved
// toolchain roots.
package toolchain

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/tbereknyei/nixgg/internal/batch"
)

// defaultSystem returns the Nix "system" string matching this Go
// binary's build target. Works for the two we care about now.
func defaultSystem() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "x86_64-linux"
	case "linux/arm64":
		return "aarch64-linux"
	case "darwin/amd64":
		return "x86_64-darwin"
	case "darwin/arm64":
		return "aarch64-darwin"
	}
	return runtime.GOARCH + "-" + runtime.GOOS
}

// Config holds the pinned nixgg toolchain roots.
type Config struct {
	// Absolute path to the real cc (usually gcc-wrapper/bin/g++ from
	// the flake). We only use this to derive the bin/ dir; the actual
	// tool name that goes into the Nix expression is the caller's
	// argv[0] (cc, gcc, c++, g++), so that a `cc` shim doesn't turn
	// into a g++ compilation inside the sandbox.
	RealCC string

	// /nix/store/…-gcc-wrapper-… — the toolchain root as it appears
	// inside the sandbox. Nix imports need this as a string.
	CompilerRoot  string
	BashRoot      string
	CoreutilsRoot string

	// /nix/store/…-nixgg-nix — the realised nix/ helper package.
	// Every thunk imports its {builder,linker,archiver}.nix from here.
	Helpers string

	// The nix binary to invoke on force. Not needed by the shim path.
	Nix string

	// The alt store URL (`local?root=…` or `auto`). Passed via
	// NIX_CONFIG when we invoke `nix build`.
	Store string

	// Nix "system" string for JSON drvs we emit in sandbox mode
	// (e.g. "x86_64-linux"). Defaults to $NIXGG_SYSTEM, else falls
	// back to a compile-time constant matching this build.
	System string

	// The full set of store paths mkNixggBuild.nix declared as inputs
	// (every output of buildInputs/propagatedBuildInputs plus the
	// toolchain roots — see knownStorePathInputs in mkNixggBuild.nix).
	// storedeps matches flag/env text against this list rather than
	// guessing at Nix's store-path grammar with a regex — see
	// storedeps.go. Populated from $NIXGG_KNOWN_STORE_PATHS, a JSON
	// array literal computed at eval time and exported identically by
	// both preBuild (sandbox) and shellHook (native) via
	// scrubWrapperEnv, so both modes see byte-identical input — that's
	// what keeps their drv hashes comparable. Empty, not an error, if
	// unset or unparseable — storedeps then simply finds nothing.
	KnownStorePaths []string

	// Opt-in batch-group definitions from $NIXGG_BATCH_GROUPS — see
	// internal/batch's own package docstring for the mechanism and
	// why it's prototype-scope (classification only; compile.go logs
	// which group a TU matched, but still submits one derivation per
	// TU). Zero value (no groups) if unset, same "absence is not an
	// error" convention as KnownStorePaths above.
	BatchGroups batch.Config
}

// FromEnv reads the NIXGG_* variables. Returns an error listing every
// missing var, so the caller can print one clear diagnostic instead of
// hitting a series of ENV panics.
func FromEnv() (*Config, error) {
	c := &Config{
		RealCC:        os.Getenv("NIXGG_REAL_CC"),
		CompilerRoot:  os.Getenv("NIXGG_COMPILER_ROOT"),
		BashRoot:      os.Getenv("NIXGG_BASH_ROOT"),
		CoreutilsRoot: os.Getenv("NIXGG_COREUTILS_ROOT"),
		Helpers:       os.Getenv("NIXGG_NIX_HELPERS"),
		Nix:           os.Getenv("NIXGG_NIX"),
		Store:         os.Getenv("NIXGG_STORE"),
		System:        os.Getenv("NIXGG_SYSTEM"),
	}
	if c.System == "" {
		c.System = defaultSystem()
	}
	c.KnownStorePaths = knownStorePathsFromEnv()
	c.BatchGroups = batch.FromJSON(os.Getenv("NIXGG_BATCH_GROUPS"))
	var missing []string
	if c.RealCC == "" {
		missing = append(missing, "NIXGG_REAL_CC")
	}
	if c.CompilerRoot == "" {
		missing = append(missing, "NIXGG_COMPILER_ROOT")
	}
	if c.BashRoot == "" {
		missing = append(missing, "NIXGG_BASH_ROOT")
	}
	if c.CoreutilsRoot == "" {
		missing = append(missing, "NIXGG_COREUTILS_ROOT")
	}
	if c.Helpers == "" {
		missing = append(missing, "NIXGG_NIX_HELPERS")
	}
	if c.Nix == "" {
		missing = append(missing, "NIXGG_NIX")
	}
	if c.Store == "" {
		missing = append(missing, "NIXGG_STORE")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing env: %v (run `nixgg env` to bootstrap)", missing)
	}
	return c, nil
}

// knownStorePathsFromEnv parses $NIXGG_KNOWN_STORE_PATHS, a JSON array
// of store path strings mkNixggBuild.nix exports identically in both
// modes (see scrubWrapperEnv there). Returns nil if unset or
// unparseable — never an error, since an empty manifest just means
// storedeps finds nothing.
func knownStorePathsFromEnv() []string {
	s := os.Getenv("NIXGG_KNOWN_STORE_PATHS")
	if s == "" {
		return nil
	}
	var paths []string
	if err := json.Unmarshal([]byte(s), &paths); err != nil {
		return nil
	}
	return paths
}
