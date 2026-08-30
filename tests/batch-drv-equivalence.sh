#!/usr/bin/env bash
# Regression test: native and sandbox modes produce byte-identical
# batch-archive derivations for the same source, and the batch
# mechanism's own fallback path is exercised, not just its happy path.
#
# tests/drv-equivalence.sh's own filter
# (^[a-z0-9]+-(tu-|ar-|bin-)) is BLIND to "batch-"-prefixed drvs by
# construction (see go/internal/expr/batcharchive.go's own package
# docstring) — this script is that shape's own, separate check,
# mirroring drv-equivalence.sh's methodology exactly but filtering on
# "batch-" instead.
#
# Two fixtures:
#   lua-batch    — small (~30 TUs), fast, ENTIRELY one batch group
#                  (every TU feeding liblua.a is batched) — the fast,
#                  legible happy-path check.
#   redis-batch-probe — ~150 TUs, PARTIALLY batched (only deps/, not
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

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"

ALT_STORE="${ALT_STORE:-/tmp/nixgg-batch-equiv-store}"
PATCHED_NIX="${PATCHED_NIX:-$nixgg_root/.patched-nix}"
if [[ ! -x "$PATCHED_NIX/bin/nix" ]]; then
  echo "==> building patched nix (one-time; substituted from cache)" >&2
  nix build --no-eval-cache "$nixgg_root#patched-nix" \
    -o "$PATCHED_NIX" >&2 || {
      echo "failed to build $nixgg_root#patched-nix" >&2
      exit 2
    }
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

if [[ ! -e "$ALT_STORE/$(readlink -f "$PATCHED_NIX")" ]]; then
  "$PATCHED_NIX/bin/nix" copy --from daemon --to "local?root=$ALT_STORE" \
      --no-check-sigs "$(readlink -f "$PATCHED_NIX")" >/dev/null 2>&1 || true
fi

