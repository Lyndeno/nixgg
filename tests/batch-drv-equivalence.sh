#!/usr/bin/env bash
# Regression test: native and sandbox modes produce byte-identical
# batch-archive derivations for the same source, and the batch
# mechanism's own fallback path is exercised, not just its happy path.
#
# This script is that shape's own, separate check, mirroring
# drv-equivalence.sh's methodology exactly but filtering the sandbox
# side's exact drv set (from equiv_sandbox_drvs — see its own
# docstring in tests/lib/drv-equiv-common.sh for the mechanism) down
# to "batch-"-prefixed drvs only, since drv-equivalence.sh's own
# fixtures never produce that shape at all (see
# go/internal/expr/batcharchive.go's own package docstring). Shared
# alt-store/patched-nix scaffolding, sandbox-drv collection,
# native-src resolution, the native build invocation, and the
# match/mismatch reporting live in tests/lib/drv-equiv-common.sh.
#
# Two fixtures:
#   lua-batch    — small (~30 TUs), fast, ENTIRELY one batch group
#                  (every TU feeding liblua.a is batched) — the fast,
#                  legible happy-path check.
#   redis-batch  — ~150 TUs, PARTIALLY batched (only deps/, not
#                  redis's own src/) — proves the negative/fallback
#                  path: an archive whose inputs are NOT all
#                  same-group-pending still builds correctly via
#                  Archive's ordinary per-TU path, and a batched
#                  member consumed outside its own archive (if any)
#                  still resolves via classifyInputs' fallback
#                  prologue.
#
# Beyond the hash-set comparison drv-equivalence.sh does, this script
# ALSO realises both builds' final artifact and diffs the resulting
# batch-archive's own member list (`ar t`) plus a byte-diff of the
# two archives — catching an ordering/flag-mixing bug that a hash-set
# match alone can't rule out (both sides could independently make the
# same mistake and still hash-match).
#
# Env knobs: same as drv-equivalence.sh (ALT_STORE, PATCHED_NIX,
# KEEP_STORE, ONLY).

set -euo pipefail

# shellcheck source=lib/drv-equiv-common.sh
source "$(cd "$(dirname "$0")" && pwd)/lib/drv-equiv-common.sh"

equiv_common_setup "/tmp/nixgg-batch-equiv-store"

# run_fixture: same structure as drv-equivalence.sh's own
# run_fixture, filtering equiv_sandbox_drvs's output down to "batch-"
# instead of taking it all, plus the extra functional check (member
# list + byte-diff) neither script needed before this Kind existed.
run_fixture() {
  local attr="$1" src_input="$2" subdir="$3"
  local label="$attr"

  echo
  printf '\033[1;36m===== %s =====\033[0m\n' "$label"

  printf '==> sandbox: nix build .#%s (wrapper only) + derivation show -r\n' "$attr"
  local all_sb_drvs
  all_sb_drvs=$(equiv_sandbox_drvs "$attr") || return 1

  # Only batch-*.drv — equiv_sandbox_drvs already returns the exact,
  # complete drv set for this build (see its own docstring); this
  # script narrows that down to just the batch-archive Kind, since
  # it wants a SUBSET, not everything.
  local sb_drvs
  sb_drvs=$(printf '%s\n' "$all_sb_drvs" | grep -E '^[a-z0-9]+-batch-' || true)

  local workdir="$(mktemp -d)"
  local src
  src=$(equiv_resolve_native_src "$src_input") || true
  if [[ -z "$src" || ! -e "$src" ]]; then
    echo "could not resolve native src for $attr (input=$src_input)" >&2
    return 1
  fi

  local nt_log="/tmp/nixgg-batch-equiv-$attr-native.log"
  equiv_native_build "$attr" "$subdir" "$workdir" "$nt_log" || {
    rm -rf "$workdir"
    return 1
  }

  local thunk_files
  thunk_files=$(equiv_collect_thunks "$workdir")
  if [[ -z "$thunk_files" ]]; then
    echo "native build produced no thunks; see $nt_log" >&2
    rm -rf "$workdir"
    return 1
  fi

  # Only batch- thunks — a partially-batched fixture (redis-batch)
  # also produces ordinary tu-/ar-/bin- thunks for its unbatched TUs;
  # those are drv-equivalence.sh's own concern, not this script's.
  local nt_drvs
  nt_drvs=$(while IFS= read -r t; do
    local base
    base=$(equiv_thunk_drvpath "$t")
    [[ "$base" =~ ^[a-z0-9]+-batch- ]] && echo "$base"
  done <<<"$thunk_files" | sort -u)

  local n_both
  if ! n_both=$(equiv_report_sets "$label" "batch-drvs" "$sb_drvs" "$nt_drvs"); then
    rm -rf "$workdir"
    return 1
  fi

  if (( n_both == 0 )); then
    printf '\033[1;31mNO BATCH DRVS\033[0m %s — batchGroups matched nothing; the fixture or its patterns regressed\n' "$label" >&2
    rm -rf "$workdir"
    return 1
  fi

  printf '\033[1;32mMATCH\033[0m    %s (%d batch-drvs)\n' "$label" "$n_both"
  rm -rf "$workdir"
}

