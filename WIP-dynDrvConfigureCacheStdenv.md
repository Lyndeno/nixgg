# dynDrvConfigureCacheStdenv — work in progress

Context for resuming this thread in a fresh session. See
`nix/dynDrvConfigureCacheStdenv.nix`'s own top comment for the
mechanism; this doc is about *state*. Sibling docs:
`WIP-dynDrvStdenv.md` (the build/install split this borrows group B/C
from) and `WIP-configureCacheStdenv.md` (the configure/build split
this borrows group A from).

## What this is

Combines both existing tricks: `configureCacheStdenv`'s configure-step
early-cutoff (`configureSrcFilter` + CA), applied to the configure
step that `dynDrvStdenv`'s sandboxed build/install split otherwise
runs unconditionally on every rebuild.

```nix
pkgs.hello.override {
  stdenv = dynDrvConfigureCacheStdenv {
    stdenv = pkgs.stdenv;
    configureSrcFilter = {
      includePatterns = configureSrcFilterPresets.autotools;
      existenceStubs = [ "src/hello.c" ];
    };
  };
}
```

## Why configure can leave the sandbox

`dynDrvStdenv`'s phase 1 sets `NIXGG_BYPASS=1` for configure and only
unsets it in `preBuild`. Read `go/internal/shim/passthrough.go`'s
`bypassed()`: under that flag every shim call is a genuine
`syscall.Exec` passthrough — same process image, same argv, same env,
no RPC, no dynamic derivation. Configure under `dynDrvStdenv` today is
behaviorally identical to configure with no shims at all; it just
happens to run inside a sandbox that does nothing to it. So it can be
pulled out into its own plain, unsandboxed group and given
`configureCacheStdenv`'s exact treatment.

## Three groups

- **Group A** (configure) — `configureCacheStdenv`'s group A,
  unmodified, PLUS shims exported on `PATH` via `postPatch` with
  `NIXGG_BYPASS=1` held for the whole group (never unset — no
  buildPhase here to unset it before). Needed so cmake/autotools bake
  the shim's own store path into the generated Makefile instead of
  the real gcc-wrapper's (same Gotcha #2 `dynDrvStdenv` already
  documents). No sandbox, no `requiredSystemFeatures`.
- **Group B** (build only) — `dynDrvStdenv`'s phase 1, minus
  configurePhase: `unpackPhase patchPhase ggRestorePhase buildPhase`.
  Always runs its own real unpack+patch against the REAL, unfiltered
  `src` (same lesson `configureCacheStdenv`'s own history worked
  out — group A's snapshot may only have the filtered subset).
  `ggRestorePhase` overlays group A's real tree on top (mirroring
  `configureCacheStdenv`'s own restore logic byte-for-byte, including
  the `.gg-buildroot` absolute-path rewrite — see Gotcha #1 below),
  then rewrites every occurrence of group A's real output paths to
  `dynDrvStdenv`'s placeholder scheme (`/nonexistent`,
  `/nonexistent-<output>`) before buildPhase runs. Everything else —
  shim activation, bypass-unset in `preBuild`, `nixgg
  assemble`/submit in `postBuild`, CA text-mode, sandboxed
  `requiredSystemFeatures` — unchanged from `dynDrvStdenv`'s phase 1.
- **Group C** (install onward) — `dynDrvStdenv`'s phase 2, copied
  verbatim. Doesn't care whether the tree it restores came from a
  1-group or 2-group build side.

## Current status: working, hello + mosh + zstd verified

`.#hello-dyndrv-configure-cached` — autotools, single-output, WITH
`configureSrcFilter` set (autotools preset + `existenceStubs =
["src/hello.c"]`). Verified directly (2026-08-16):

1. **Builds and runs**: `nix build`/`nix run` produces a real binary;
   `versionCheckHook`'s own `installCheckPhase` printed the genuine
   "hello (GNU Hello) 2.12.3" banner during the build itself.
2. **Per-TU acceleration unaffected by pulling configure out**: group
   B's resolved derivation references 83 `tu-*` compile sources, 1
   `ar-*` archive, 1 `bin-*` link — 85 total, matching
   `hello-dyndrv`'s own documented shim-invocation count exactly.
3. **Early-cutoff holds**: constructed the package three ways
   (baseline / edit to `src/hello.c` [excluded] / edit to
   `configure.ac` [included]) and compared group A's own `ggtree`
   output path. baseline and excluded matched exactly; included
   differed. Automated in
   [tests/dyndrv-configure-cache-cutoff.sh](tests/dyndrv-configure-cache-cutoff.sh),
   mirroring `tests/configure-cache-cutoff.sh`'s own technique.
4. `tests/smoke.sh`'s quick set (15 examples, including
   `hello-dyndrv-configure-cached`, `mosh-dyndrv-configure-cached`,
   and `zstd-dyndrv-configure-cached`) — all pass, no regressions.