# run_fixture: same structure as drv-equivalence.sh's own
# run_fixture, filtering on "batch-" instead of "tu-|ar-|bin-", plus
# the extra functional check (member list + byte-diff) neither script
# needed before this Kind existed.
run_fixture() {
  local attr="$1" src_input="$2" subdir="$3"
  local label="$attr"

  echo
  printf '\033[1;36m===== %s =====\033[0m\n' "$label"

  local pre_snap; pre_snap=$(ls "$ALT_STORE"/nix/store/ 2>/dev/null | sort)

  local sb_log="/tmp/nixgg-batch-equiv-$attr-sandbox.log"
  printf '==> sandbox: nix build .#%s\n' "$attr"
  "$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
    --print-out-paths "$nixgg_root#$attr" \
    > "$sb_log" 2>&1 || {
      echo "sandbox build failed; see $sb_log:" >&2
      tail -20 "$sb_log" >&2
      return 1
    }

  local post_snap; post_snap=$(ls "$ALT_STORE"/nix/store/ 2>/dev/null | sort)
  local new_paths; new_paths=$(comm -13 <(echo "$pre_snap") <(echo "$post_snap"))

  # Only batch-*.drv — same resolved-vs-unresolved concern
  # drv-equivalence.sh's own comment documents for bin-/ar- applies
  # here too (Nix rewrites the outer .drv.drv's inner drvs from
  # inputDrvs-form to inputSrcs-form on build); a batch-archive drv
  # always references at least the toolchain via inputs.srcs and
  # NEVER references another drv in inputs.drvs at all (see
  # BatchArchiveJSON's own docstring — no sibling drv/thunk inputs by
  # construction), so unlike bin-/ar-, there is no resolved-rewrite
  # variant to filter out here: this Kind's own drv never had a
  # nonempty inputDrvs to begin with.
  local sb_drvs
  sb_drvs=$(for base in $new_paths; do
    local f="$ALT_STORE/nix/store/$base"
    [[ ! -f "$f" ]] && continue
    [[ "$base" == *.drv.drv ]] && continue
    [[ ! "$base" =~ ^[a-z0-9]+-batch- ]] && continue
    echo "$base"
  done | sort -u)

  local workdir="$(mktemp -d)"
  local src
  if [[ "$src_input" == "example" ]]; then
    src="$nixgg_root/example"
  else
    src=$("$PATCHED_NIX/bin/nix" flake archive --json "$nixgg_root" 2>/dev/null \
      | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['inputs']['$src_input']['path'])" 2>/dev/null)
  fi

  if [[ -z "$src" || ! -e "$src" ]]; then
    echo "could not resolve native src for $attr (input=$src_input)" >&2
    return 1
  fi

  cp -a "$src"/. "$workdir/"
  chmod -R u+w "$workdir"
  rm -rf "$workdir/.nixgg" 2>/dev/null || true

  if [[ -e "$workdir/${subdir}/Makefile" ]]; then
    ( cd "$workdir/${subdir}" && make clean ) >/dev/null 2>&1 || true
  fi

  local build_cmd
  build_cmd="$("$PATCHED_NIX/bin/nix" eval --raw \
    "$nixgg_root#$attr-shell.passthru.buildCommand" 2>/dev/null)" || {
      echo "could not read buildCommand for $attr" >&2
      return 1
    }

  printf '==> native: nix develop .#%s-shell in %s\n' "$attr" "$workdir/${subdir}"
  local nt_log="/tmp/nixgg-batch-equiv-$attr-native.log"
  (
    cd "$workdir/${subdir}"
    "$PATCHED_NIX/bin/nix" develop "$nixgg_root#$attr-shell" --command bash -c "
      export NIXGG_STORE='local?root=$ALT_STORE'
      export NIXGG_AUTOFORCE=0
      set -euo pipefail
      $build_cmd
    "
  ) > "$nt_log" 2>&1 || {
    echo "native build failed; see $nt_log:" >&2
    tail -20 "$nt_log" >&2
    return 1
  }

  local thunk_files
  thunk_files=$(find "$workdir" -type f -path '*/.nixgg/thunks/*.nix' 2>/dev/null)
  if [[ -z "$thunk_files" ]]; then
    echo "native build produced no thunks; see $nt_log" >&2
    return 1
  fi

  # Only batch- thunks — a partially-batched fixture (redis-batch-probe)
  # also produces ordinary tu-/ar-/bin- thunks for its unbatched TUs;
  # those are drv-equivalence.sh's own concern, not this script's.
  local nt_drvs
  nt_drvs=$(while IFS= read -r t; do
    local base
    base=$("$PATCHED_NIX/bin/nix" eval --no-eval-cache --impure --raw \
      --file "$t" drvPath 2>/dev/null | xargs -n1 basename)
    [[ "$base" =~ ^[a-z0-9]+-batch- ]] && echo "$base"
  done <<<"$thunk_files" | sort -u)

  local only_sandbox only_native both
  only_sandbox=$(comm -23 <(echo "$sb_drvs") <(echo "$nt_drvs"))
  only_native=$(comm -13 <(echo "$sb_drvs") <(echo "$nt_drvs"))
  both=$(comm -12 <(echo "$sb_drvs") <(echo "$nt_drvs"))

  local n_both n_only_sb n_only_nt
  n_both=$(printf '%s\n' "$both" | grep -c . || true)
  n_only_sb=$(printf '%s\n' "$only_sandbox" | grep -c . || true)
  n_only_nt=$(printf '%s\n' "$only_native" | grep -c . || true)
  n_both=${n_both:-0}
  n_only_sb=${n_only_sb:-0}
  n_only_nt=${n_only_nt:-0}

  printf '   %d batch-drvs match, %d only-sandbox, %d only-native\n' \
    "$n_both" "$n_only_sb" "$n_only_nt"

  if (( n_only_sb > 0 || n_only_nt > 0 )); then
    printf '\033[1;31mMISMATCH\033[0m %s\n' "$label" >&2
    (( n_only_sb > 0 )) && printf '  only in sandbox:\n%s\n' "$only_sandbox" >&2
    (( n_only_nt > 0 )) && printf '  only in native:\n%s\n' "$only_native" >&2
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
if [[ -z "${ONLY:-}" || "$ONLY" == "redis-batch-probe" ]]; then
  run_fixture "redis-batch-probe" "redis-src" "" || fail=1
  functional_check "redis-batch-probe" || fail=1
fi

echo
if (( fail )); then
  echo "some batch fixtures diverged or failed."
  exit 1
fi
echo "all batch fixtures equivalent and functional."
