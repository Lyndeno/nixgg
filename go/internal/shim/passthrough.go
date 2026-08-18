package shim

import (
	"os"
	"syscall"
)

// Passthrough replaces the current process image with the real tool.
// This is the right thing for shims that decide not to model a call:
// we don't pay for a fork, stdin/stdout/stderr are already correct,
// and the caller sees the tool's true exit code.
func Passthrough(realTool string, args []string) error {
	argv := append([]string{realTool}, args...)
	return syscall.Exec(realTool, argv, os.Environ())
}

// bypassed reports whether the shim should skip nixgg's derivation
// path and just exec the real tool. Controlled by NIXGG_BYPASS.
//
// Intended for phases that can't route through nixgg without
// breaking — most commonly autoconf `./configure` or cmake's
// probe phase, which compile+exec tiny binaries synchronously.
// Users flip it around those phases:
//
//	NIXGG_BYPASS=1 ./configure
//	make      # NIXGG_BYPASS unset → shims fire as normal
//
// Any non-empty, non-"0" value is truthy.
func bypassed() bool {
	v := os.Getenv("NIXGG_BYPASS")
	return v != "" && v != "0"
}
