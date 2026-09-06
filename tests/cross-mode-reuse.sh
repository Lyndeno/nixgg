#!/usr/bin/env bash
# Behavioral test: a drv built by hand in `nix develop` is a real
# substitute for the same drv in a fully sandboxed `nix build` — not
# just structurally identical (tests/drv-equivalence.sh already pins
# that), but actually reused, with zero rebuild, at the point a pure
# build needs it.
#
# tests/drv-equivalence.sh proves the SET of native-mode and
# sandbox-mode drv hashes match. It never builds anything through
# BOTH paths in the same store, so it can't tell "these two modes
# would produce the same drv" apart from "a real substitution event
# happens when one mode's build follows the other's" — the actual
# property ARCHITECTURE.md's "Corollary: dev-shell and pure-build
# derivations are interchangeable" describes. This script builds that
# missing link:
#
#   1. Fresh alt store. `nix develop .#lua-shell`, by hand, compile
#      exactly 2 of lua's 32 TUs (lapi.o, lauxlib.o) with the real
#      Makefile's own CC/flags — nothing else in the package is built
#      yet, sandbox mode included.
#   2. Force those 2 thunks into real derivations (what
#      NIXGG_AUTOFORCE=1 or `nixgg force` would do automatically for a
#      real link step; done by hand here since a 2-object partial
#      build never links).
#   3. Run a full, ordinary `nix build .#lua`, substituters disabled
#      (so a remote build-trace match can't quietly explain an absent
#      "building" line — same reasoning as tests/perf-regression.sh).
#   4. Assert the 2 drv paths native mode just built are NOT among
#      this run's "building '...'" lines (they were substituted, not
#      recompiled) AND that they ARE referenced by some drv the
#      sandbox build actually produced (liblua.a's own inputDrvs) —
#      both halves matter: "not in the building log" alone is also
#      what you'd see if the two modes' drv hashes had DIVERGED (the
#      exact regression this test exists to catch), since a
#      never-referenced path is trivially never built either. The
#      other 30 TUs genuinely built this run is the negative control
#      (without it, a stale/broken store that made EVERYTHING look
#      cached would pass for the wrong reason).
#
# Env knobs (same names tests/drv-equivalence.sh already uses):
#   ALT_STORE      root of the alt store (default /tmp/nixgg-cross-mode-store)
#   PATCHED_NIX    path to a builder-rpc-v0-capable nix
#                  (default: ./.patched-nix, built from flake if missing)
#   KEEP_STORE=1   don't wipe ALT_STORE at start (for local iteration)

set -uo pipefail

# shellcheck source=lib/drv-equiv-common.sh
source "$(cd "$(dirname "$0")" && pwd)/lib/drv-equiv-common.sh"

equiv_common_setup "/tmp/nixgg-cross-mode-store"

attr="lua"
src_input="lua-src"

src=$(equiv_resolve_native_src "$src_input") || true
if [[ -z "$src" || ! -e "$src" ]]; then
  echo "could not resolve native src for $attr (input=$src_input)" >&2
  exit 1
fi

workdir="$(mktemp -d)"
cp -a "$src"/. "$workdir/"
chmod -R u+w "$workdir"
rm -rf "$workdir/.nixgg" 2>/dev/null || true

# Partial native build: hand-compile 2 of lua's 32 TUs with the exact
# CC/flags `make linux` uses (SYSCFLAGS=-DLUA_USE_LINUX — see
# src/Makefile's own `linux:` target), but naming the 2 objects
# directly instead of running the full `make linux` — the point is to
# simulate "developer edited 2 files, ran make", not to build the
# whole package by hand.
nt_log="/tmp/nixgg-cross-mode-native.log"
printf '==> native: nix develop .#%s-shell, hand-compile lapi.o lauxlib.o\n' "$attr"
(
  cd "$workdir/src"
  "$PATCHED_NIX/bin/nix" develop "$nixgg_root#$attr-shell" --command bash -c "
    export NIXGG_STORE='local?root=$ALT_STORE'
    export NIXGG_AUTOFORCE=0
    set -euo pipefail
    make CC=cc MYCFLAGS=-DLUA_USE_LINUX lapi.o lauxlib.o
  "
) > "$nt_log" 2>&1 || {
  echo "native partial build failed; see $nt_log:" >&2
  tail -20 "$nt_log" >&2
  rm -rf "$workdir"
  exit 1
}

