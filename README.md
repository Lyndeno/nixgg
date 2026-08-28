# nixgg

> A [gg][gg]-style build accelerator built on Nix content-addressed
> derivations.

Intercept every `-c` compile, `ar` archive, and link with a shim.
Turn each invocation into a content-addressed Nix derivation. Nix
decides what's cached and what needs building — nixgg just
constructs the expressions.

Two modes for producing derivations, same drv-hashes either way:

- **Native** — shims write `.nix` thunk files on disk, one `nix build`
  at the end. Works with any recent Nix daemon.
- **Sandbox / dyn-drv** — shims call `nix derivation add` inside a
  `builder-rpc-v0` sandbox and submit the final drv as the outer
  derivation's output. `nix build .#hello` Just Works, and so does
  `nix run .#hello` — the flake exposes a real derivation, not just
  a resolvable string.

## Try it

```sh
# 1. Enter a shim-enabled shell.
nix develop

# 2. Native: your normal build system, drvs materialised on demand.
cd example
make          # compiles + auto-force-links via NIXGG_AUTOFORCE=1
./hello

# 3. Sandbox: same source, whole graph as dynamic Nix derivations.
nix build .#hello
./result/bin/hello
# or just:
nix run .#hello

# 4. Real projects, sandbox mode, out-of-tree sources pinned in flake.lock.
nix build .#lua         # lua 5.4.7 — 32 TUs, 1 archive, 1 link
nix build .#fmt         # {fmt} 11.0.2 — cmake + ninja + libfmt.a
nix build .#mosh        # mosh unstable — autoconf + protobuf + openssl/ncurses/zlib
```

Both modes produce byte-identical `.drv` files, by construction: the
build command is rendered once, in Go, and sandbox mode bakes it into
a JSON drv while native mode passes the same text through a thunk for
`nix/resolve-script.nix` to fill in the few values only Nix knows at
eval time. `nix build .#lua` gets an instant cache hit from an earlier
native build in an extracted lua source tree, and vice versa.

Two tests, covering different failure modes:

- [tests/drv-equivalence.sh](tests/drv-equivalence.sh) — the invariant.
  149 drvs across five fixtures: `hello` (3), `lua` (37), `fmt` (3),
  `mosh` (38), `gcc` (68), every one matching byte-for-byte between the
  two modes. ~25 min; `ONLY=hello` is a 35-second smoke of the same
  machinery.
- [tests/smoke.sh](tests/smoke.sh) — every example builds, its artifact
  is at the FHS path it should be, and it runs. ~2 min;
  `EXAMPLES=all` adds redis, ffmpeg and llvm.

The second exists because the first structurally cannot catch a whole
class of bug: it compares drv *hashes* and never realises an output, so
it stayed green at 149/149 while a change to output placement left
native mode unable to collect any artifact at all.