`.#mosh-dyndrv-configure-cached` — autotools + `autoreconfHook`, no
`configureSrcFilter` (mosh isn't in the verified preset set). Verified
directly:

5. **`autoreconfHook`'s phase injection is unaffected**: group A never
   hardcodes `phases` (uses `dontBuild`/`dontInstall`/... toggles, same
   as `configureCacheStdenv`'s own group A), so
   `appendToVar preConfigurePhases autoreconfPhase` still applies
   normally — no special-casing needed.
6. **Per-TU acceleration count matches exactly**: group B's resolved
   derivation references 30 `tu-*` compile sources, 6 `ar-*` archives,
   2 `bin-*` links — 38 total, the same count `mosh-dyndrv` already
   documents.
7. `mosh-server --version` runs correctly through `nix shell`.

`.#zstd-dyndrv-configure-cached` — cmake, 4 outputs, no
`configureSrcFilter` (zstd's own `CMakeLists.txt` uses `file(GLOB
...)`, so filtering can't preserve early-cutoff for it — same
reasoning as `zstd-cache`). Verified directly:

8. **Multi-output splitting works**: `out`/`bin`/`dev`/`man` all
   populated correctly by group C's `restoreOutputsScript`, same as
   `zstd-dyndrv`.
9. **A real `checkPhase` (`ctest`) passes**: `playTests` ran and
   passed against group C's resolved binaries, not stubs.
10. **The `gen_html` mid-build-exec fix applies to BOTH group A and
    group B**, not group B alone — see Gotcha #4 below. With the fix
    applied to both (as wired in `flake.nix`), `zstd`'s own
    manual-page generation (which execs a freshly-built helper
    mid-build) and the final `bin/zstd --version` both work correctly
    through `nix run`/`nix shell`.

## Gotchas

1. **`ggRestorePhase`'s overlay target is the CURRENT directory, not
   `$NIX_BUILD_TOP`.** First draft copied `${groupA.ggtree}/tree/.`
   into `"$NIX_BUILD_TOP/"` directly and `cd`'d to
   `"$NIX_BUILD_TOP/$(cat .../.gg-cwd)"` — this lands one level too
   shallow, because group B's real `unpackPhase` already `cd`'d into
   `$sourceRoot` before `ggRestorePhase` runs (same as
   `configureCacheStdenv`'s own group B). Fixed by copying into `.`
   (the current directory, already `$sourceRoot`) and also porting
   `configureCacheStdenv`'s `.gg-buildroot` absolute-path rewrite step
   (automake's generated Makefile can bake group A's absolute build
   directory as literal text via `build-aux/missing`) — missing that
   step produced `make: *** No rule to make target
   'lib/alloca.in.h'...` because the Makefile's own path references
   still pointed at group A's `/build/<hash>`, not group B's.
2. **Nix version matters for eval.** `builtins.outputOf` and
   `nix store submit-output` need the patched Nix
   (`.patched-nix/bin/nix`), not whatever `nix` is on `PATH` — same
   requirement as every other dyn-drv mechanism in this repo. A stock
   Nix eval fails opaquely with `attribute 'outputOf' missing`.
3. New files must be `git add`ed before flake evaluation can see
   them at all (Nix's flake source filter only tracks git-known
   paths) — unrelated to the mechanism itself, just an easy first-run
   trip-up.
4. **A gen_html-style mid-build-exec fix needs `extraGroupAAttrs`,
   not just `extraGroupBAttrs`.** Unlike `dynDrvStdenv`, where the
   whole unpack..build sequence is ONE group and `extraPhase1Attrs`
   alone suffices, here cmake's Makefile generation happens in group
   A (configure only) — group B never reconfigures
   (`dontConfigure`-equivalent by construction, no `configurePhase` in
   its `phases` string at all). Patching only `extraGroupBAttrs`
   reproduces the exact same `./gen_html: Permission denied` failure
   as the unpatched build, because group B restores group A's
   ALREADY-GENERATED (unpatched) Makefile/CMakeCache verbatim — the
   patch never gets a chance to influence what cmake wrote. Confirmed
   directly: patching group A only, or both A and B, both work;
   patching group B only does not. Practical upshot: any
   `extraPhase1Attrs`-style CMakeLists/configure-input patch needs to
   go in `extraGroupAAttrs` (where configure actually runs); pass it
   to `extraGroupBAttrs` too only if group B's OWN build step (not
   just configure) also needs to see it.

## Multi-output + configureSrcFilter combined — resolved

Previously deferred: hello only tested single-output+filter, zstd only
tested multi-output+no-filter (its own `CMakeLists.txt` globs sources,
so filtering can't preserve early-cutoff for it regardless). Closed by
adding `gdbm` as a fourth fixture — autotools, plain `configure` (no
`autoreconfHook`), 5 real outputs (`out dev info lib man`),
`AC_CONFIG_SRCDIR([src/gdbmdefs.h])`:

- `.#gdbm-dyndrv-configure-cached` in `flake.nix`, using the same
  `configureSrcFilterPresets.autotools` preset hello uses.
- Added to `tests/smoke.sh`'s `DYNCONFIGCACHE` set — builds, all 5
  outputs populated by group C's `restoreOutputsScript`, `nix run
  .#gdbm-dyndrv-configure-cached -- --version` prints the real
  `gdbmtool` banner (multi-output RPATH fixup + restore-outputs
  confirmed working end to end).
- `tests/dyndrv-configure-cache-cutoff.sh`/`-fixture.nix` parameterized
  over `package` (`hello` | `gdbm`) instead of hardcoding hello — group
  A's own `ggtree` output path is identical for baseline vs. an edit to
  `src/avail.c` (excluded by the autotools preset) and different for
  an edit to `configure.ac` (included). Verified directly: PASS for
  both packages.

No new gotchas beyond what hello/zstd already surfaced — the
multi-output restore/rpath-fixup machinery is shared verbatim with
`dynDrvStdenv`'s own group C and doesn't care whether group A was
filtered or not.