thunk_files=$(equiv_collect_thunks "$workdir")
if [[ -z "$thunk_files" ]]; then
  echo "native partial build produced no thunks; see $nt_log" >&2
  rm -rf "$workdir"
  exit 1
fi

# Force each thunk into a real derivation in the alt store — a real
# `nixgg force` / NIXGG_AUTOFORCE=1's inline-link-realise hook would
# do this automatically once something links against the object; done
# by hand here since a bare `.o` compile never triggers either.
native_drvs=""
force_log="/tmp/nixgg-cross-mode-force.log"
while IFS= read -r t; do
  "$PATCHED_NIX/bin/nix" build --no-eval-cache --impure --file "$t" \
    >>"$force_log" 2>&1 || {
      echo "forcing thunk $t failed; see $force_log" >&2
      rm -rf "$workdir"
      exit 1
    }
  native_drvs="$native_drvs $(equiv_thunk_drvpath "$t")"
done <<<"$thunk_files"
rm -rf "$workdir"

echo "  native-built (nix develop + hand-run make, no sandbox):"
printf '    %s\n' $native_drvs

# Full, ordinary sandboxed build. Substituters disabled: the only way
# a "building '...'" line can be absent for one of our 2 drv paths is
# that it already exists in THIS store from the native step above —
# not a lucky remote binary-cache hit (mirrors tests/perf-regression.sh's
# own reasoning for the same flag).
sb_log="/tmp/nixgg-cross-mode-sandbox.log"
printf '==> sandbox: nix build .#%s (substituters disabled)\n' "$attr"
"$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link -Lv \
  --option substituters "" \
  "$nixgg_root#$attr" > "$sb_log" 2>&1 || {
    echo "sandbox build failed; see $sb_log:" >&2
    tail -20 "$sb_log" >&2
    exit 1
  }

built_this_run=$(grep -oP "(?<=^building ')[^']*\.drv(?=')" "$sb_log" | xargs -r -n1 basename | sort -u)

ok=1

# "Not in the building log" alone is ambiguous: a drv-hash mismatch
# between native and sandbox mode (the exact regression this test
# exists to catch) would ALSO make our 2 native-built paths absent
# from the sandbox build's log — they'd just be irrelevant, never
# referenced at all, not silently substituted. Confirm the positive
# side too: something the sandbox build actually produced (liblua.a,
# or the final link) must reference each native-built drv path in its
# OWN inputDrvs — i.e. the sandbox build's real dependency graph, not
# just this run's build log, names the exact path native mode built.
for b in $native_drvs; do
  if grep -qxF "$b" <<<"$built_this_run"; then
    printf '\033[1;31m  FAIL\033[0m %s was rebuilt by the sandboxed build — cross-mode reuse broken\n' "$b" >&2
    ok=0
    continue
  fi
  if ! grep -lqF "$b" "$ALT_STORE"/nix/store/*.drv 2>/dev/null; then
    printf '\033[1;31m  FAIL\033[0m %s is not referenced by ANY drv the sandbox build produced — its hash diverged from what sandbox mode wanted, so "not rebuilt" above means "irrelevant", not "substituted"\n' "$b" >&2
    ok=0
  fi
done

# Negative control: the OTHER TUs (30 of lua's 32) must have genuinely
# built this run. Without this check, a store/setup bug that made
# EVERY drv look pre-existing (e.g. a stale ALT_STORE bleeding in from
# a prior run) would pass the loop above for the wrong reason.
other_tu_count=$(grep -c '^[a-z0-9]\+-tu-.*\.o\.drv$' <<<"$built_this_run" || true)
if [[ "$other_tu_count" -lt 25 ]]; then
  printf '\033[1;31m  FAIL\033[0m expected the sandboxed build to actually compile most of lua'"'"'s TUs (>=25), only %d building lines seen — is ALT_STORE stale?\n' \
    "$other_tu_count" >&2
  ok=0
fi

if [[ "$ok" != "1" ]]; then
  echo "cross-mode reuse verification failed."
  exit 1
fi
printf '\033[1;32m  PASS\033[0m %d TUs hand-built via nix develop were substituted, not rebuilt; %d others built normally\n' \
  "$(printf '%s\n' $native_drvs | grep -c .)" "$other_tu_count"