Neither of the above catches a regression that widens *how much*
rebuilds on a small edit — both only check "did the build
succeed/match," not "how many translation units actually recompiled."
[tests/perf-regression.sh](tests/perf-regression.sh) closes that gap:
edit one `.c` file in a 34-TU fixture, assert exactly its own TU
recompiles and every other one stays a cache hit — the actual property
["Measured incremental-rebuild cost"](#measured-incremental-rebuild-cost)
below claims openssl gets in practice.

### Invoking sandbox mode explicitly

`nix develop`'s shellHook already prepends the patched Nix to `PATH`
and points `NIX_CONFIG` at an alt store (`local?root=/tmp/nixgg-store`,
override via `NIXGG_STORE`), so the `nix build .#hello` above Just
Works from inside it. To run sandbox mode from an arbitrary shell
instead, spell out all four pieces yourself:

```sh
nix build .#patched-nix -o .patched-nix     # one-time; substituted from cache

./.patched-nix/bin/nix build .#hello -Lv \
  --store 'local?root=/tmp/incremental' \
  --extra-experimental-features "ca-derivations dynamic-derivations" \
  --extra-system-features builder-rpc-v0
```

Every part of that is load-bearing:

- **`./.patched-nix/bin/nix`** — it must be the patched Nix, not
  whatever is on `PATH`. Your system Nix will get surprisingly far (it
  evaluates the flake and starts the outer derivation) and then fail
  inside the build with

  ```
  error: Submit outputs for a currently running derivation
         not supported by store 'local'
  ```

  because `nix store submit-output` does not exist in it.
- **`--store 'local?root=…'`** — an alternative store. Sandbox mode
  registers derivations from inside a running build, which a normal
  daemon store refuses.
- **`ca-derivations dynamic-derivations`** — content-addressed outputs
  and `builtins.outputOf`. Note the **plural** in `ca-derivations`;
  Nix treats an unknown feature name as a warning, not an error, so a
  typo here fails later and confusingly.
- **`--extra-system-features builder-rpc-v0`** — `mkNixggBuild` sets
  `requiredSystemFeatures = [ "builder-rpc-v0" ]`, so without this the
  derivation is simply unbuildable on this machine.
- **`-Lv`** is optional, but sandbox mode does its interesting work
  inside a build, so without it you see none of the `[nixgg]` lines.

One wrinkle worth knowing: `result` symlinks to a `/nix/store/…` path
that does not exist on your real filesystem, because the artifact lives
under the alt-store root. Read it there instead:

```sh
/tmp/incremental/nix/store/…-bin-hello/bin/hello
```

## Use it in your own project

nixgg is a flake input. Pull in `mkNixggBuild` and call it with your
own source, target, and build command — same function every example
in this repo uses.

Two things a consuming flake needs beyond the call itself: the
experimental features that dynamic derivations require, and a Nix that
can serve `builder-rpc-v0`. Both are shown below.

```nix
# flake.nix
{
  inputs.nixgg.url = "github:tomberek/nixgg";

  # mkNixggBuild's output is a `builtins.outputOf` node, and its outer
  # derivation asks for the builder-rpc-v0 system feature. Without
  # these, `nix build` fails at eval with "experimental Nix feature
  # 'dynamic-derivations' is disabled". Nix prompts you to trust these
  # the first time; after that it Just Works.
  nixConfig = {
    extra-experimental-features = [
      "ca-derivations"
      "dynamic-derivations"
      "configurable-impure-env"
    ];
    extra-system-features = [ "builder-rpc-v0" ];
  };

  outputs = { self, nixpkgs, nixgg }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      mkNixggBuild = nixgg.packages.${system}.mkNixggBuild;
    in
    {
      packages.${system}.default = (mkNixggBuild {
        pname = "myproject";
        version = "0.1.0";
        src = ./.;
        target = "myproject";      # basename the link shim submits as `out`
        nativeBuildInputs = [ pkgs.pkg-config ];
        buildInputs = [ pkgs.zlib ];
        buildCommand = ''
          make -j"$NIX_BUILD_CORES"
        '';
      }).result;
    };
}
```

You also need a Nix that implements `builder-rpc-v0` and `nix store
submit-output` — that work lives on NixOS/nix master and is not in a
released Nix yet. This flake exposes the build it pins:

```sh
# One-time: get a capable nix (substituted from cache.nixos.org).
nix build github:tomberek/nixgg#patched-nix -o ./.patched-nix

# Build your project with it.
./.patched-nix/bin/nix build .
```

Stock Nix is fine for nixgg's **native** mode (thunks on disk, one
`nix build` at the end); the patched Nix is only needed to consume a
`mkNixggBuild` result, which is sandbox mode.

See `examples/*/default.nix` for real-world call sites (lua, {fmt},
mosh, redis, ffmpeg, and a 3-phase LLVM build). If your build execs one
of its own binaries mid-build — codegen and bootstrap tools do this —
read `examples/llvm/default.nix`: that needs two or more chained
`mkNixggBuild` calls, since a not-yet-realised output can't be run.

`mkNixggBuild`'s parameters:

| param | required | meaning |
|---|---|---|
| `pname` | yes | naming only |
| `version` | no (default `"0"`) | naming only |
| `src` | yes | the source tree |
| `target` | yes | path of the final artifact; its basename is matched against the link/archive shim's `-o` to decide what gets submitted as the derivation's output |
| `buildCommand` | yes | shell run inside the sandbox once shims are on `PATH` — typically `make`/`cmake --build`/`ninja` |
| `nativeBuildInputs` | no | build-time tools (compilers, generators, `pkg-config`) |
| `buildInputs` | no | libraries the build links against |
| `propagatedBuildInputs` | no | passed through to the underlying `stdenv.mkDerivation` |

