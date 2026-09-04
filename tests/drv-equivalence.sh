#!/usr/bin/env bash
# Regression test: native and sandbox modes produce byte-identical
# .drv files for the same source.
#
# Runs each fixture two ways:
#
#   sandbox:  `nix build .#<attr>`     — JSON drv → nix derivation add
#   native:   fetch same source, unpack into a tempdir, `nix develop`
#             and run the same buildCommand there. Shims write .nix
#             thunks; nix-instantiate on each thunk → drv path.
#
# Compare the SET of drv-hashes produced. If native and sandbox agree
# on every drv-hash, the mode-independent Derivation representation
# is doing its job.
#
# Shared alt-store/patched-nix scaffolding, native-src resolution, the
# native build invocation, and the match/mismatch reporting live in
# tests/lib/drv-equiv-common.sh (shared with
# tests/batch-drv-equivalence.sh, which proves the same invariant for
# the batch-archive Kind). Only the sandbox-side drv-name filter and
# the fixture list are this script's own.
#
# Env knobs:
#   ALT_STORE      root of the alt store (default /tmp/nixgg-equiv-store)
#   PATCHED_NIX    path to a builder-rpc-v0-capable nix
#                  (default: ./.patched-nix, built from flake if missing)
#   KEEP_STORE=1   don't wipe ALT_STORE at start (for local iteration)
#   ONLY=name      only run one fixture (hello / lua / fmt / …)

set -euo pipefail

# shellcheck source=lib/drv-equiv-common.sh
source "$(cd "$(dirname "$0")" && pwd)/lib/drv-equiv-common.sh"

equiv_common_setup "/tmp/nixgg-equiv-store"

