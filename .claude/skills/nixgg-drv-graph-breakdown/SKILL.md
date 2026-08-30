---
name: nixgg-drv-graph-breakdown
description: |
  Count exactly how many derivations a nixgg sandbox-mode build (mkNixggBuild /
  dynDrvStdenv / batchGroups) breaks into, WITHOUT compiling a single translation
  unit. Builds only the outer wrapper's `^out` — which runs the real build command
  (make/ninja) with nixgg's shim doing cheap `nix derivation add` registration —
  then walks the registered .drv graph structurally via `nix show-derivation`.
  Confirmed at scale on lua (39 drvs), mosh-batch (8, batching engaged), fmt-batch/
  gcc-batch (batching declined, negative case), and LLVM's llvm-min-tblgen phase
  (186 drvs, ~2000 real TUs never touched). Trigger phrases: "how many drvs does
  this break into", "what does the batch graph look like", "inspect the dyn-drv
  build graph without forcing it", "count TUs without compiling", "did batching
  actually engage for this example".
---

# nixgg drv-graph breakdown

## What this does

For any nixgg sandbox-mode flake attribute (`mkNixggBuild` output, `dynDrvStdenv`-
wrapped package, a `*-batch` example, LLVM's phase-chained attrs, etc.), reports
the exact set and count of derivations the shim registered for that build —
`tu-*.o.drv` compiles, `ar-*.a.drv` archives, `batch-*.a.drv` combined-archive
groups, and the final `bin-*.drv`/`ar-*.a.drv` link/archive — **without running a
single real compiler invocation**. This is the mechanism behind the "how many
drvs do these break into" / "did batching actually engage" class of question.

## Why this works

`mkNixggBuild`'s outer derivation has `outputHashMode = "text"`; its own build
just runs the package's ordinary build command (`make`, `ninja`, `cmake --build`)
with nixgg's shims live. Every `cc`/`c++`/`ar` invocation the shim intercepts
calls `nix derivation add` — a cheap JSON-to-drv-path registration, not a build —
and the shim logs `[nixgg]     drv:  /nix/store/...-tu-foo.o.drv` for each one.
The outer derivation's own **output** is literal text: the path of the final
submitted inner drv (e.g. `bin-mosh-server.drv`). None of the registered `tu-`/
`ar-`/`batch-` drvs are *built* by this — only added to the store as `.drv`
files — so building the outer wrapper never forces a real compile, however large
the project.

`nix build`'s normal target (the flake attr / `.package`) forces full resolution
of the whole `builtins.outputOf` chain, which DOES run every TU. The trick is to
build one level short of that: the outer wrapper's `.drv.drvPath`, at its `^out`
output specifically (not `^out` on any inner `tu-`/`ar-`/`bin-` drv).

## Recipe

```sh
export NIX_REMOTE=""
export NIX_CONFIG="
experimental-features = nix-command flakes ca-derivations dynamic-derivations configurable-impure-env
extra-system-features = builder-rpc-v0
store = local?root=/tmp/nixgg-graph-inspect
"
PATCHED_NIX="$PWD/.patched-nix"   # nix build .#patched-nix -o .patched-nix, one-time

ATTR=.#mosh-batch    # or .#lua, .#llvm-min-tblgen, any mkNixggBuild-shaped attr

# 1. Get the OUTER wrapper's own .drv path (not the .package attr — that one
#    forces full resolution). `.drv` is mkNixggBuild's raw dyn-drv attrset;
#    dynDrvStdenv-wrapped packages don't expose `.drv` directly — see below.
outer=$("$PATCHED_NIX/bin/nix" eval --raw "${ATTR}.drv.drvPath")

# 2. Build ONLY the outer wrapper's ^out. This runs make/ninja/cmake with
#    the shim registering every tu-/ar-/batch- drv via `nix derivation add`,
#    but forces NOTHING beyond that — its own output is the submitted inner
#    .drv's path, as plain text.
inner_drv=$("$PATCHED_NIX/bin/nix" build --no-eval-cache --no-link \
  --print-out-paths "${outer}^out")

echo "submitted drv: $inner_drv"
```

`inner_drv` is now a real store path like `/nix/store/...-bin-mosh-server.drv` —
still just a `.drv` file, nothing built.

## Walking the graph

`nix show-derivation`'s JSON schema (as of this writing) is
`{"derivations": {"<basename>": {"inputs": {"drvs": {"<basename>": {...}}}}}}` —
note the double nesting (`derivations` wrapper, then `inputs.drvs`, NOT the flat
`inputDrvs` some other Nix JSON formats use). `walk.py` in this skill's own
directory does the recursive walk and prints the breakdown by drv kind:

```sh
python3 .claude/skills/nixgg-drv-graph-breakdown/walk.py "$PATCHED_NIX/bin/nix" "$inner_drv"
```

This prints the exact breakdown — e.g. `mosh-batch` → `{'batch': 6, 'bin': 1, 'tu': 1}`
(8 total, batching engaged across all 6 archives); `fmt-batch`/`gcc-batch` →
all `tu`+one `ar`, zero `batch` (batching correctly declined — see Gotchas);
LLVM's `llvm-min-tblgen` phase alone → 186 (182 tu + 3 ar + 1 bin), discovered
without compiling a single one of LLVM's ~2000 translation units.

## Gotchas

- **`.drv.drvPath` vs `.drv.outPath`**: `outPath` on a CA derivation is an
  unresolved placeholder until built — always go through `.drvPath` then
  `^out`, never eval `.outPath` and expect a real path back.
- **`^out` must be on the OUTER wrapper, not an inner drv.** Building any
  `tu-`/`ar-`/`bin-`/`batch-` drv's own `^out` forces a REAL compile/archive/
  link of that one member — fine for spot-checking one member, fatal to the
  "never force a build" property if done for the whole graph.
- **`dynDrvStdenv`-wrapped packages (`hello-dyndrv`, `mosh-dyndrv-*`, etc.)
  don't expose a top-level `.drv`** the same way `mkNixggBuild` outputs do —
  the phase-1 sandboxed derivation is reachable via `nix derivation show
  .#foo` after `nix eval .#foo.drvPath`, but confirm the attrset shape first
  (`nix eval .#foo --apply builtins.attrNames`) since the phase-1/phase-2
  split means the interesting graph is nested one level differently than a
  plain `mkNixggBuild` result.
- **Negative case ≠ bug.** `fmt-batch`/`gcc-batch` are DESIGNED to decline
  batching: whichever archive is the build's own submission target
  (`NIXGG_SANDBOX_TARGET`) can never be batched — a `batch-`-named drv there
  would violate `submit-output`'s naming contract (see
  `go/internal/shim/batcharchive.go`'s `tryBatchArchive`). Seeing all-`tu`+one-
  `ar` for those two attrs is the correct, expected result, not a broken
  batchGroups config.
- **Substituters can mask a real build behind a cache hit** if you build the
  *fully resolved* `^out` of a `tu-`/`ar-`/`bin-` drv directly instead of the
  outer wrapper — a build-trace match downloads instead of compiling, so
  "no `building '...'` line" doesn't always mean "declined batching" if
  you're not doing the outer-wrapper trick above. The outer-wrapper approach
  sidesteps this entirely: it never asks Nix to realize any inner drv, so
  there's nothing for a substituter to intercept.
- **Cachix/auth warnings on stderr are noise** (`cachix.anduril.dev` 401,
  `unknown setting 'lazy-trees'`, etc.) — harmless on this Nix build, not a
  sign the technique failed. Check the actual `nix build`/`nix show-derivation`
  exit code and stdout, not stderr noise.