Every `mkNixggBuild` call also returns a `.shell` — a plain `mkShell`
mirroring the sandbox's exact `stdenv` environment. `nix develop` into
it when you need to reproduce a sandbox build by hand; it's what
`tests/drv-equivalence.sh` uses to run the native side under the same
tool env.

## Upgrade an existing nixpkgs package

`mkNixggBuild` is for builds you write yourself. `dynDrvStdenv` is for
builds nixpkgs already wrote — any ordinary `stdenv.mkDerivation`
package gets the builder-rpc-v0 treatment with a one-line `override`,
no rewriting its `package.nix`. Same prerequisites as `mkNixggBuild`
(see [Use it in your own project](#use-it-in-your-own-project) above
for the `nixConfig` block and `patched-nix`):

```nix
{ pkgs, nixgg }:

let
  dynDrvStdenv = nixgg.packages.${pkgs.system}.dynDrvStdenv { stdenv = pkgs.stdenv; };
in
pkgs.hello.override { stdenv = dynDrvStdenv; }
```

Tested directly against real nixpkgs packages spanning the common
build-system shapes — `hello` (autotools), `mosh` (autotools +
`autoreconfHook`), `zstd` (cmake, 4 outputs, custom `checkPhase`
running `ctest`, plus a package that execs one of its own binaries
mid-build — see below). All build, install to the right outputs, run
real per-translation-unit shim acceleration (every `cc`/`c++`/`ar`
call becomes its own content-addressed derivation, same as
`mkNixggBuild`), and pass their own `installCheckPhase`/`checkPhase`
unmodified.

### How it works

`dynDrvStdenv` overrides `mkDerivationFromStdenv` — the same seam
nixpkgs' own `pkgsMusl`/`pkgsStatic`/ccache use, just with a much
smaller radius: it changes how a package's derivation gets *built*,
not the toolchain or the whole package set. Every override is scoped
to the one package you apply it to via `.override { stdenv = ...; }`;
nothing else in your `pkgs` set changes.

Under the hood it splits `stdenv.mkDerivation` into two real
derivations:

1. **Phase 1** (`unpackPhase` through `buildPhase`) runs as a
   `builder-rpc-v0` sandboxed derivation with nixgg's shims live on
   `PATH` — real `configurePhase`, real setup hooks (`autoreconfHook`,
   `cmake`, ...), real `make`/`ninja`, whatever the package actually
   does, with every `cc`/`c++`/`ar` invocation turned into its own
   dynamic derivation exactly like `mkNixggBuild`. `nixgg assemble`
   then walks the resulting tree, resolves every shimmed output, and
   submits the whole tree as one dynamic derivation output.
2. **Phase 2** (`checkPhase` through `distPhase`) is an ordinary
   derivation seeded from phase 1's fully-resolved tree, running the
   package's own unmodified `checkPhase`/`installPhase`/`fixupPhase`/
   `installCheckPhase`/`meta` — so multi-output splitting, RPATH
   shrinking, `ctest`/test-suite execution, and install-time checks
   all still work exactly as nixpkgs wrote them, against real
   binaries (not unresolved stubs).

### Packages that exec their own binaries mid-build

A handful of build systems (cmake's `add_custom_target(... DEPENDS
some-tool)` being the common case) compile a helper tool and
immediately exec it as part of the same build — zstd's
`contrib/gen_html` renders `zstd_manual.html` this way. Inside a
`builder-rpc-v0` sandbox that fails: the shim's link step for the
helper leaves an unresolved placeholder in place of a real executable
(nothing inside the sandbox resolves dynamic-derivation outputs
synchronously), so `./gen_html` errors with "Permission denied".

Fix it with the same phase-chaining pattern `mkNixggBuild`'s own
`examples/two-phase` and `examples/llvm` already use: build the helper
standalone via `mkNixggBuild` first, then patch the wrapped package's
build graph to call that already-resolved binary instead of building
its own. The patch has to go through `dynDrvStdenv`'s
`extraPhase1Attrs` parameter, not a plain `.overrideAttrs` — nixpkgs'
own `.override`/`.overrideAttrs` reapplication contract always
re-invokes the package function with its *original*, unpatched attrs
first, so an attrs-level patch applied via `.overrideAttrs` never
reaches phase 1. `extraPhase1Attrs`/`extraPhase2Attrs` are spliced in
before phase 1 is computed, at the `dynDrvStdenv { ...; }` call site
itself:

```nix
{ pkgs, mkNixggBuild, dynDrvStdenv }:

let
  genHtml = mkNixggBuild {
    pname = "zstd-gen-html";
    version = "0";
    src = pkgs.zstd.src;
    target = "gen_html";
    buildCommand = ''
      cd contrib/gen_html
      g++ -O2 -c gen_html.cpp -o gen_html.o
      g++ gen_html.o -o gen_html
    '';
  };
in
pkgs.zstd.override {
  stdenv = dynDrvStdenv {
    stdenv = pkgs.stdenv;
    extraPhase1Attrs = finalAttrs: old: old // {
      postPatch = old.postPatch + ''
        substituteInPlace build/cmake/contrib/gen_html/CMakeLists.txt \
          --replace-fail \
            'add_executable(gen_html ''${GENHTML_DIR}/gen_html.cpp)' \
            "" \
          --replace-fail \
            'DEPENDS gen_html COMMENT "Update zstd manual")' \
            'COMMENT "Update zstd manual")' \
          --replace-fail \
            'set(GENHTML_BINARY ''${PROJECT_BINARY_DIR}/gen_html''${CMAKE_EXECUTABLE_SUFFIX})' \
            'set(GENHTML_BINARY ${genHtml.package}/bin/gen_html)'
      '';
    };
  };
}
```

See [examples/zstd-dyndrv/default.nix](examples/zstd-dyndrv/default.nix)
for the full, tested version. `extraPhase1Attrs`'s `old` is phase 1's
own attrset as `dynDrvStdenv` built it (already carrying the real
package's `postPatch`, plus dynDrvStdenv's own shim-activation
`postPatch`/`preBuild`) — not the raw, unmodified `package.nix`
attrs — so appending to `old.postPatch`, as above, preserves
everything already there. `extraPhase2Attrs` is the same shape for
phase 2, mostly for symmetry: phase 2 *is* reachable via an ordinary
`.overrideAttrs` on the returned package — only phase 1 has the
reapplication problem.

If you already have a package built through `dynDrvStdenv` and want
to patch phase 1 without rebuilding the `dynDrvStdenv { ...; }` call
site, use `.overridePhase1Attrs` — a `passthru` on the returned
package with the same `finalAttrs: old: old // {...}` shape as
`extraPhase1Attrs`, composing on top of it:

