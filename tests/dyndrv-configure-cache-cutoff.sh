#!/usr/bin/env bash
# Regression test: dynDrvConfigureCacheStdenv's early-cutoff actually
# holds — same property tests/configure-cache-cutoff.sh checks for
# configureCacheStdenv alone, now through the combined 3-group split.
#
# Builds group A (the *-configure-<version> derivation, same naming
# convention as configureCacheStdenv's own group A) three ways and
# compares its ggtree OUTPUT PATH:
#
#   baseline   — real, unedited src
#   excluded   — src edited at a file configureSrcFilter's
#                includePatterns/existenceStubs DON'T cover
#   included   — src edited at a file the filter DOES cover
#
# Early-cutoff means: excluded must produce the SAME ggtree path as
# baseline (the edit never reaches group A's actual input content,
# so CA collapses the rebuild back to the same output) — while
# included must produce a DIFFERENT one (a negative control: if this
# ALSO matched baseline, the filter would be excluding everything,
# not correctly discriminating).
#
# This only checks the caching *mechanism* — whether the resulting
# package builds/runs is tests/smoke.sh's DYNCONFIGCACHE set's job,
# not this script's. hello (single-output) and gdbm (multi-output:
# out/dev/info/lib/man) are both run through this — the combined
# mechanism's third fixture (zstd) has no configureSrcFilter to test
# cutoff against at all (see nix/dynDrvConfigureCacheStdenv.nix's
# flake.nix usage — zstd's own CMakeLists.txt globs its sources, so
# filtering can't preserve early-cutoff for it).
#
# Env knobs:
#   ALT_STORE      root of the alt store (default /tmp/nixgg-dyndrv-cutoff-store)
#   PATCHED_NIX    path to a builder-rpc-v0-capable nix
#                  (default: ./.patched-nix, built from flake if missing)
#   KEEP_STORE=1   don't wipe ALT_STORE at start (for local iteration)

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"
fixture_nix="$here/dyndrv-configure-cache-cutoff-fixture.nix"

ALT_STORE="${ALT_STORE:-/tmp/nixgg-dyndrv-cutoff-store}"
PATCHED_NIX="${PATCHED_NIX:-$nixgg_root/.patched-nix}"
if [[ ! -x "$PATCHED_NIX/bin/nix" ]]; then
  echo "==> building patched nix (one-time; substituted from cache)" >&2
  nix build --no-eval-cache "$nixgg_root#patched-nix" -o "$PATCHED_NIX" >&2 || exit 2
fi

if [[ "${KEEP_STORE:-}" != "1" ]]; then
  chmod -R u+w "$ALT_STORE" 2>/dev/null || true
  rm -rf "$ALT_STORE"
fi
mkdir -p "$ALT_STORE"

export NIX_REMOTE=""
export NIX_CONFIG="
experimental-features = nix-command flakes ca-derivations dynamic-derivations configurable-impure-env
extra-system-features = builder-rpc-v0
store = local?root=$ALT_STORE
"

# groupA_ggtree_path <package> <edit-arg>
#
# Instantiates the fixture with the given package/edit, extracts group
# A's own derivation (the one named *-configure-<version> — nested one
# level inside the outer drv's *.drv.drv input, since group B sits
# between the outer package derivation and group A here), builds ONLY
# its "ggtree" output, and prints the resulting real store path.
groupA_ggtree_path() {
  local package="$1" edit_arg="$2"
  local outer_drv group_b_drv group_a_drv

  outer_drv=$("$PATCHED_NIX/bin/nix-instantiate" --impure \
    --arg flakeDir "$nixgg_root" \
    --argstr package "$package" \
    ${edit_arg:+--argstr edit "$edit_arg"} \
    "$fixture_nix" 2>/tmp/nixgg-dyndrv-cutoff-instantiate.log) || {
      echo "  instantiate failed (package=$package edit=$edit_arg); see /tmp/nixgg-dyndrv-cutoff-instantiate.log" >&2
      tail -10 /tmp/nixgg-dyndrv-cutoff-instantiate.log >&2
      return 1
    }

  group_b_drv=$("$PATCHED_NIX/bin/nix" show-derivation "$outer_drv" 2>/dev/null \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
info = list(d['derivations'].values())[0]
matches = [k for k in info['inputs']['drvs'] if k.endswith('.drv.drv')]
print(matches[0] if matches else '')
")
  if [[ -z "$group_b_drv" ]]; then
    echo "  could not find group B derivation (package=$package edit=$edit_arg)" >&2
    return 1
  fi

  group_a_drv=$("$PATCHED_NIX/bin/nix" show-derivation "/nix/store/$group_b_drv" 2>/dev/null \
    | python3 -c "
import json, sys
d = json.load(sys.stdin)
info = list(d['derivations'].values())[0]
matches = [k for k in info['inputs']['drvs'] if '-configure-' in k]
print(matches[0] if matches else '')
")
  if [[ -z "$group_a_drv" ]]; then
    echo "  could not find group A derivation (package=$package edit=$edit_arg)" >&2
    return 1
  fi

  "$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link --print-out-paths \
    "/nix/store/$group_a_drv^ggtree" 2>/tmp/nixgg-dyndrv-cutoff-build.log | tail -1 || {
      echo "  group A build failed (package=$package edit=$edit_arg); see /tmp/nixgg-dyndrv-cutoff-build.log" >&2
      tail -10 /tmp/nixgg-dyndrv-cutoff-build.log >&2
      return 1
    }
}

run_cutoff_check() {
  local package="$1"

  baseline=$(groupA_ggtree_path "$package" "") || return 1
  echo "  baseline: $baseline"

  excluded=$(groupA_ggtree_path "$package" "excluded") || return 1
  echo "  excluded: $excluded"

  included=$(groupA_ggtree_path "$package" "included") || return 1
  echo "  included: $included"

  local ok=1
  if [[ "$excluded" != "$baseline" ]]; then
    printf '\033[1;31m  FAIL\033[0m excluded-file edit changed group A output — early-cutoff broken\n' >&2
    ok=0
  fi
  if [[ "$included" == "$baseline" ]]; then
    printf '\033[1;31m  FAIL\033[0m included-file edit did NOT change group A output — filter not discriminating (excludes everything?)\n' >&2
    ok=0
  fi

  if [[ "$ok" != "1" ]]; then
    return 1
  fi
  printf '\033[1;32m  PASS\033[0m excluded-edit cached, included-edit invalidated\n'
}

overall_ok=1

printf '\033[1;36m===== hello (dynDrvConfigureCacheStdenv, single-output) =====\033[0m\n'
run_cutoff_check hello || overall_ok=0

printf '\033[1;36m===== gdbm (dynDrvConfigureCacheStdenv, multi-output: out/dev/info/lib/man) =====\033[0m\n'
run_cutoff_check gdbm || overall_ok=0

if [[ "$overall_ok" != "1" ]]; then
  echo "early-cutoff verification failed."
  exit 1
fi
