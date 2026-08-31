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
# Env knobs:
#   ALT_STORE      root of the alt store (default /tmp/nixgg-equiv-store)
#   PATCHED_NIX    path to a builder-rpc-v0-capable nix
#                  (default: ./.patched-nix, built from flake if missing)
#   KEEP_STORE=1   don't wipe ALT_STORE at start (for local iteration)
#   ONLY=name      only run one fixture (hello / lua / fmt / …)

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
nixgg_root="$(cd "$here/.." && pwd)"

ALT_STORE="${ALT_STORE:-/tmp/nixgg-equiv-store}"
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

# Seed alt store with patched-nix so the outer .drv.drv can be built.
if [[ ! -e "$ALT_STORE/$(readlink -f "$PATCHED_NIX")" ]]; then
  "$PATCHED_NIX/bin/nix" copy --from daemon --to "local?root=$ALT_STORE" \
      --no-check-sigs "$(readlink -f "$PATCHED_NIX")" >/dev/null 2>&1 || true
fi

# ---------------------------------------------------------------
# Fixtures. Each entry is:
#
#   attr | native-src-flake-input | native-src-subdir | build-cmd
#
# - attr:        the flake attribute to `nix build`
# - native-src:  either a flake-input attr name (e.g. "lua-src") for
#                the same tarball the sandbox pulls in, or "example"
#                (path-relative to nixgg_root) for the local example
# - native-src-subdir: cd into this dir inside the unpacked src
#                (empty = src root)
# - build-cmd:   what to run in the src dir under `nix develop`
# ---------------------------------------------------------------
run_fixture() {
  # attr:      flake attr (also used to derive .#$attr-shell)
  # src_input: flake-input name for the source ("example" for hello)
  # subdir:    cd into this dir inside the unpacked src (empty = root)
  #
  # The build command comes from the flake itself:
  # `.#$attr-shell.passthru.buildCommand` — same string mkNixggBuild
  # passes to buildPhase. No per-fixture logic lives in this script.
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
  #   - tu-*.o.drv + ar-*.a.drv + bin-*.drv (nixgg-emitted)
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
    [[ ! "$base" =~ ^[a-z0-9]+-(tu-|ar-|bin-) ]] && continue
    # Peek the aterm. bin-*/ar-* drvs typically reference at least
    # one .drv in inputDrvs. tu-*.o.drv (compile) usually has an
    # empty inputDrvs and non-empty inputSrcs (the staged src +
    # toolchain), so we can't reject "empty inputDrvs" outright
    # — instead, only drop resolved bin-/ar-* by name pattern
    # (they always appear alongside the unresolved form, so we can
    # dedup by keeping the LATER-created one? No — keep whichever
    # references drvs in position 2).
    if [[ "$base" =~ ^[a-z0-9]+-bin- || "$base" =~ ^[a-z0-9]+-ar- ]]; then
      # bin-/ar- shim emits reference inputDrvs. If this drv has
      # zero .drv refs in position 2, it's the resolved rewrite.
      if ! head -c 500 "$f" | grep -q '\[("/nix/store/[^"]*\.drv"'; then
        continue
      fi
    fi
    echo "$base"
  done | sort -u)

  # -- 2. native build --
  local workdir="$(mktemp -d)"
  local src
  if [[ "$src_input" == "example" ]]; then
    src="$nixgg_root/example"
  else
    # Resolve just this one flake input's store path — NOT `nix flake
    # archive`, which resolves and copies EVERY input (including
    # large ones unrelated to this fixture, like ffmpeg-src/llvm-src/
    # gcc-src) into whatever store NIX_CONFIG points at. Under this
    # script's own isolated ALT_STORE that's needlessly expensive and,
    # confirmed directly in CI (tests/batch-drv-equivalence.sh's own
    # copy of this same pattern), can silently fail (stderr suppressed
    # below) and report "could not resolve native src" despite the
    # fixture's own sandbox build having already succeeded moments
    # earlier against the very same input.
    src=$("$PATCHED_NIX/bin/nix" eval --impure --raw \
      --expr "(builtins.getFlake \"$nixgg_root\").inputs.\"$src_input\".outPath" 2>/dev/null)
  fi

  if [[ -z "$src" || ! -e "$src" ]]; then
    echo "could not resolve native src for $attr (input=$src_input)" >&2
    return 1
  fi

  # Copy source, run the build. Store paths are read-only, so we
  # need our own writable copy.
  cp -a "$src"/. "$workdir/"
  chmod -R u+w "$workdir"

  # Wipe any pre-existing .nixgg/ that auto-seed might hit.
  rm -rf "$workdir/.nixgg" 2>/dev/null || true

  # "example" is a live working directory, not a pristine store path, so
  # a stray `make` there leaves main.o / util.o / hello behind. Those are
  # gitignored (the sandbox never sees them) but this copy is verbatim,
  # and make would skip both compiles — zero thunks, looking like a nixgg
  # bug. Use the fixture's own `clean` target so it can't drift from the
  # build rules. No shims on PATH here, so this deletes and never builds.
  if [[ -e "$workdir/${subdir}/Makefile" ]]; then
    ( cd "$workdir/${subdir}" && make clean ) >/dev/null 2>&1 || true
  fi

  # Pull the exact buildCommand the sandbox runs, straight from the
  # flake — no duplication of build recipes in this test.
  local build_cmd
  build_cmd="$("$PATCHED_NIX/bin/nix" eval --raw \
    "$nixgg_root#$attr-shell.passthru.buildCommand" 2>/dev/null)" || {
      echo "could not read buildCommand for $attr" >&2
      return 1
    }

  printf '==> native: nix develop .#%s-shell in %s\n' "$attr" "$workdir/${subdir}"
  local nt_log="/tmp/nixgg-equiv-$attr-native.log"
  # `.#<attr>-shell` is a plain mkShell whose shellHook is a byte-copy
  # of mkNixggBuild.nix:preBuild — same buildInputs, same env scrub,
  # same NIXGG_* exports.
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

  # nixgg's auto-seed walks up to nearest `.git` and lands `.nixgg/`
  # there. Under our tempdir (no .git), it lands wherever the shim
  # ran — for a recursive-make build like mosh's that's one
  # `.nixgg/thunks/` per subdir the shim was invoked in. Collect
  # thunks from ALL of them.
  local thunk_files
  thunk_files=$(find "$workdir" -type f -path '*/.nixgg/thunks/*.nix' 2>/dev/null)
  if [[ -z "$thunk_files" ]]; then
    echo "native build produced no thunks; see $nt_log" >&2
    return 1
  fi

  local nt_drvs
  nt_drvs=$(while IFS= read -r t; do
    "$PATCHED_NIX/bin/nix" eval --no-eval-cache --impure --raw \
      --file "$t" drvPath 2>/dev/null | xargs -n1 basename
  done <<<"$thunk_files" | sort -u)

  # -- 3. compare sets --
  local only_sandbox only_native both
  only_sandbox=$(comm -23 <(echo "$sb_drvs") <(echo "$nt_drvs"))
  only_native=$(comm -13 <(echo "$sb_drvs") <(echo "$nt_drvs"))
  both=$(comm -12 <(echo "$sb_drvs") <(echo "$nt_drvs"))

  # Line counts. `wc -l` counts newlines; add 1 for content that
  # doesn't end in \n. Simplest: filter empty lines then wc.
  local n_both n_only_sb n_only_nt
  n_both=$(printf '%s\n' "$both" | grep -c . || true)
  n_only_sb=$(printf '%s\n' "$only_sandbox" | grep -c . || true)
  n_only_nt=$(printf '%s\n' "$only_native" | grep -c . || true)
  n_both=${n_both:-0}
  n_only_sb=${n_only_sb:-0}
  n_only_nt=${n_only_nt:-0}

  printf '   %d drvs match, %d only-sandbox, %d only-native\n' \
    "$n_both" "$n_only_sb" "$n_only_nt"

  if (( n_only_sb > 0 || n_only_nt > 0 )); then
    printf '\033[1;31mMISMATCH\033[0m %s\n' "$label" >&2
    (( n_only_sb > 0 )) && printf '  only in sandbox:\n%s\n' "$only_sandbox" >&2
    (( n_only_nt > 0 )) && printf '  only in native:\n%s\n' "$only_native" >&2
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