```nix
(pkgs.openssl.override { stdenv = dynDrvStdenv; }).overridePhase1Attrs (
  finalAttrs: old: old // {
    postPatch = old.postPatch + "sed -i ... crypto/mem.c\n";
  }
)
```

Same underlying cost as rebuilding the `dynDrvStdenv { ...; }` call
site with an `extraPhase1Attrs` — phase 1 is reconstructed either
way, by construction — just without hand-building a second
`dynDrvStdenv` instance to get there.

### Measured incremental-rebuild cost

Per-TU acceleration only pays off when most translation units are
actually unchanged. Two real measurements against openssl (`~2200`
translation units) through `dynDrvStdenv`, both using
`pkgs.openssl.override { stdenv = dynDrvStdenv { stdenv = pkgs.stdenv; }; }`
as the baseline already built once:

| Scenario | What changed | TUs recompiled | Non-TU drvs freshly built |
|---|---|---|---|
| Baseline (cold) | first build of 3.6.3 | 2213 / 2213 | everything |
| One-file patch (the CI case) | one real declaration added to `crypto/mem.c`, via `.overridePhase1Attrs`'s `postPatch` | 2 / 2213 (`tu-libcrypto-lib-mem.o`, `tu-libcrypto-shlib-mem.o`) | 13 — the 7 engine `.so`s, `bin-libssl.so.3`/`bin-libcrypto.so.3`/`bin-openssl` that link the changed object, plus the outer wrapper drvs |
| Version bump (3.6.3 → 3.5.7) | full release bump, same package | 2153 / 2192 (98%) | effectively everything |

