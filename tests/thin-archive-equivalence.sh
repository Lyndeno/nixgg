#!/usr/bin/env bash
# Regression test: native and sandbox modes produce byte-identical
# drvs for a build that uses a THIN archive (`ar csrDT`), and the
# resulting binary actually runs — proving the member-propagation
# mechanism in go/internal/shim/storeinput.go's classifyInputs +
# go/internal/members works identically in both modes, not just in
# the isolated Go unit tests (thinarchive_test.go).
#
# Mirrors tests/drv-equivalence.sh's own methodology (same shared
# scaffolding in tests/lib/drv-equiv-common.sh) but for a fixture that
# has no flake-input source (examples/thin-archive/src is a small
# in-tree fixture, not a fetched tarball) — so native src resolution
# is done directly here rather than through
# equiv_resolve_native_src's flake-input branch.
#
# Beyond the hash-set comparison, this script also realises the
# sandbox build's own binary and runs it, and separately force-
# realises the native build's binary and runs it — catching a bug
# where both modes happen to agree on a drv hash but the underlying
# mechanism is still wrong in a way that doesn't affect hashing (not
# expected here, since the whole point of the Derivation struct is
# that same struct -> same hash -> same script, but cheap insurance
# given this is a newly-added Kind of dependency edge).
#
# Env knobs: same as drv-equivalence.sh (ALT_STORE, PATCHED_NIX,
# KEEP_STORE).

set -euo pipefail

# shellcheck source=lib/drv-equiv-common.sh
source "$(cd "$(dirname "$0")" && pwd)/lib/drv-equiv-common.sh"

equiv_common_setup "/tmp/nixgg-thin-archive-equiv-store"

attr="thin-archive"
label="$attr"

echo
printf '\033[1;36m===== %s =====\033[0m\n' "$label"

# -- 1. sandbox build --
pre_snap=$(ls "$ALT_STORE"/nix/store/ 2>/dev/null | sort)

sb_log="/tmp/nixgg-thin-archive-equiv-sandbox.log"
printf '==> sandbox: nix build .#%s\n' "$attr"
"$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
  --print-out-paths "$nixgg_root#$attr" \
  > "$sb_log" 2>&1 || {
    echo "sandbox build failed; see $sb_log:" >&2
    tail -20 "$sb_log" >&2
    exit 1
  }
sb_out=$(tail -1 "$sb_log")

post_snap=$(ls "$ALT_STORE"/nix/store/ 2>/dev/null | sort)
new_paths=$(comm -13 <(echo "$pre_snap") <(echo "$post_snap"))

# Same filter as drv-equivalence.sh's own run_fixture: tu-/ar-/bin-
# drvs, plus dropping the resolved-rewrite variant of a target drv
# (see that script's own comment for the full rationale — identical
# reasoning applies here unchanged).
sb_drvs=$(for base in $new_paths; do
  f="$ALT_STORE/nix/store/$base"
  [[ ! -f "$f" ]] && continue
  [[ "$base" == *.drv.drv ]] && continue
  [[ ! "$base" =~ ^[a-z0-9]+-(tu-|ar-|bin-|nixgg-) ]] && continue
  if [[ "$base" =~ ^[a-z0-9]+-nixgg- ]]; then
    grep -q '"outputHashMode","nar"' "$f" 2>/dev/null || continue
  fi
  if [[ "$base" =~ ^[a-z0-9]+-bin- || "$base" =~ ^[a-z0-9]+-ar- || "$base" =~ ^[a-z0-9]+-nixgg- ]]; then
    if ! head -c 500 "$f" | grep -q '\[("/nix/store/[^"]*\.drv"'; then
      continue
    fi
  fi
  echo "$base"
done | sort -u)

# -- 2. functional check: the sandbox binary actually runs --
printf '==> functional: running sandbox-built binary\n'
sb_bin="$ALT_STORE$sb_out/bin/thin-archive"
if [[ ! -x "$sb_bin" ]]; then
  echo "sandbox binary missing or not executable: $sb_bin" >&2
  exit 1
fi
sb_run_out=$("$sb_bin")
if [[ "$sb_run_out" != "thin-archive: ok" ]]; then
  echo "sandbox binary produced unexpected output: $sb_run_out" >&2
  exit 1
fi
printf '\033[1;32mOK\033[0m       sandbox binary runs: %s\n' "$sb_run_out"

# -- 3. native build --
workdir="$(mktemp -d)"
cp -a "$nixgg_root/examples/thin-archive/src"/. "$workdir/"
chmod -R u+w "$workdir"
rm -rf "$workdir/.nixgg" 2>/dev/null || true

nt_log="/tmp/nixgg-thin-archive-equiv-native.log"
printf '==> native: nix develop .#%s-shell in %s\n' "$attr" "$workdir"
(
  cd "$workdir"
  "$PATCHED_NIX/bin/nix" develop "$nixgg_root#$attr-shell" --command bash -c "
    export NIXGG_STORE='local?root=$ALT_STORE'
    export NIXGG_AUTOFORCE=1
    set -euo pipefail
    make
  "
) > "$nt_log" 2>&1 || {
  echo "native build failed; see $nt_log:" >&2
  tail -20 "$nt_log" >&2
  rm -rf "$workdir"
  exit 1
}

# -- 4. functional check: the native binary actually runs --
printf '==> functional: running native-built binary\n'
if [[ ! -x "$workdir/thin-archive" ]]; then
  echo "native binary missing or not executable: $workdir/thin-archive" >&2
  rm -rf "$workdir"
  exit 1
fi
nt_run_out=$("$workdir/thin-archive")
if [[ "$nt_run_out" != "thin-archive: ok" ]]; then
  echo "native binary produced unexpected output: $nt_run_out" >&2
  rm -rf "$workdir"
  exit 1
fi
printf '\033[1;32mOK\033[0m       native binary runs: %s\n' "$nt_run_out"

# -- 5. compare drv sets --
thunk_files=$(equiv_collect_thunks "$workdir")
if [[ -z "$thunk_files" ]]; then
  echo "native build produced no thunks; see $nt_log" >&2
  rm -rf "$workdir"
  exit 1
fi

nt_drvs=$(while IFS= read -r t; do
  equiv_thunk_drvpath "$t"
done <<<"$thunk_files" | sort -u)

if ! n_both=$(equiv_report_sets "$label" "drvs" "$sb_drvs" "$nt_drvs"); then
  rm -rf "$workdir"
  exit 1
fi

rm -rf "$workdir"

if (( n_both == 0 )); then
  printf '\033[1;31mNO DRVS\033[0m %s — filter matched nothing; the fixture regressed\n' "$label" >&2
  exit 1
fi

printf '\033[1;32mMATCH\033[0m    %s (%d drvs)\n' "$label" "$n_both"
echo
echo "thin-archive fixture: native/sandbox equivalent and both functional."
