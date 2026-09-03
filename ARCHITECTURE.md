# nixgg architecture

nixgg is a build accelerator that treats every `-c` compile, `ar`
archive, and link invocation as a content-addressed **Nix
derivation**. You run make / cmake / ninja as usual; nixgg shims sit
on PATH in place of the real compiler; each `cc`/`c++`/`ar` call
turns into a Nix derivation. Nix decides what's cached and what
needs building — nixgg just constructs the expressions.

Two modes for producing those derivations:

- **Native mode** (default): shims write `.nix` thunk files on disk;
  `nixgg force` (or the link shim's `NIXGG_AUTOFORCE=1` hook) does
  one `nix build --file <helper> …` at the end. This is the "make
  it work on my machine" path — no experimental Nix features, works
  against any recent daemon.
- **Sandbox / dyn-drv mode** (`NIXGG_SANDBOX=1`, requires the
  builder-rpc-v0 Nix from [#15793][pr], now on master): shims use
  `nix derivation add` to register derivations directly with the
  daemon. The link shim calls `nix store submit-output` for the
  final target. `mkNixggBuild` wraps the resulting `builtins.outputOf`
  string (which has no `.type`/`.drvPath` and so cannot be `nix run`
  or `nix profile install`ed) in one ordinary `stdenv.mkDerivation`
  that copies the bytes out — the `.package` attribute, which is what
  every example's flake output actually is. This is what makes
  `nix build .#hello`, `.#lua`, `.#fmt`, `.#mosh`, and `nix run` on
  any of them, Just Work from a regular flake. The raw string is
  still reachable at `.#hello.result` for anything that wants it.

The whole tool is Go, ships as a single static ELF (~2.9 MB), no
CGO, no third-party deps.

[pr]: https://github.com/NixOS/nix/pull/15793

## The invariant

> Every shim writes a derivation. Nix decides everything else.

That's the only rule. It's what makes native mode and sandbox mode
share the same call sites, and it's what makes reasoning about
correctness simple.

The one carveout is autoconf/cmake probes (`conftest*`,
`CMake*Compiler*`, `Check*Exists`). Those need a runnable output for
the configure logic to `if ./conftest; then …`. In those specific
cases the compile shim realises synchronously via a per-invocation
`nix build --file`. `mode.For(path)` decides — no env var involved,
purely filename-driven.

## User-facing entry points

```bash
# native mode — plain make, shims write thunks, force at end
eval "$(nixgg env)"
export NIXGG_AUTOFORCE=1        # optional; link shim realises inline
make -j$(nproc)

# sandbox / dyn-drv mode — via flake
nix build .#hello               # produces hello-0/bin/hello ELF
nix build .#lua                 # produces lua-0/bin/lua ELF (Lua 5.4.7)
nix build .#fmt                 # produces libfmt.a-0/lib/libfmt.a
nix build .#mosh                # produces mosh-server-0/bin/mosh-server ELF
nix run .#hello                 # same artifact, run directly
```

Flake packages currently exposed:

- `nixgg-bin` — the Go binary + shims tree, built via `buildGoModule`.
- `mkNixggBuild` — the Nix function that wraps a user build command
  in a `builder-rpc-v0` derivation whose output is a `.drv` file
  (dynamic derivation). Built on top of `stdenv.mkDerivation`, so
  `buildInputs` / `nativeBuildInputs` / `propagatedBuildInputs` and
  standard setup hooks (cmake, autoreconf, pkg-config) Just Work.
- `hello`, `lua`, `fmt`, `mosh`, `redis`, `ffmpeg`, `two-phase`,
  `llvm` — concrete call sites of `mkNixggBuild`, exposed as
  `.package`: a real derivation, so `nix build`/`nix run`/
  `nix profile install` all work normally. `.#<name>.result` reaches
  the raw `builtins.outputOf` string underneath, for anyone who wants
  it directly. Driven from an `exampleDefs` table in flake.nix, so
  each is one entry rather than three hand-written attrs.
- `<name>-shell` for each of the above — plain `mkShell`s mirroring
  that example's stdenv env. `nix develop .#<name>-shell` gives native
  mode the exact same buildInputs / setup-hooks the sandbox has; used
  by `tests/drv-equivalence.sh`.
- `llvm-min-tblgen`, `llvm-tblgen`, `two-phase-codegen` — intermediate
  phases, separately buildable so a phase chain can be smoke-tested one
  step at a time.
- `env-shell`, `patched-nix`, `nixgg-nix` (helper package),
  toolchain roots — supporting bits.

## CLI

Two subcommands. Everything else was cargo-culted from an older
design and has been removed.

- **`nixgg env`** — prints a shell-sourceable block: NIXGG_* env
  vars, PATH prefix, CC/CXX. `eval "$(nixgg env)"` is the bootstrap.
- **`nixgg force [--roots] <target…>`** — escape hatch. Native mode
  only. Realises thunks left on disk after a build that ran without
  `NIXGG_AUTOFORCE=1`. `--roots` picks up any leaf thunk (not
  imported by any other) and forces the manifest of caller-visible
  symlinks pointing at it.

Shim entry (busybox-style: argv[0] ∈ {cc, gcc, c++, g++, ar, ranlib}
dispatches to the corresponding internal handler).

## Binary layout

```
nixgg/
├── go/                         the entire Go tree — flake's `src = ./go`
│   ├── main.go                 argv[0] dispatch
│   ├── go.mod                  no external deps; stdlib only
│   └── internal/               (listed below)
├── bin/nixgg                   built static ELF (git-ignored)
├── shims/                      symlinks: cc, gcc, c++, g++, ar, ranlib → ../bin/nixgg
├── nix/
│   ├── builder.nix             per-TU CA derivation (native mode)
│   ├── linker.nix              link CA derivation (native mode)
│   ├── archiver.nix            ar CA derivation (native mode)
│   ├── resolve-script.nix      fills @NIXGG_*@ markers in Go's script
│   ├── pure-store-path.nix     pure-eval-safe builtins.storePath
│   ├── toolchain.nix           GENERATED by the flake
│   └── mkNixggBuild.nix        stdenv.mkDerivation wrapper for dyn-drv;
│                               also exposes `.shell` (plain mkShell
│                               mirroring the same stdenv env)
├── flake.nix / flake.lock      pinned nixpkgs; produces env-shell,
│                               nixgg-bin, an exampleDefs-driven set of
│                               examples (+ a -shell each), mkNixggBuild, …
├── examples/                   out-of-tree examples driven from flake inputs
│   ├── lua/default.nix         lua 5.4.7 — plain `make linux CC=cc`
│   ├── fmt/default.nix         {fmt} 11.0.2 — cmake + ninja + libfmt.a
│   ├── mosh/default.nix        mosh — autoconf + protobuf + ncurses/openssl/zlib
│   ├── redis/default.nix       redis 8.2.2 — plain Makefile, nested deps/
│   ├── ffmpeg/default.nix      ffmpeg 7.1.2 — bespoke configure, ~1200 TUs
│   ├── two-phase/              minimal phase-chain smoke test (codegen + app)
│   └── llvm/default.nix        llvm 19.1.7 — 3-phase (tblgen ×2 → llc)
├── dyn-drv/                    dyn-drv exploration + test fixtures
│   ├── NOTES.md                what we learned about builder-rpc-v0
│   ├── config.nix              test helper
│   ├── dyn-one-layer.nix       smallest dyn-drv chain (no sandbox)
│   ├── dyn-json-drv.nix        working `nix derivation add` fixture
│   ├── hello-mkbuild.nix       hello.cc via mkNixggBuild
│   └── nixgg-sandbox.nix       builder-rpc-v0 sandbox fixture
├── example/                    smoke-test Makefile (main.cc + util.cc)
├── tests/
│   ├── drv-equivalence.sh      native ↔ sandbox drv-hash regression test
│   └── smoke.sh                every example builds, lands at its FHS
│                               path, and runs (what the hash test can't see)
└── go/internal/
    ├── dispatch/               argv[0] classification, @rspfile expansion
    ├── mode/                   placeholder vs. realise (filename patterns)
    ├── toolchain/              NIXGG_* env loading
    ├── paths/                  .nixgg/{thunks,srcs,scans,symlinks,promoted}/
    ├── stage/                  hardlink staging (src+headers)
    ├── scan/                   gcc -MM -MG + mtime cache
    ├── wrapperenv/             capture NIX_CFLAGS_COMPILE etc into JSON
    ├── storedeps/              regex /nix/store/... refs from flags/env
    ├── classify/               resolve symlink → Store/Thunk/Drv/Regular/Absent
    ├── drvref/                 sandbox-mode stub format (single definition)
    ├── expr/                   Nix expression + JSON drv emitters
    ├── thunk/                  hash + write .nix, symlink output
    ├── sandbox/                nix derivation add / store add / submit-output
    ├── realise/                DAG-walk + one `nix build` at end (native)
    ├── shim/                   compile / link / archive entrypoints
    └── cli/                    env / force
```

Total ~4700 lines of Go, no CGO, no third-party deps.

## The `.nixgg/` workspace (native mode)

Every project accumulates a `.nixgg/` tree at its root. `paths.Resolve`
picks the workspace root by walking up from `$PWD`:

1. `$NIXGG_THUNKS_DIR` if set.
2. Nearest ancestor containing an existing `.nixgg/`.
3. Nearest ancestor with `.git`; auto-seed `.nixgg/` there.
4. `$PWD/.nixgg/`.

Recursive-submake projects (deps/*, lib/, programs/) all converge on
one thunks dir without extra plumbing.

```
.nixgg/
├── thunks/     ← <thunk-id>.nix files (one per derivation)
├── srcs/       ← <tu-id>/ dirs, hardlinks of source + headers
├── scans/      ← <scan-key>.{out,err,deps} — scan-headers memoization
├── symlinks/   ← <thunk-id> manifest — caller-visible paths → me
└── promoted/   ← <sha1(abs-path)> — regular file X was built from thunk Y
```

### `thunks/<thunk-id>.nix`

Content-addressed Nix expression for one compile/link/archive.
`<thunk-id>` = `sha256(expression body)[:32]` — same expression body
→ same file, so writes are idempotent.

```nix
import /nix/store/…-nixgg-nix/builder.nix {
  srcTree        = /abs/path/.nixgg/srcs/hello-fbb4837994e7;
  source         = "hello.cc";
  outName        = "hello.o";
  scriptTemplate = ''set -euo pipefail
export PATH="@NIXGG_COREUTILS@/bin:@NIXGG_COMPILER@/bin"
mkdir -p "$out"
cd "$src"
"c++" '-O2' '-I.' -c "$source" -o "$out/$outName"
'';
  markerTag      = "NIXGG";
  storeDepsJSON  = ''[…]'';
  wrapperEnvJSON = ''{…}'';
}
```

The shell body is verbatim Go output — the helper does not assemble it.
Only the `@NIXGG_*@` markers are substituted, because those stand for
values Nix alone knows at eval time: the helper's own toolchain
arguments, and (for link/archive) inputs that may be unrealised sibling
thunks whose CA output placeholders don't exist until instantiation.

Sandbox mode bakes the same body, from the same Go code, straight into
its JSON drv. That is why there is one implementation of the command
rather than two that must be kept in sync — see `nix/resolve-script.nix`,
and `internal/expr/derivation.go`'s `buildScript`.

`markerTag` is not always `NIXGG`. Markers are plain text, so a flag
spelling one (`-DAT=@NIXGG_COMPILER@`) would be substituted too; Go picks
the first unoccupied `NIXGG`/`NIXGG1`/`NIXGG2`/… for each script and
passes it along.

Paths are absolute. Two consequences:

- `cp` of a thunk symlink to a peer directory (a real Makefile pattern
  — e.g. `cp obj/foo dest/foo`) is non-destructive; the copy's
  imports still resolve.
- `nix build --file <any-thunk-path>` works from any cwd.

Trade-off: `.nixgg/` no longer moves with `mv`. Rename a workspace →
`rm -rf .nixgg && rebuild`. Store outputs are unaffected because
absolute vs. relative paths in a Nix expression produce byte-identical
CA hashes (Nix hashes the *value* of the import, not its spelling).

### `srcs/<tu-id>/`

Hardlinks of source + every header the scanner discovered.
`<tu-id>` = `<slug>-<12 hex>`, hex = `sha256(abs-output)[:12]`.
Prevents collisions when two compiles from different cwds produce
outputs with the same basename (redis has both `src/sds.o` and
`deps/hiredis/sds.o`).

Reuse check: for every (abs, staged-rel) pair, `stat` both and compare
inodes. Hardlink → same inode. Inode mismatch or file-set mismatch →
`rm -rf` and repopulate.

### `scans/<key>.{out,err,deps}`

Cache of `gcc -MM -MG`. Key = `sha256(compiler + source + sorted flags)`.
`.deps` records `<abs>\t<mtime-nsec>` per referenced file; next lookup
batch-stats and compares.

### `symlinks/<thunk-id>`

Append-only manifest of caller-visible symlinks pointing at this
thunk. Read by force to promote them all in one pass. Stale entries
filtered at read time.

### `promoted/<sha1(abs)>`

Two-line file: `<thunk-id>\n<store-path>`. Written by
`realise.PromoteToStore` after copying store bytes into the working
tree. Read by `classify.Target` when a link/ar shim sees a regular
file on disk — "was this a nixgg-produced file? which thunk?"

**Why bytes not symlinks**: Nix pins store-path mtimes to 1969. A
symlink to `/nix/store/…` shows an ancient mtime under `stat(2)`, so
make would treat the .o as older than the .c and recompile forever.
Byte-copies let `chtimes(now)` make the dependency check correct.

## Flow: one compile shim invocation (native mode)

```
make → cc (symlink to nixgg binary)
  │
  ▼
main.go: argv[0]=cc → dispatch.ToolCC → shim.Compile
  │
  ├─ parseCompileArgs: source, output, flags (dropping -M* sandbox-invalid)
  │
  ├─ scan.Run: gcc -MM -MG (cached under .nixgg/scans/)
  │    → headers, project root, staged -I flags, store -I flags
  │
  ├─ stage.TUID(abs-output), stage.Sources:
  │    reuse .nixgg/srcs/<tu-id>/ if inodes match; else rm+hardlink
  │
  ├─ rewriteFlags, wrapperenv.JSON, storedeps.From
  │
  ├─ expr.Compile → Nix expression string (byte-deterministic)
  │
  ├─ if mode.For(source) == Realise:  (autoconf/cmake probe)
  │    → thunk.Write + nix build --file … --print-out-paths
  │    → link output → /nix/store/…-tu-foo.o/foo.o
  │
  └─ else (placeholder — the common path):
       ├─ thunk.Compute → thunk-id
       ├─ thunk.Write   → .nixgg/thunks/<id>.nix (idempotent)
       ├─ thunk.LinkPlaceholder → output → thunks/<id>.nix
       └─ thunk.RecordSymlink   → .nixgg/symlinks/<id> += output
```

No fork per shim beyond the one `gcc -MM -MG` (scan-cached). The
link shim additionally, if `NIXGG_AUTOFORCE=1`, calls
`realise.Realise` on its just-written link thunk — batches the whole
DAG into one `nix build`.

## Flow: sandbox mode (`NIXGG_SANDBOX=1`)

Compile / archive / link shims all follow the same shape, differing
only in which JSON emitter they use.

```
shim runs (inside builder-rpc-v0 sandbox)
  │
  ├─ stage.Sources: hardlink source+headers into $TMPDIR/.nixgg/srcs/<tu-id>/
  │
  ├─ sandbox.StoreAddScan (compile only):
  │    nix store add --scan -n src-<name> <staged-dir>
  │    → returns /nix/store/…-src-<name>
  │
  ├─ expr.CompileJSON / LinkJSON / ArchiveJSON:
  │    Assemble the JSON derivation description Nix's `nix derivation add`
  │    accepts. inputs.drvs entries reference upstream .drv basenames;
  │    inputs.srcs uses store-path basenames (NOT full /nix/store/... —
  │    the parser rejects the leading slash).
  │
  ├─ sandbox.DerivationAdd:
  │    echo <json> | nix derivation add
  │    → returns /nix/store/…-<name>.drv
  │
  ├─ sandbox.PointOutputAtDrv: write a drvref stub at the caller-visible
  │    output path. Small regular file with a magic header + the drv
  │    path (NOT a symlink, because builder-rpc-v0 registers .drv files
  │    with the daemon but doesn't materialise them into the sandbox
  │    filesystem — a symlink target would dangle and fail `test -e`
  │    checks in downstream Makefile prerequisites, e.g. mosh's
  │    `mosh-client: ../crypto/libmoshcrypto.a`). classify.Target reads
  │    the header to recover the drv path → returns Kind.Drv; downstream
  │    link/archive shims include it under their inputs.drvs.
  │
  └─ link shim additionally, iff basename matches NIXGG_SANDBOX_TARGET:
     sandbox.SubmitOutput: nix store submit-output <drv> out
     → registers this drv as the outer derivation's `out`.
```

The outer `mkNixggBuild` derivation is marked `outputHashMode =
"text"`, name ending in `.drv`. Its output IS the submitted `.drv`
file. Consumers reach the compiled artifact via
`builtins.outputOf outer.outPath "out"` — Nix walks: build outer →
read its output (a `.drv`) → build that inner drv → return its
output.

For the exact placeholder digest algorithm (nix32-encoded sha256 of
`"nix-upstream-output:<drvHashPart>:<pathName>"`), see
`expr.caOutputPlaceholder` — verified byte-exact against
`builtins.outputOf` via a pinned test vector.

## Placeholder vs. Realise (mode.For)

The compile shim decides per-TU whether to defer (Placeholder) or
realise synchronously. Purely filename-driven, no env var:

- `conftest*` → autoconf configure probe.
- `test?Compiler*`, `CMake*Compiler*` → cmake compiler-detection.
- `Check*Exists`, `Check*Include`, `Check*SourceCompiles`,
  `Check*SourceRuns`, `Check*SymbolExists`, `Check*TypeSize` → cmake
  Check* macros.
- `*/CMakeFiles/CMakeScratch/*`, `*/CMakeFiles/CMakeTmp/*` → cmake
  TryCompile scratch dirs.

Every pattern here was added because a real project tripped it.

## Output layout (FHS)

Artifacts land where the rest of the Nix ecosystem expects them:

```
bin-mosh-server/bin/mosh-server     link outputs  -> $out/bin/
ar-libiberty.a/lib/libiberty.a      ar outputs    -> $out/lib/
tu-regex.o/regex.o                  compile outputs stay FLAT
```

The first two matter because a store path's internal layout is what
makes it installable. `nix profile install`, `nix run`, `buildEnv` /
`symlinkJoin`, and NixOS's `environment.systemPackages` all locate
artifacts by scanning `bin/`, `lib/`, `share/` and friends. An
executable sitting flat at `$out/<name>` is invisible to every one of
them, so before this a nixgg-built program could not be installed the
way any other Nix package can.

Compile outputs deliberately stay flat. A per-TU output holding one `.o`
is not a package and has no FHS home, and relocating it would rewrite
every sibling reference — churning every drv hash in the project to move
files nothing user-facing ever reads. Concretely: the FHS change moved
link and archive hashes but left all 67 of gcc's `tu-*` drvs
byte-identical, so they stayed cached across it.

Producer and consumer decide placement separately, because each knows
only half of what is needed:

- `Derivation.outSubdir` (producer) keys on `Kind` — it knows it is a
  link or an archive.
- `inputSubdirFor` (consumer) keys on the artifact's **filename** —
  `.a` → `lib`, `.o` → flat, anything else → `bin`.

The consumer side must key on the filename rather than on the producing
derivation's name, and that is load-bearing rather than incidental: in
sandbox mode a sibling reference is a drv path (`…-ar-libfoo.a.drv`,
kind legible) but in native mode it is a thunk path
(`.nixgg/thunks/<hash>.nix`, kind absent). Inferring from the drv name
resolves to `lib` in one mode and `""` in the other for the same input,
which emits different scripts and different drv hashes — exactly the
invariant break this project cannot tolerate. Two tests pin this:
`TestOutSubdirAgreesWithInputSubdirFor` and
`TestInputSubdirForIsModeIndependent`.

## What every CA hash includes

For a compile derivation, native or sandbox:

- Compiler binary content (via `compilerRoot` → pinned by flake.lock).
- `toolBasename` (cc/gcc/c++/g++).
- `srcTree` NAR-hash — Nix imports the staged directory.
- `source` — relative path inside srcTree.
- `flags` — order-preserved JSON array.
- `wrapperEnv` — sorted JSON object (NIX_CFLAGS_COMPILE et al).
- `storeDeps` — sorted array of `/nix/store/…` roots referenced.
- `bash + coreutils` (baked into helpers at nixgg-nix realise time).

Native and sandbox modes produce **bit-identical** store paths for
identical inputs. The mode only affects *when* realisation happens
and *how* Nix is asked to do it.

## Performance snapshots

### Native mode

Warm rebuild of lua after edit is 1.8s (mostly the shim pass + one
`nix build` on the whole DAG). Zstd cold is 2.8s. Redis cold is
~1m30s (175 TUs across deps + src).

### Sandbox mode (dyn-drv)

- **hello** (1 TU + 1 link): 5.7s cold via `nix build .#hello`.
- **lua** (32 TUs + 1 archive + 1 link): 10.7s cold, 8.5s warm.
- **fmt** (2 TU + 1 archive): cmake configure + build via
  `.#fmt-shell`'s cmake+ninja stdenv env, produces libfmt.a.
- **mosh** (~34 TU + 6 archives + 2 links): autoconf configure +
  recursive make, stdenv-plumbed openssl/ncurses/zlib/protobuf, one
  drv per TU submitted through the daemon.

Warm rebuilds pay the shim pass every time (JSON drv construction is
cheap; Nix's own eval cache short-circuits the actual builds via the
same drv-hash → cache hit). Prior to `internal/rpc`, each shim call
additionally paid `fork+exec nix derivation add`/`nix store add
--scan`/`nix store submit-output` (~20-90ms/call depending on load —
see "What we don't (yet) do", now resolved below).

## Correctness properties

- **Content-addressed identity**: same source + same flags = same
  store path. `git checkout` bumping mtimes doesn't force rebuild
  (the shim's thunk id is unchanged; scan-cache validates via mtime
  but recovers via CA hash).
- **Distributable**: any machine with the same flake pin produces
  the same store paths. Point at a substituter → other developers
  pull instead of compile.
- **Deterministic**: back-to-back builds produce byte-identical
  outputs (barring toolchain-embedded timestamps like redis's
  `mkreleasehdr.sh`).
- **Mode-equivalent**: `tests/drv-equivalence.sh` pins that every
  inner drv matches byte-for-byte between native and sandbox mode
  across all four fixtures — 81 drvs total (hello 3 · lua 37 · fmt 3
  · mosh 38). Both modes share the same `Derivation` struct and
  `preBuild` scrubbing, so drift is caught immediately.

## What we don't (yet) do

- ~~Warm-path drv memoization for sandbox mode~~ — **resolved**.
  `internal/rpc` speaks the Nix worker protocol directly over the
  sandbox's own daemon socket (`NIX_REMOTE`), replacing the fork+exec
  of `nix derivation add`/`nix store add --scan`/`nix store
  submit-output` with one persistent connection per shim invocation.
  `internal/aterm` renders the same ATerm derivation text `nix
  derivation add` computes internally (from the existing
  `expr.JSONDrv`), and `internal/nar` encodes a directory into NAR
  format for the scan-upload op — both read directly out of the
  pinned Nix source and verified byte-exact against real output
  before being wired in. On by default (`NIXGG_RPC=1` in every
  sandbox mechanism's env block); `NIXGG_RPC=0` falls back to the CLI
  path.

  The file-cache idea this section used to propose (`.nixgg/` keyed on
  thunk id, to avoid *repeating* fork+exec calls across rebuilds) is
  moot now — the cost it was trying to amortize (fork+exec itself, not
  just its repetition) is gone. The sandbox-per-build ephemerality
  argument that killed the file-cache idea (no persistent host path,
  `dontUnpack = false`, `.nixgg/` anchored inside the throwaway build
  dir) still holds and would still block a *future* cross-build cache,
  but there's no longer a reason to want one for this specific cost.

- **Link/archive lines beyond ~1700 inputs.** Every input is rendered
  into one space-separated string that becomes the single `bash -c`
  argument, in both wire formats. Linux caps *one* argv string at
  `MAX_ARG_STRLEN` = 131072 bytes, independent of the much larger
  `ARG_MAX`. Past it, execve fails with E2BIG before bash starts.

  Measured, not extrapolated: the ceiling is exactly 131072 (131072
  fails, 131000 succeeds), and the largest script this project has
  actually produced is LLVM's `ar-libLLVMCodeGen.a` at 42 KB — about
  3x headroom. ffmpeg's `libavcodec.a`, at 1003 objects, reached 68 KB
  (2x). So this is latent, but a single monolithic static library
  roughly 3x larger than anything here reaches it.

  **Two fixes that don't work, and why.** An `@file` response file
  written by a heredoc inside the same script saves ~3% (135798 ->
  131827 bytes for 2000 inputs) and still exceeds the limit: the
  constraint is the script text bash receives, and a heredoc still
  names every path inside it. Passing the list in one environment
  variable fails for the same reason — a single env string is capped
  at the same 131072 (verified directly).

  **What does work**, in increasing order of effort:

  1. *Chunked env vars.* The per-string cap is per string; the total
     budget is `ARG_MAX` (2 MB here). Eight 100 KB chunks carry 800 KB
     successfully — a ~6x gain — with the script iterating over the
     chunks. Verified.
  2. *A store-path manifest.* Write the input list to its own store
     path and reference it as `@/nix/store/<hash>-manifest`. The
     script becomes ~60 bytes regardless of input count, so the
     ceiling disappears rather than moving. `ar` and `gcc` both accept
     `@file` (verified). Costs one extra store object per link/archive
     step and makes the manifest a real drv input.

  (2) is the right answer if this ever bites; (1) is the cheap
  stopgap. Both change the script text, so both move drv bytes for
  every affected derivation and need a large-input-count fixture added
  to `tests/drv-equivalence.sh` — no current fixture comes close.
  Don't re-propose the file cache on its own.

  Batching multiple inputs into one raw-protocol call wouldn't help
  here either: `nix derivation add` has no batch mode (two
  concatenated JSON docs fail to parse), and the argv-length ceiling
  is about the *rendered script text* of one link/archive step, not
  the number of daemon round trips — `internal/rpc` (added for the
  unrelated fork+exec-cost problem above) sends one op per
  derivation either way, same as the CLI did.

- **Batching multiple TUs into one derivation** (a different idea
  from the raw-protocol batching dismissed just above) is now real,
  not a prototype: after measuring that `internal/helper`'s
  connection pooling doesn't help (see README's "Optional: a
  persistent helper" section) because Nix's own per-derivation
  overhead — forking a builder, sandboxing, mounting the store — is
  ~10-20x the daemon handshake cost it amortizes, the next lever is
  derivation *count* itself: bundle N TUs into one multi-output
  derivation instead of one drv per TU.

  This only pays off for source that's genuinely stable relative to
  how often the project rebuilds — a content-addressed batch
  derivation's hash covers every member, so touching one file forces
  real recompilation of every unchanged sibling in the same batch.
  Actively-edited directories would trade saved Nix scheduling
  overhead for wasted real compiler time; vendored dependency trees
  (redis's `deps/{hiredis,lua,jemalloc,...}/`) have nothing to lose
  since every file compiles anyway on a cold build.

  `go/internal/batch` classifies a compile's source path against a
  project author's opt-in `{name, patterns}` groups (`mkNixggBuild`'s
  `batchGroups` param), same shape as `configureSrcFilter`'s
  `includePatterns`. `internal/shim.deferCompileToBatch` records a
  pending member instead of submitting a per-TU derivation, and
  `internal/shim.tryBatchArchive` combines every pending member
  belonging to one archive's same group into ONE derivation (N
  compiles + 1 archive) when that archive's own `ar` invocation sees
  them — see `go/internal/expr/batcharchive.go`'s package docstring
  for the derivation shape. Verified at real scale:
  `redis-batch-probe` collapses ~45 vendored-dep TUs + 5 archives into
  5 combined derivations (158 → 113 total drvs); `mosh-batch` collapses
  30 TUs + 6 archives into 8; ffmpeg's own per-library archives
  (`ffmpeg-batch`) confirm the same win at ~1200 TUs where it fits (see
  the two gaps below, both since fixed).

  Two real gaps found at ffmpeg's scale, both fixed:
  - **The `MAX_ARG_STRLEN` ceiling this doc's own "Link/archive lines
    beyond ~1700 inputs" section already names for ordinary
    link/archive scripts applies identically to a batch's combined
    compile+archive script** — confirmed directly: batching ffmpeg's
    largest per-library archives (`libavcodec`/`libavformat`/
    `libavfilter`, 350-550+ TUs each) produces a 400KB-1MB script,
    which failed at build time with "Argument list too long." Fixed
    the same way `assemble.Build` already fixed the identical problem
    for openssl's own tree-restore script: the combined script goes
    through `Env["batchScript"]` + `passAsFile` instead of `Args`
    (`go/internal/expr/batcharchive.go`'s `BatchArchiveJSON`,
    `nix/batchArchiver.nix`), so `Args` stays a short, fixed string
    regardless of member count.
  - **Object-basename collisions across a batch's members are now
    handled** (`go/internal/shim/batcharchive.go`'s
    `disambiguateOutNames`, added after this bug was found): batching
    writes every member's compiled object into one shared scratch
    directory before `ar` packages them, keyed by the compile's own
    output basename — a project with two source files sharing a
    basename in different subdirectories (ffmpeg's
    `libavutil/cpu.c` + `libavutil/x86/cpu.c`, or
    `libswscale/swscale.c` + `libswscale/x86/swscale.c`) used to have
    the second compile silently overwrite the first's object before
    packaging, producing either a downstream link failure (missing
    symbols) or a "successful" archive quietly missing an
    implementation. Fixed by giving each colliding member a
    deterministic `-2`/`-3`/... suffix before its extension.

  A third gap, found and fixed after the two above: **folding N TUs
  into one batch derivation traded away Nix's own per-derivation
  build parallelism** — every member of a batch used to compile
  strictly one at a time in the combined derivation's single builder
  process, confirmed directly via `ps aux` while building ffmpeg's
  libavcodec batch (exactly one gcc/cc1 process running at any moment,
  for the whole ~350-TU batch's duration, regardless of available
  cores). Fixed by backgrounding each member's compile and bounding
  concurrency at `$NIX_BUILD_CORES` via a FIFO explicit-pid `wait`
  job runner (`batchArchiveScript`'s own docstring explains why FIFO
  wait, not `wait -n` — the latter has a real job-reaping race that
  silently loses a compile failure). Confirmed directly against a
  real `mosh-batch` build: up to 18 concurrent compiler processes
  observed, versus exactly 1 before the fix.

  One real gotcha found wiring the classification piece up: a TU's
  path relative to "the project root" has no single stable value
  across a build — `internal/scan` computes `ProjectRoot` per compile
  call (the common ancestor of that call's own cwd + `-I` dirs), so
  the same logical file resolves to a *different* relative path
  depending on which directory `make` happened to be in when it
  invoked the shim for that particular TU. Confirmed directly against
  a real redis build: compiling from inside `deps/hiredis/` with no
  outside `-I` references collapsed `ProjectRoot` down to
  `deps/hiredis` itself, so matching against the "relative path"
  caught only 1 of ~40 files under `deps/`. Fixed by classifying
  against the TU's absolute path with an unanchored search instead of
  assuming any relative path's position 0 is the project root — see
  `internal/batch.Classify`'s own docstring.

- ~~Multi-target dyn-drv builds~~ — **resolved**. `mkNixggBuild`
  used to submit exactly one final drv; a project with multiple
  binaries from one build (lua's lua + luac, mosh's client + server)
  needed one `mkNixggBuild` call per target, discarding every other
  link. `targets` is now a list of `{ name; path; }` (order load-
  bearing — the first entry is "the" back-compat `result`/`package`);
  the outer derivation gets one output per target, named
  `"<name>.drv"`, and each target's own link/archive drv is renamed
  `"<outerBuildName>-<targetName>"` (no `bin-`/`ar-` prefix) so its
  real name matches Nix's own `outputPathName($name, outputKey)`
  check, which `submit-output` enforces server-side — confirmed
  directly against a real builder-rpc-v0 sandbox before wiring it in
  (see `go/internal/shim/storeinput.go`'s `multiTargetName`
  docstring for the exact formula). Shared intermediate archives
  underneath (e.g. mosh's `libmoshcrypto.a`) keep their original
  naming and stay fully shared across every target that links
  against them — only each target's own final link/archive drv
  renames. `mosh` is the real multi-target example: `.#mosh` builds
  `mosh-server` (the first `targets` entry), `.#mosh-client` builds
  the second — both confirmed as genuine, working ELF binaries.
- **Activity log emission** (`NIXGG_LOG=/path.ndjson`). Old bash
  version had per-shim JSON events; Go rewrite dropped it. Easy to
  add if needed.
- **Cmake/ninja target introspection** wrappers. nix-ninja
  (github.com/pdtpartners/nix-ninja) is a full implementation of
  this pattern for ninja graphs; we may end up interop-ing with it
  rather than reinventing.

## Building the binary

```
cd go && CGO_ENABLED=0 go build -ldflags='-s -w' -tags 'osusergo netgo' -o ../bin/nixgg .
```

Or via the flake:

```
nix build .#nixgg-bin
```

Produces a static 2.9 MB ELF plus `shims/` symlinks. Add `shims/`
(and `bin/`) to PATH — `eval "$(nixgg env)"` does that.