The one-file-patch case is the realistic CI scenario this project
targets: a security patch or small bugfix on an unchanged release only
needs a compiler for the 2 touched TUs out of 2213; everything else is
a build-trace cache hit. The 13 non-TU drvs that DO rebuild are real,
correct propagation of a real content change — verified by diffing the
exact drv hashes actually built in each run (zero overlap between the
baseline and patched runs' "actually built" sets). An earlier attempt
patched in only a comment line; GCC emits nothing for a comment, so
the `.o` was byte-identical to the unpatched one and the test measured
nothing — fixed by adding a real declaration instead.

The version-bump row is the negative result: openssl's own `Configure`
bakes its version number into `opensslv.h`, a header nearly every `.c`
file transitively includes, so almost nothing survives a release bump
byte-for-byte even though most source is unchanged text. Per-TU
content addressing can't distinguish "this file changed" from
"something it includes changed" — both invalidate the cache, so the
win size depends on the diff's *object-level* footprint, not its line
count.

See `~/nixgg-example`'s `flake.nix` (a separate flake using this repo
as a library, not part of this repository) for the three openssl
variants these numbers came from: `openssl-dyndrv`,
`openssl-3-5-dyndrv`, and `openssl-dyndrv-patched`. Its own
`tests/measure-incremental-rebuild.sh` automates the one-file-patch
row against openssl specifically, but lives outside this repo and its
CI. [tests/perf-regression.sh](tests/perf-regression.sh) automates the
same property in-repo, on a cheap fixture (lua, 34 TUs) that runs on
every push/PR: edit one `.c` file, assert exactly its own translation
unit recompiles and all 33 others stay cache hits. The binary
same-path/different-path cutoff checks above can't tell "2 TUs
rebuilt" apart from "every TU rebuilt" — this can.

## Cache an existing package's configure step

`dynDrvStdenv` accelerates the whole build/install split. Sometimes
you don't need that much — you just want configure to stop rerunning
every time you touch something downstream, like `installFlags` or a
`postInstall`. `configureCacheStdenv` splits `stdenv.mkDerivation` at
the configure/build boundary instead. It doesn't need the
`builder-rpc-v0` sandbox at all, because configure isn't compiling
anything unknown — there's nothing to shim:

```nix
{ pkgs, nixgg }:

let
  configureCacheStdenv = nixgg.packages.${pkgs.system}.configureCacheStdenv;
in
pkgs.hello.override { stdenv = configureCacheStdenv { stdenv = pkgs.stdenv; }; }
```

Changing anything after configure — `installFlags`, `postInstall`,
whatever — no longer reruns or rehashes configure. Configure's own
output is also content-addressed, so if a rerun happens to produce
identical output, the build after it doesn't rerun either.

For the stronger case — an unrelated source-file edit shouldn't rerun
configure at all — pass `configureSrcFilter` to shrink configure's own
`src` down to just the files it reads, via a small content-addressed
filter derivation. (Not `lib.fileset`: that needs a real path at eval
time, and most packages' `src` is a fetcher derivation that hasn't
been built yet.)

```nix
let
  configureSrcFilterPresets = nixgg.packages.${pkgs.system}.configureSrcFilterPresets;
in
pkgs.hello.override {
  stdenv = configureCacheStdenv {
    stdenv = pkgs.stdenv;
    configureSrcFilter = {
      includePatterns = configureSrcFilterPresets.autotools;
      existenceStubs = [ "src/hello.c" ]; # hello's own AC_CONFIG_SRCDIR arg
    };
  };
}
```

`configureSrcFilterPresets` ships starting points for `autotools` and
`cmake`, and they're just that — starting points, not guarantees. An
under-inclusive pattern list doesn't fail loudly; it silently caches a
stale configure output. Test by building the package, not by reading
the pattern list. `hello` alone needed more than the obvious
`Makefile.am`/`configure.ac` — also `*.in`/`*.mk` templates, and its
whole `po/` directory, which is easier to include wholesale than to
enumerate. `existenceStubs` covers the opposite case: a file that's
only ever checked for existence, never read, like autoconf's
`AC_CONFIG_SRCDIR` — safe to stub out as empty.

