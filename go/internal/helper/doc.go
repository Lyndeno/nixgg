// Package helper implements an optional, opt-in local relay for
// internal/rpc's three sandbox ops (DerivationAdd, StoreAddScan,
// SubmitOutput). Exists to amortize the real Nix daemon's own
// handshake cost — measured at ~4.3ms per Dial(), 99% of the current
// per-call RPC cost, versus ~23µs for an op on an already-open
// connection — across every shim invocation in a build, not just
// within one.
//
// Without this package, internal/rpc.Dial (and therefore
// sandbox.DerivationAdd/StoreAddScan/SubmitOutput under NIXGG_RPC=1)
// opens a fresh connection to the real Nix daemon per call — already
// an improvement over fork+exec'ing the CLI, but still paying a full
// handshake per shim invocation (twice per compile: once for
// StoreAddScan, once for DerivationAdd — sandbox.go calls dialRPC()
// independently in each).
//
// This package's helper process holds a small POOL of already-
// handshaken daemon connections (see Pool's own docstring for why a
// single shared connection would serialize a `make -j` build's
// concurrent shim calls — confirmed against the real Nix C++ client,
// which pools connections for the same reason) and relays ops from
// many short-lived shim processes over a lightweight local protocol
// whose own per-call cost (~162µs for a bare socket round trip,
// measured) is negligible next to what it replaces.
//
// A third, independently opt-in layer on top of NIXGG_RPC: setting
// NIXGG_RPC_HELPER=<socket-path> makes sandbox.go relay through the
// helper instead of dialing the daemon directly; NIXGG_RPC=1 with no
// helper var still dials directly, exactly as before. Nothing about
// internal/rpc's own wire-protocol code changes — the helper is a
// client of internal/rpc like any other caller, just a long-lived one
// serving many short-lived peers.
package helper