# ---------------------------------------------------------------
# Fixtures. Each entry is:
#
#   attr | native-src-flake-input | native-src-subdir
#
# - attr:        the flake attribute to `nix build`
# - native-src:  either a flake-input attr name (e.g. "lua-src") for
#                the same tarball the sandbox pulls in, or "example"
#                (path-relative to nixgg_root) for the local example
# - native-src-subdir: cd into this dir inside the unpacked src
#                (empty = src root)
#
# The build command comes from the flake itself:
# `.#$attr-shell.passthru.buildCommand` — same string mkNixggBuild
# passes to buildPhase. No per-fixture logic lives in this script.
# ---------------------------------------------------------------
run_fixture() {
  local attr="$1" src_input="$2" subdir="$3"
  local label="$attr"

  echo
  printf '\033[1;36m===== %s =====\033[0m\n' "$label"

  # -- 1. sandbox build --
  # Snapshot the store *before* this fixture so we can compute
  # newly-added drvs afterward. Running multiple fixtures against
  # a single alt store (--keep-store, or just fixture-i then
  # fixture-i+1) would otherwise pollute the set.
  local pre_snap; pre_snap=$(ls "$ALT_STORE"/nix/store/ 2>/dev/null | sort)

  local sb_log="/tmp/nixgg-equiv-$attr-sandbox.log"
  printf '==> sandbox: nix build .#%s\n' "$attr"
  "$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
    --print-out-paths "$nixgg_root#$attr" \
    > "$sb_log" 2>&1 || {
      echo "sandbox build failed; see $sb_log:" >&2
      tail -20 "$sb_log" >&2
      return 1
    }

  # Set of drv hashes the sandbox produced. Filter:
  #   - tu-*.o.drv (unchanged naming) + a target's own link/archive
  #     drv. The latter's naming changed once mkNixggBuild gained
  #     multi-target support: every mkNixggBuild-based build now
  #     names its OWN target drvs "<outerBuildName>-<targetKey>" (no
  #     "bin-"/"ar-" prefix at all — see
  #     go/internal/shim/storeinput.go's multiTargetName docstring
  #     for why), while dynDrvStdenv/dynDrvConfigureCacheStdenv (which
  #     never set NIXGG_SANDBOX_TARGET to the JSON-map shape) still
  #     produce the OLD "bin-<outName>"/"ar-<outName>" names for their
  #     own per-TU link/archive drvs. Both shapes are real and need
  #     to match here — "^[a-z0-9]+-nixgg-" catches the former,
  #     "^[a-z0-9]+-(bin-|ar-)" the latter.
  #
  #     "^[a-z0-9]+-nixgg-" ALSO matches things that aren't a target
  #     drv at all: the outer text-hash wrapper itself (name is bare
  #     "nixgg-<pname>", no per-target suffix) and, coincidentally,
  #     nixgg's own toolchain build drvs (name is literally "nixgg" —
  #     the nixgg-nix helper package, the nixgg-bin build). Both are
  #     excluded below by outputHashMode: a real target drv is always
  #     "nar" (see linker.nix/archiver.nix/LinkJSON/ArchiveJSON — none
  #     of them ever use "text"); the outer wrapper is always "text"
  #     (mkNixggBuild.nix's own dyn-drv marker); the toolchain drvs
  #     have no outputHashMode key at all (ordinary input-addressed
  #     derivations, not CA).
  #   - drop the outer .drv.drv wrapper (text-mode drv-producing)
  #   - drop RESOLVED variants: when Nix builds the outer .drv.drv,
  #     it rewrites each inner drv from inputDrvs-form (references
  #     other drvs by path) into inputSrcs-form (references the
  #     resolved store outputs). The former is what our shim
  #     emitted; the latter is Nix's rewrite. Native mode never
  #     produces the rewritten variant, so exclude it here for a
  #     like-for-like set comparison. Heuristic: the unresolved
  #     form has *some* .drv path in the aterm's inputDrvs slot
  #     (position 2, `[(…drv…)]`). The resolved form has `[]`
  #     there.
  local post_snap; post_snap=$(ls "$ALT_STORE"/nix/store/ 2>/dev/null | sort)
  local new_paths; new_paths=$(comm -13 <(echo "$pre_snap") <(echo "$post_snap"))

  local sb_drvs
  sb_drvs=$(for base in $new_paths; do
    local f="$ALT_STORE/nix/store/$base"
    [[ ! -f "$f" ]] && continue
    [[ "$base" == *.drv.drv ]] && continue
    [[ ! "$base" =~ ^[a-z0-9]+-(tu-|ar-|bin-|nixgg-) ]] && continue
    if [[ "$base" =~ ^[a-z0-9]+-nixgg- ]]; then
      # Real target drv only if outputHashMode is "nar" — excludes
      # the outer text-hash wrapper and nixgg's own toolchain drvs
      # (see the comment above).
      grep -q '"outputHashMode","nar"' "$f" 2>/dev/null || continue
    fi
    # Peek the aterm. A target's own link/archive drv (either naming
    # shape) typically references at least one .drv in inputDrvs.
    # tu-*.o.drv (compile) usually has an empty inputDrvs and non-
    # empty inputSrcs (the staged src + toolchain), so we can't
    # reject "empty inputDrvs" outright — instead, only drop resolved
    # target-drv variants by name pattern (they always appear
    # alongside the unresolved form).
    if [[ "$base" =~ ^[a-z0-9]+-bin- || "$base" =~ ^[a-z0-9]+-ar- || "$base" =~ ^[a-z0-9]+-nixgg- ]]; then
      # Target-drv shims emit references into inputDrvs. If this drv
      # has zero .drv refs in position 2, it's the resolved rewrite.
      if ! head -c 500 "$f" | grep -q '\[("/nix/store/[^"]*\.drv"'; then
        continue
      fi
    fi
    echo "$base"
  done | sort -u)

  # -- 2. native build --
  local workdir="$(mktemp -d)"
  local src
  src=$(equiv_resolve_native_src "$src_input") || true
  if [[ -z "$src" || ! -e "$src" ]]; then
    echo "could not resolve native src for $attr (input=$src_input)" >&2
    return 1
  fi

  local nt_log="/tmp/nixgg-equiv-$attr-native.log"
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

  local nt_drvs
  nt_drvs=$(while IFS= read -r t; do
    equiv_thunk_drvpath "$t"
  done <<<"$thunk_files" | sort -u)

  # -- 3. compare sets --
  local n_both
  if ! n_both=$(equiv_report_sets "$label" "drvs" "$sb_drvs" "$nt_drvs"); then
    rm -rf "$workdir"
    return 1
  fi
  printf '\033[1;32mMATCH\033[0m    %s (%d drvs)\n' "$label" "$n_both"
  rm -rf "$workdir"
}

fail=0

# Fixtures:  attr | src-input | src-subdir
if [[ -z "${ONLY:-}" || "$ONLY" == "hello" ]]; then
  run_fixture "hello" "example" "" || fail=1
fi
if [[ -z "${ONLY:-}" || "$ONLY" == "lua" ]]; then
  run_fixture "lua" "lua-src" "" || fail=1
fi
if [[ -z "${ONLY:-}" || "$ONLY" == "fmt" ]]; then
  run_fixture "fmt" "fmt-src" "" || fail=1
fi
if [[ -z "${ONLY:-}" || "$ONLY" == "mosh" ]]; then
  run_fixture "mosh" "mosh-src" "" || fail=1
fi
if [[ -z "${ONLY:-}" || "$ONLY" == "gcc" ]]; then
  run_fixture "gcc" "gcc-src" "" || fail=1
fi

echo
if (( fail )); then
  echo "some fixtures diverged."
  exit 1
fi
echo "all fixtures equivalent."