Not every cmake package can use `configureSrcFilter` at all: if a
package's own `CMakeLists.txt` collects its sources with
`file(GLOB ...)` over a whole directory (zstd does this), configure
needs that entire directory present regardless of any filter — there's
no smaller include list that helps. `fmt` lists its sources
explicitly, so filtering it actually does something; see
`.#fmt-cache-filtered` for the working example, and
[tests/configure-cache-cutoff.sh](tests/configure-cache-cutoff.sh) for
an automated check that the caching itself holds (not just that the
build succeeds): it edits a file the filter excludes and one it
includes, and asserts configure's own output only changes for the
latter.

## Combining both: skip configure AND get per-TU acceleration

`dynDrvStdenv` and `configureCacheStdenv` split a package at different
boundaries, but the same package can use both. `dynDrvStdenv`'s
configure step runs with nixgg's shims live but bypassed — every shim
call is a plain passthrough exec, not a sandboxed one — so configure
doesn't actually need the sandbox it happens to run inside. Pull it
into its own `configureCacheStdenv`-shaped group, and the sandboxed
group only has to do the build:

```nix
{ pkgs, nixgg }:

let
  dynDrvConfigureCacheStdenv = nixgg.packages.${pkgs.system}.dynDrvConfigureCacheStdenv;
  configureSrcFilterPresets = nixgg.packages.${pkgs.system}.configureSrcFilterPresets;
in
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

Three derivations instead of two: configure (plain, unsandboxed,
early-cutoff via `configureSrcFilter` — same as `configureCacheStdenv`
above), build (sandboxed, real per-TU shim acceleration — same as
`dynDrvStdenv`), install-onward (unmodified, copied from
`dynDrvStdenv`'s own install group). An edit to a file configure
never reads skips configure entirely; an edit to a source file only
recompiles that file's own dynamic derivation. Tested against `hello`
(autotools, with `configureSrcFilter`), `zstd` (cmake, 4 outputs, real
`ctest` checkPhase, and the `gen_html` mid-build-exec fix — patched on
both the configure and build group, since cmake's Makefile generation
happens in the former), and `gdbm` (autotools, 5 outputs, covering
multi-output + `configureSrcFilter` together). See
[.#hello-dyndrv-configure-cached](flake.nix) and
[.#zstd-dyndrv-configure-cached](flake.nix) in `flake.nix`.

## Architecture

Every shim writes a derivation. Nix does the rest. See
[ARCHITECTURE.md](ARCHITECTURE.md) for the design and the shim
mechanics. See [dyn-drv/NOTES.md](dyn-drv/NOTES.md) for the
sandbox / dynamic-derivation exploration notes.

### Talking to the daemon directly instead of shelling out

Worth exploring if you're picking at where sandbox mode's time goes:
every shim call in sandbox mode used to fork+exec the `nix` CLI once
per operation (`nix derivation add`, `nix store add --scan`, `nix
store submit-output`) — cheap in isolation (~20-90ms), but it adds up
linearly with translation-unit count on a warm rebuild, where real
compiler time isn't the bottleneck anymore.

The Nix C API (`libnixstorec`/`libnixutilc`, pinned in this flake as
`.#nix-store-c`/`.#nix-util-c`) only reaches one of the three calls —
`builder-rpc-v0`'s `SubmitOutput` and `AddToStoreScanning` ops have no
C binding at all. The alternative nixgg landed on instead:
`internal/rpc` speaks the Nix worker protocol directly over the
sandbox's own daemon socket (the same `NIX_REMOTE=unix://…` the CLI
would've connected to anyway), with `internal/aterm` and
`internal/nar` rendering the exact bytes `nix derivation add`/`nix
store add --scan` compute internally — read directly out of the
pinned Nix source and verified byte-for-byte against real output
before being wired in.

