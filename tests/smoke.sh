#!/usr/bin/env bash
# Smoke test: every example still builds, and its artifact is where we
# say it is and actually runs.
#
# This exists because tests/drv-equivalence.sh structurally cannot catch
# a whole class of bug. That test compares drv HASHES between native and
# sandbox mode; it never realises an output and never reads one. So when
# link outputs moved to $out/bin, it reported a clean 149/149 while every
# native build failed to collect its artifact and two phase-chaining
# examples referenced tool paths that no longer existed.
#
# It also covers the four examples drv-equivalence leaves out. That
# omission is structural, not an oversight: the gate resolves each
# fixture's native source from a single flake INPUT, and llvm/two-phase
# have no single `src` — they are multi-phase, one source per phase.
#
# Cheap by default (EXAMPLES=quick, ~2 min). The expensive ones are
# opt-in because llvm alone is ~1500 TUs.
#
# Usage:
#   tests/smoke.sh                 # quick set
#   EXAMPLES=all tests/smoke.sh    # everything, including llvm (~1h+)
#   EXAMPLES="hello gcc" tests/smoke.sh
#   ALT_STORE=/tmp/my-store tests/smoke.sh

set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"

ALT_STORE="${ALT_STORE:-/tmp/nixgg-smoke-store}"
PATCHED_NIX="${PATCHED_NIX:-$nixgg_root/.patched-nix}"
if [[ ! -x "$PATCHED_NIX/bin/nix" ]]; then
  echo "==> building patched nix (one-time)" >&2
  nix build --no-eval-cache "$nixgg_root#patched-nix" -o "$PATCHED_NIX" >&2 || exit 2
fi
mkdir -p "$ALT_STORE"

export NIX_REMOTE=""
export NIX_CONFIG="
experimental-features = nix-command flakes ca-derivations dynamic-derivations configurable-impure-env
extra-system-features = builder-rpc-v0
store = local?root=$ALT_STORE
"

# attr | expected path inside $out | how to prove it works
#
# The expected path is the point of this test: it encodes the FHS layout
# (link -> bin/, ar -> lib/) so a change to output placement that forgets
# a consumer fails here loudly instead of in a user's terminal.
#
# "-" as the run command means the artifact is a library: existing at the
# right path with non-zero size is all we assert.
QUICK=(
  "hello|bin/hello|%s"
  "two-phase|bin/app|%s"
  "fmt|lib/libfmt.a|-"
  "lua|bin/lua|%s -v"
  "gcc|lib/libiberty.a|-"
  "mosh|bin/mosh-server|%s --version"
)
SLOW=(
  # Not in QUICK because it needs the kernel's `dev` output — a ~3GB
  # closure — which is a poor fit for a set advertised as "cheap by
  # default, ~2 min".
  #
  # The version in this path tracks linuxPackages.kernel.modDirVersion
  # from the flake pin, so a `nix flake update` that bumps the kernel
  # needs this string bumped too. Kept explicit rather than globbed
  # because the whole point of the `want` column is to pin the exact
  # FHS location, and depmod cares that it is lib/modules/<ver>/.
  "kmod|lib/modules/6.18.41/extra/hello_mod.ko|-"
  "redis|bin/redis-server|%s --version"
  "ffmpeg|bin/ffmpeg_g|%s -version"
  "llvm|bin/llc|%s --version"
)