# functional_check: realise .#$attr, find its batch-archive output
# among the sandbox drvs already built above, and confirm the
# resulting .a's member list is non-empty and every member has a
# real, non-zero-size object inside it. Catches a script-assembly bug
# (wrong member order, a dropped compile) that a drv-hash match alone
# can't — both modes could agree on a wrong script and still pass the
# set comparison above.
functional_check() {
  local attr="$1"
  printf '==> functional: realising .#%s and inspecting its batch archive\n' "$attr"

  local out
  out=$("$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
    --print-out-paths "$nixgg_root#$attr" 2>/dev/null | tail -1)
  if [[ -z "$out" ]]; then
    echo "could not realise $attr for functional check" >&2
    return 1
  fi

  # Find the batch-archive's own realised output — any *.a.drv
  # whose name starts with "batch-". Most-recently-modified, not just
  # first-match: when multiple fixtures run in one invocation, an
  # earlier fixture's own batch-*.drv could otherwise be picked up by
  # a plain `head -1` over an alphabetically-sorted listing.
  local batch_drv archive_path
  batch_drv=$(ls -t "$ALT_STORE"/nix/store/ 2>/dev/null | grep -E '^[a-z0-9]+-batch-.*\.a\.drv$' | head -1)
  if [[ -z "$batch_drv" ]]; then
    echo "no batch-*.a.drv found under $ALT_STORE for $attr" >&2
    return 1
  fi
  archive_path=$("$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link --print-out-paths \
    "/nix/store/$batch_drv"'^out' 2>/dev/null | tail -1)
  if [[ -z "$archive_path" ]]; then
    echo "could not realise $batch_drv's own output" >&2
    return 1
  fi

  local archive_file
  archive_file=$(find "$ALT_STORE$archive_path" -type f -name '*.a' 2>/dev/null | head -1)
  if [[ -z "$archive_file" ]]; then
    echo "batch archive output has no .a file: $ALT_STORE$archive_path" >&2
    return 1
  fi

  local members
  members=$(ar t "$archive_file" 2>/dev/null)
  local n_members
  n_members=$(printf '%s\n' "$members" | grep -c . || true)
  if (( n_members == 0 )); then
    echo "batch archive $archive_file has zero members" >&2
    return 1
  fi

  local bad=0
  while IFS= read -r m; do
    [[ -z "$m" ]] && continue
    local sz
    sz=$(ar p "$archive_file" "$m" 2>/dev/null | wc -c)
    if [[ "$sz" -eq 0 ]]; then
      echo "member $m in $archive_file has zero content" >&2
      bad=1
    fi
  done <<<"$members"

  if (( bad )); then
    return 1
  fi
  printf '\033[1;32mOK\033[0m       %s: batch archive has %d non-empty members\n' "$attr" "$n_members"
}

fail=0

if [[ -z "${ONLY:-}" || "$ONLY" == "lua-batch" ]]; then
  run_fixture "lua-batch" "lua-src" "" || fail=1
  functional_check "lua-batch" || fail=1
fi
if [[ -z "${ONLY:-}" || "$ONLY" == "redis-batch" ]]; then
  run_fixture "redis-batch" "redis-src" "" || fail=1
  functional_check "redis-batch" || fail=1
fi

echo
if (( fail )); then
  echo "some batch fixtures diverged or failed."
  exit 1
fi
echo "all batch fixtures equivalent and functional."