Measured on lua (34 TUs), isolating shim-pass overhead from real
compile time (single-file edit, warm store, substituters off, 5 runs
averaged each way): **~1.46s with the RPC path vs. ~2.83s via the CLI
fallback — roughly 48% faster.** The win scales with TU count and
rebuild frequency, not with compiler time, so it matters most on
exactly the workload per-TU acceleration targets — a large project's
edit/rebuild loop — and nearly vanishes on a cold build, where real
`g++` invocations dominate either way.

On by default (`NIXGG_RPC=1` in every sandbox mechanism's env block);
`NIXGG_RPC=0` falls back to the CLI path if something unforeseen turns
up. `tests/drv-equivalence.sh`'s full sweep (149 drvs) and
`tests/smoke.sh EXAMPLES=all` both pass with it on.

### Optional: a persistent helper to amortize the daemon handshake

Even on the direct-RPC path above, every shim invocation still opens
its own connection to the real Nix daemon and pays a full handshake —
measured at **~4.3ms, 99% of a direct RPC call's own cost**, versus
**~23µs** for an op on a connection that's already open. And it's paid
*twice* per compile today, since `DerivationAdd`/`StoreAddScan` each
dial independently.

`mkNixggBuild`'s optional `rpcHelper = true;` starts a small persistent
process (`nixgg helper`, `go/internal/helper`) once per build, in
`preBuild`, and every shim relays its three ops through it instead of
dialing the daemon directly. The helper holds a small *pool* of
already-handshaken daemon connections — sized to `$NIX_BUILD_CORES` —
not a single shared one: the Nix worker protocol is strictly
request/response per connection (confirmed against the real Nix C++
client, which itself pools connections rather than multiplexing ops
over one socket), so a lone shared connection would serialize a
`make -j` build's concurrent shim calls against each other.

```nix
mkNixggBuild {
  # ...
  rpcHelper = true;
}
```

Verified end-to-end on both a single-drv build (`.#hello-helper`) and
a real `make -j$NIX_BUILD_CORES` build with genuine shim-call
concurrency (mosh, 30 TUs + 6 archives) — byte-identical drv hashes to
every other path, correct output, clean shutdown. Off by default:
this is a newer, smaller-blast-radius idea than the direct-RPC path
above, worth exploring further rather than defaulting on yet. See
`go/internal/helper`'s own docs for the pool/protocol design.

## Requirements

- Nix ≥ 2.36 for sandbox mode (needs `builder-rpc-v0` + `nix store
  submit-output`, both merged into NixOS/nix master via
  [#15793][pr-15793]). Native mode works with older Nix.
- The flake pins its own Nix build; `nix develop` bootstraps it, or
  `nix build .#patched-nix` if you would rather invoke it directly —
  see [Invoking sandbox mode explicitly](#invoking-sandbox-mode-explicitly)
  for the full flag set and what each flag is for.

[pr-15793]: https://github.com/NixOS/nix/pull/15793

## Prior art

Inspired by **[gg][gg]** (Stanford SNR, [ATC '19: *From Laptop to
Lambda*][gg-paper]). gg models every build step as a
content-addressed thunk that a scheduler can dispatch to a cluster
of workers. nixgg keeps the model but drops the scheduler in favor
of the one nixOS already ships — the Nix store, its evaluator, and
its remote-build machinery. Every gg thunk becomes a Nix derivation;
every gg fingerprint becomes a Nix output path.

Related work in the same shape:

- **[nix-ninja](https://github.com/pdtpartners/nix-ninja)** —
  emits dynamic derivations from Ninja build graphs. Similar
  sandbox mechanism (builder-rpc-v0), rust implementation, targets
  the meson/cmake→ninja pipeline.
- **[sandstone](https://github.com/obsidiansystems/sandstone)** —
  Haskell-module-per-derivation via `recursive-nix`.
- **[NixOS/nix#15793](https://github.com/NixOS/nix/pull/15793)** —
  the upstream PR that added `builder-rpc-v0` + `nix store
  submit-output`. Now merged into master.

## License

MIT. See [LICENSE](LICENSE).

[gg]: https://github.com/StanfordSNR/gg
[gg-paper]: https://www.usenix.org/conference/atc19/presentation/fouladi
