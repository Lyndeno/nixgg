// Package cli implements the nixgg CLI. It's invoked when the binary
// is called as "nixgg" (or via `nix run .#nixgg -- run ...`), not
// through a shim symlink.
package cli

import (
	"fmt"
	"os"
)

// Main is the CLI entrypoint. args excludes argv[0].
//
// The CLI is deliberately small. Day-to-day, users don't invoke nixgg
// as a wrapper — they source `eval $(nixgg env)`, set
// NIXGG_AUTOFORCE=1, and run plain `make`. Shims do the rest.
//
//	env    — print the shell fragment that sets up NIXGG_* and PATH.
//	force  — escape hatch: materialise thunks left in the working tree
//	         after a build that didn't have NIXGG_AUTOFORCE=1 set.
func Main(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("missing subcommand")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "force":
		return cmdForce(rest)
	case "env":
		return cmdEnv(rest)
	case "assemble":
		return cmdAssemble(rest)
	case "helper":
		return cmdHelper(rest)
	case "-h", "--help", "help":
		usage()
		return nil
	}
	usage()
	return fmt.Errorf("unknown subcommand %q", sub)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: nixgg <subcommand> [args…]

  env    [--store URL] [--print-only]
         Print shell env: NIXGG_ROOT, PATH, toolchain roots. Source
         with `+"`eval $(nixgg env)`"+`.

  force  [--thunks-dir DIR] [--roots] [target…]
         Materialise thunks. Only needed if you built without
         NIXGG_AUTOFORCE=1 (the link shim's inline realise hook).

  assemble <root> <name>
         Sandbox-mode only. Walk <root> for drvref stubs left by shim
         calls during a whole-tree build (dynDrvStdenv's phase 1),
         build one drv that restores the tree and resolves every
         stub, and submit it as this derivation's "out" output.

  helper --socket PATH [--remote URL] [--pool-size N]
         Optional persistent relay for internal/rpc's three sandbox
         ops (see NIXGG_RPC_HELPER). Not for interactive use.

The usual flow doesn't call nixgg at all after env:

  eval "$(nixgg env)"
  export NIXGG_AUTOFORCE=1
  make -j$(nproc)
`)
}