case "${EXAMPLES:-quick}" in
  quick) SET=("${QUICK[@]}") ;;
  all)   SET=("${QUICK[@]}" "${SLOW[@]}") ;;
  *)     SET=()
         for want in ${EXAMPLES}; do
           for e in "${QUICK[@]}" "${SLOW[@]}"; do
             [[ "${e%%|*}" == "$want" ]] && SET+=("$e")
           done
         done
         if [[ ${#SET[@]} -eq 0 ]]; then
           echo "no known examples in EXAMPLES='$EXAMPLES'" >&2; exit 2
         fi ;;
esac

fail=0
for entry in "${SET[@]}"; do
  IFS='|' read -r attr want run <<<"$entry"
  printf '\033[1;36m===== %s =====\033[0m\n' "$attr"

  # Cheap (no-build) check: mainProgram must be set iff the artifact is
  # meant to be run. A library incorrectly claiming a mainProgram would
  # send `nix run` at a nonsense path; a binary missing one degrades
  # silently, since Nix's own pname fallback happens to paper over it
  # for every current example (pname == the target basename always) —
  # this is the only check that would catch that regressing.
  is_lib="0"; [[ "$want" == lib/* ]] && is_lib="1"
  has_mp="0"
  "$PATCHED_NIX/bin/nix" eval --no-eval-cache \
    "$nixgg_root#$attr.meta.mainProgram" >/dev/null 2>&1 && has_mp="1"
  if [[ "$is_lib" == "1" && "$has_mp" == "1" ]]; then
    printf '\033[1;31m  BAD META\033[0m %s has meta.mainProgram but is a library\n' "$attr" >&2
    fail=1; continue
  fi
  if [[ "$is_lib" == "0" && "$has_mp" == "0" ]]; then
    printf '\033[1;31m  BAD META\033[0m %s has no meta.mainProgram; nix run would '"'"'guess'"'"' \n' "$attr" >&2
    fail=1; continue
  fi

  log="/tmp/nixgg-smoke-$attr.log"
  out=$("$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
        --print-out-paths "$nixgg_root#$attr" 2>"$log" | tail -1)
  if [[ -z "$out" ]]; then
    echo "  BUILD FAILED; see $log:" >&2
    tail -8 "$log" >&2
    fail=1; continue
  fi

  # The artifact must be at the documented FHS path.
  disk="$ALT_STORE$out/$want"
  if [[ ! -e "$disk" ]]; then
    printf '\033[1;31m  MISSING\033[0m %s\n' "\$out/$want" >&2
    echo "  what is actually there:" >&2
    ( cd "$ALT_STORE$out" && find . -maxdepth 2 | sed 's|^|      |' ) >&2
    fail=1; continue
  fi
  if [[ ! -s "$disk" ]]; then
    echo "  EMPTY: \$out/$want" >&2; fail=1; continue
  fi

  if [[ "$run" == "-" ]]; then
    printf '\033[1;32m  OK\033[0m       $out/%s (%s bytes)\n' \
      "$want" "$(stat -c%s "$disk")"
    continue
  fi

  # shellcheck disable=SC2059
  cmd=$(printf "$run" "$disk")
  if out_txt=$(eval "$cmd" 2>&1 | head -1); then
    printf '\033[1;32m  OK\033[0m       $out/%s -> %s\n' "$want" "$out_txt"
  else
    printf '\033[1;31m  RAN BUT FAILED\033[0m $out/%s -> %s\n' "$want" "$out_txt" >&2
    fail=1
    continue
  fi

  # Also drive it through `nix run` itself, not just a direct exec of
  # the path we predicted — this is the actual point of `.package`
  # being a derivation rather than an outputOf string. Reuses the
  # build above (same drv, cached), so this is nearly free.
  runlog="/tmp/nixgg-smoke-$attr-run.log"
  if "$PATCHED_NIX/bin/nix" run --no-eval-cache "$nixgg_root#$attr" \
       -- --version >"$runlog" 2>&1 \
     || "$PATCHED_NIX/bin/nix" run --no-eval-cache "$nixgg_root#$attr" \
       >>"$runlog" 2>&1; then
    printf '\033[1;32m  OK\033[0m       nix run .#%s\n' "$attr"
  else
    printf '\033[1;31m  NIX RUN FAILED\033[0m .#%s; see %s\n' "$attr" "$runlog" >&2
    tail -4 "$runlog" >&2
    fail=1
  fi
done

echo
if [[ $fail -eq 0 ]]; then
  printf '\033[1;32mall examples build and run.\033[0m\n'
else
  printf '\033[1;31msome examples failed.\033[0m\n'
fi
exit $fail
