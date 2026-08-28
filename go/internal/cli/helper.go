// Package cli's helper subcommand — see internal/helper for the
// actual server/pool/protocol implementation. This file just wires
// flags to it.
package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/tbereknyei/nixgg/internal/helper"
)

// cmdHelper runs the persistent daemon-side relay in the foreground.
// Callers background it (`nixgg helper ... &`) from a build's
// preBuild hook and send SIGTERM from postBuild — see
// mkNixggBuild.nix's own comment on NIXGG_RPC_HELPER for the exact
// shape. Not meant to be run interactively.
func cmdHelper(args []string) error {
	var socketPath, remote string
	poolSize := 4

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--socket":
			i++
			if i >= len(args) {
				return fmt.Errorf("--socket needs a value")
			}
			socketPath = args[i]
		case "--remote":
			i++
			if i >= len(args) {
				return fmt.Errorf("--remote needs a value")
			}
			remote = args[i]
		case "--pool-size":
			i++
			if i >= len(args) {
				return fmt.Errorf("--pool-size needs a value")
			}
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("--pool-size: %w", err)
			}
			poolSize = n
		case "-h", "--help":
			helperUsage()
			return nil
		default:
			helperUsage()
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if socketPath == "" {
		return fmt.Errorf("--socket is required")
	}
	if remote == "" {
		remote = os.Getenv("NIX_REMOTE")
	}
	if remote == "" {
		return fmt.Errorf("--remote is required (or set NIX_REMOTE)")
	}

	srv, err := helper.Listen(socketPath, remote, poolSize)
	if err != nil {
		return err
	}

	// Signal a caller can wait for: once the socket exists and is
	// accepting, print one line to stdout. preBuild backgrounds this
	// process and should poll for the socket file (or read this
	// line) before letting buildPhase's shims start — a shim racing
	// the helper's own bind would otherwise get "connection refused"
	// on the very first compile.
	fmt.Println("ready")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	done := make(chan struct{})
	go func() {
		srv.Serve()
		close(done)
	}()

	<-sig
	srv.Shutdown()
	<-done
	_ = os.Remove(socketPath)
	return nil
}

func helperUsage() {
	fmt.Fprint(os.Stderr, `usage: nixgg helper --socket PATH [--remote unix://...] [--pool-size N]

Runs the persistent daemon-side relay in the foreground (see
internal/helper). Not for interactive use — meant to be started from
a sandboxed build's preBuild hook and stopped via SIGTERM from
postBuild. Prints "ready" on stdout once its socket is accepting.

  --socket PATH     where to listen (shims connect here via
                     NIXGG_RPC_HELPER)
  --remote URL      the real Nix daemon's own NIX_REMOTE; defaults to
                     $NIX_REMOTE if unset
  --pool-size N     max concurrent daemon connections (default 4;
                     should track NIX_BUILD_CORES)
`)
}
