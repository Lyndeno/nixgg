// Package sandbox holds sandbox-mode helpers: submitting JSON drvs to
// the outer nix daemon via `nix derivation add`, looking up drv paths
// for previously-produced outputs.
//
// This mode is selected by NIXGG_SANDBOX=1 and is only meaningful
// when nixgg is running inside a builder-rpc-v0 derivation (see
// nixgg/dyn-drv/NOTES.md).
package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tbereknyei/nixgg/internal/drvref"
	"github.com/tbereknyei/nixgg/internal/expr"
	"github.com/tbereknyei/nixgg/internal/toolchain"
)

// Enabled reports whether NIXGG_SANDBOX=1 is set.
func Enabled() bool {
	return os.Getenv("NIXGG_SANDBOX") == "1"
}

// DerivationAdd pipes a JSON drv description to `nix derivation add`
// and returns the resulting drv store path. --offline: nix should
// have zero business contacting substituters for a `derivation add`
// (it's registering a drv description, not resolving inputs), but
// under some sandbox configurations it tries anyway and stalls on
// name-resolution failures. Cheap paranoia.
func DerivationAdd(cfg *toolchain.Config, drv expr.JSONDrv) (string, error) {
	body, err := json.Marshal(drv)
	if err != nil {
		return "", fmt.Errorf("encode drv json: %w", err)
	}
	cmd := exec.Command(cfg.Nix, "--offline", "derivation", "add")
	cmd.Env = os.Environ()
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix derivation add %s: %w\n%s", drv.Name, err, stderr.String())
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("nix derivation add: empty output")
	}
	return path, nil
}

// StoreAddScan uploads a directory via `nix store add --scan -n name
// path` and returns the resulting store path. --scan makes the
// daemon scan the tree for references to already-present store
// objects and record them — required inside a sandbox where
// unregistered references cause build-time errors.
func StoreAddScan(cfg *toolchain.Config, name, path string) (string, error) {
	cmd := exec.Command(cfg.Nix, "--offline", "store", "add", "--scan", "-n", StoreName(name), path)
	cmd.Env = os.Environ()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix store add %s: %w\n%s", path, err, stderr.String())
	}
	sp := strings.TrimSpace(string(out))
	if sp == "" {
		return "", fmt.Errorf("nix store add: empty output")
	}
	return sp, nil
}

// SubmitOutput registers a .drv path as the currently-running outer
// derivation's named output. Only valid inside a builder-rpc-v0
// sandbox.
//
// Nix requires the submitted path's *basename* to match
// `outputPathName(outerDrvName, outputName)`. mkNixggBuild names the
// outer drv "bin-<target>.drv" precisely so our inner link drv
// (also "bin-<target>.drv") satisfies this without a rename step.
// If a rename is ever required, the mechanism would be: re-upload
// the drv ATerm bytes via `nix store add --mode text -n <canonical>`.
// That in turn requires access to the bytes — inside builder-rpc-v0
// the .drv file isn't materialised on disk, so the caller must
// capture bytes at submission time. See nix-ninja
// crates/nix-builder-rpc-client for the daemon-RPC approach.
func SubmitOutput(cfg *toolchain.Config, drvPath, outputName string) error {
	cmd := exec.Command(cfg.Nix, "--offline", "store", "submit-output", drvPath, outputName)
	cmd.Env = os.Environ()
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("nix store submit-output %s %s: %w", drvPath, outputName, err)
	}
	return nil
}

// PointOutputAtDrv writes a drvref stub at `output`, recording which
// drv produced the artifact that would otherwise live there. It is the
// sandbox-mode analogue of native mode's .nix-thunk symlink.
//
// See internal/drvref for the format and for why it is a regular file
// rather than a symlink.
func PointOutputAtDrv(output, drvPath string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	_ = os.Remove(output)
	body := drvref.Body(drvPath)
	if err := os.WriteFile(output, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write drvref %s -> %s: %w", output, drvPath, err)
	}
	return nil
}
