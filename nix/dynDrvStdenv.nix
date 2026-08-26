# dynDrvStdenv — a stdenv that turns configurePhase+buildPhase into a
# builder-rpc-v0 derivation, with nixgg's shims on PATH so every
# cc/c++/ar call becomes its own dynamic derivation (same mechanism as
# mkNixggBuild, but discovered by walking the resulting tree instead of
# by a single named `target`). installPhase/fixupPhase/checkPhase/meta
# of the original package are untouched, in a second ordinary
# derivation.
#
# Usage, scoped to one package:
#
#   hello = pkgs.hello.override {
#     stdenv = dynDrvStdenv { stdenv = pkgs.stdenv; };
#   };
#
# Mechanism: override `mkDerivationFromStdenv` (the hook
# pkgs/stdenv/generic/default.nix documents for exactly this — adapters
# like ccache/pkgsMusl use the same seam via adapters.nix's private
# withOldMkDerivation, not exported here).
#
# Not overrideCC (ccache's approach): ccache only swaps the compiler
# binary. nixgg needs to swap what BUILDS the derivation itself
# (requiredSystemFeatures, __contentAddressed, phase boundaries).
#
# Phase 1 is builder-rpc-v0, not native "nix build at the end": a
# sandboxed stdenv build can't invoke nix build/nix-instantiate at all
# — verified directly against both an alt-store (nested sandbox doesn't
# inherit outer bind-mounts) and the ambient store (read-only). Only
# the narrow builder-rpc-v0 RPC allowlist works from inside a sandbox;
# mkNixggBuild.nix already commits to it for the same reason.
#
# checkPhase moved to phase 2: with shims live, every artifact phase 1
# produces is a drvref stub (see internal/drvref) until `nixgg
# assemble` resolves it. A checkPhase running in phase 1 would exec an
# unresolved stub, not a real binary.
{
  lib,
  patchedNix,
  nixgg, # nixggBin: $out/bin/nixgg + $out/shims/{cc,c++,ar,...}
  bash,
  coreutils,
  gcc,
  gnumake,
  system, # target platform for the JSON drv `nixgg assemble` builds.
          # Not builtins.currentSystem — that's impure and breaks a
          # plain `nix build` (no --impure).
  nixpkgsPath, # nixpkgs tree stdenv0 came from, for the
               # mkDerivationFromStdenv fallback when the caller's
               # stdenv has no override set yet.
  config, # real nixpkgs config (pkgs.config) — make-derivation.nix
          # reads several config.* options; {} silently gives wrong
          # defaults.
}:

{
  stdenv,
  # Escape hatch: a caller cannot patch phase 1 via .overrideAttrs —
  # nixpkgs' own .override/.overrideAttrs reapplication always
  # re-invokes the package function with its ORIGINAL attrs first, and
  # phase 1 is already closed over by the time any .overrideAttrs the
  # caller wrote runs (verified directly: both orderings produced a
  # byte-identical phase-1 derivation to the unpatched build). Pass a
  # fix here instead, at the dynDrvStdenv call site:
  #
  #   dynDrvStdenv {
  #     stdenv = pkgs.stdenv;
  #     extraPhase1Attrs = finalAttrs: old: old // {
  #       postPatch = old.postPatch + "sed -i ... Makefile\n";
  #     };
  #   }
  #
  # `old` is phase 1's own attrset as dynDrvStdenv built it (already
  # carrying the real package's postPatch plus dynDrvStdenv's own
  # shim-activation postPatch/preBuild).
  extraPhase1Attrs ? (finalAttrs: old: old),
  # Same shape, for phase 2 (install/fixup/installCheck/dist). Phase 2
  # IS reachable via a plain .overrideAttrs on the returned package, so
  # this exists mostly for symmetry.
  extraPhase2Attrs ? (finalAttrs: old: old),
}:

let
  stdenv0 = stdenv;

  # This pinned nixpkgs' make-derivation.nix curries `lib: config:
  # stdenv: {...}` (three positional args), not the older `{lib,
  # config}: stdenv: {...}` shape — call it the way this pin wants.
  defaultMkDerivationFromStdenv =
    stdenv:
    (import "${nixpkgsPath}/pkgs/stdenv/generic/make-derivation.nix" lib config stdenv).mkDerivation;

  # NIXGG_* vars match mkNixggBuild.nix's toolchainEnv.
  #
  # NIXGG_SANDBOX_TARGET is set to an unmatchable path rather than left
  # unset: link.go's maybeSubmit defaults to submitting whatever it
  # links as the outer drv's "out" when TARGET is empty — correct for
  # mkNixggBuild's single-target builds, wrong here, where `nixgg
  # assemble` (postBuild) owns the one "out" submission for the whole
  # tree.
  #
  # knownStorePathsJSON is a parameter (not computed once here) because
  # dynDrvStdenv wraps an arbitrary existing package — the real store
  # paths the shim needs to recognize vary per package and are only
  # known from probeArgs.buildInputs, read per-call in withPhase1Attrs.
  ggShimsOnPath = knownStorePathsJSON: ''
    export PATH="${nixgg}/shims:${patchedNix}/bin:$PATH"
    export NIXGG_ROOT="${nixgg}"
    export NIXGG_COMPILER_ROOT="${gcc}"
    export NIXGG_BASH_ROOT="${bash}"
    export NIXGG_COREUTILS_ROOT="${coreutils}"
    export NIXGG_GNUMAKE_ROOT="${gnumake}"
    export NIXGG_REAL_CC="${gcc}/bin/g++"
    export NIXGG_NIX="${patchedNix}/bin/nix"
    export NIXGG_NIX_HELPERS="${nixgg}"
    export NIXGG_SANDBOX=1
    export NIXGG_STORE="auto"
    export NIXGG_SYSTEM="${system}"
    export NIXGG_SANDBOX_TARGET="/nonexistent/nixgg-phase1-no-per-artifact-submit"
    export NIXGG_KNOWN_STORE_PATHS=${lib.escapeShellArg knownStorePathsJSON}
    # Raw worker-protocol client for the sandbox's own daemon socket
    # (internal/rpc) instead of per-call fork+exec — see
    # mkNixggBuild.nix's own comment on this same var for the
    # verification this default rests on. NIXGG_RPC=0 is the escape
    # hatch back to the CLI fallback.
    export NIXGG_RPC=1
    export NIX_CONFIG="extra-experimental-features = nix-command ca-derivations dynamic-derivations"
  '';

  # Records $PWD's offset relative to $NIX_BUILD_TOP (phase 2 needs to
  # cd back there — cmake's `mkdir build && cd build` means $PWD here
  # isn't $NIX_BUILD_TOP), then walks $NIX_BUILD_TOP for every drvref
  # stub the shims left, builds one assembly drv that restores the tree
  # and resolves each stub, and submits it as this derivation's "out".
  # See go/internal/cli/assemble.go / go/internal/assemble/.
  submitBuildTreeScript = drvName: ''
    realpath --relative-to="$NIX_BUILD_TOP" "$PWD" > "$NIX_BUILD_TOP/.gg-cwd"
    ${nixgg}/bin/nixgg assemble "$NIX_BUILD_TOP" "${drvName}"
  '';
in

stdenv0.override (
  old:
  {
    mkDerivationFromStdenv =
      stdenvSelf:
      let
        mkDerivationSuper = (old.mkDerivationFromStdenv or defaultMkDerivationFromStdenv) stdenvSelf;

        # `build` takes its own extraPhase1Attrs/extraPhase2Attrs,
        # shadowing dynDrvStdenv's own same-named params — the body
        # below is otherwise UNCHANGED from before this passthru
        # existed, it just runs once per `build` call instead of
        # exactly once. passthru.overridePhase1Attrs (at the bottom)
        # re-invokes `build` with a composed extraPhase1Attrs: the
        # supported way to patch phase 1 AFTER `.override { stdenv =
        # ...; }`, since — per extraPhase1Attrs's own docstring above
        # — phase 1 is baked into phase 2's closure by VALUE
        # (builtTree) and can never be reached through nixpkgs' own
        # .overrideAttrs chain, which only ever touches phase 2's
        # build.
        build =
          argsOrFn: extraPhase1Attrs: extraPhase2Attrs:
          let
          # argsOrFn may be a plain attrset or a `finalAttrs: {...}`
          # function. Never collapse it to a plain set — that destroys
          # makeDerivationExtensible's real fixed point (finalPackage,
          # overrideAttrs, meta), breaking anything that reads
          # finalAttrs.* (hello's postInstallCheck does). probeArgs below
          # is a throwaway {} application used ONLY to read static
          # pname/version for naming; phase 1/2 attrs are built by
          # wrapping argsOrFn in another finalAttrs-function layer,
          # mirroring how overrideAttrs itself composes.
          probeArgs = lib.toFunction argsOrFn { };
          drvName = if probeArgs ? name then probeArgs.name else "${probeArgs.pname}-${probeArgs.version}";
          outerName = "gg-build-${drvName}";

          # Store paths the shim's storedeps matcher needs to recognize
          # in -I/-L flags — same computation as mkNixggBuild.nix's
          # knownStorePathInputs, sourced from the wrapped package's own
          # buildInputs instead of an explicit param.
          knownStorePathsJSON = builtins.toJSON (
            map toString (
              builtins.concatMap (p: p.all or [ p ]) (
                (probeArgs.buildInputs or [ ]) ++ (probeArgs.propagatedBuildInputs or [ ])
                ++ [ bash coreutils gcc nixgg patchedNix ]
              )
            )
          );

          # Phase 1's own derivation must declare exactly one output
          # ("out") — submit-output's ".drv" naming convention requires
          # it (see `name` below) — but build systems bake bin/dev/man/
          # etc. install paths into the Makefile/CMakeCache at configure
          # time via multiple-outputs.sh's `_overrideFirst` chain, which
          # silently collapses every output name to "$out" unless a
          # same-named bash var already exists before configurePhase
          # runs. Give each of the package's REAL non-"out" outputs its
          # own subdir of phase 1's one tree, so phase 2 can split the
          # restored tree back apart into its real outputs (see its
          # postInstall below). Confirmed necessary directly: without
          # this, zstd-dyndrv's cmake install baked every output —
          # including bin/zstd — under /nonexistent itself, and phase
          # 2's blind `cp ... $out/` only ever populated "out", leaving
          # "bin"/"dev"/"man" empty.
          realOutputs = probeArgs.outputs or [ "out" ];
          extraOutputs = builtins.filter (o: o != "out") realOutputs;
          outputPlaceholder = o: if o == "out" then "/nonexistent" else "/nonexistent-${o}";

          # Phase 2's restore/split step, generated once so it stays in
          # sync with outputPlaceholder above: for every real output,
          # make the real output dir and copy its placeholder subtree
          # (installed by phase 2's own installPhase, DESTDIR-relative)
          # into it. `v`/`ph` are built via string concatenation, not
          # Nix's `${}` antiquotation, so the emitted script contains a
          # literal shell variable reference (e.g. "$bin"), not a
          # Nix-side interpolation of it.
          #
          # `[ -d ... ] &&` guards each cp: multiple-outputs.sh's
          # automatic per-output DESTDIR routing (_overrideFirst
          # outputBin "bin" "out", etc.) only takes effect when the
          # package's OWN build system actually threads $bin/$dev/...
          # through to its install step (e.g. via --bindir=$bin/bin on
          # an autoconf-style configure). Some packages don't — openssl
          # installs everything flat under out's placeholder and does
          # its own real `mv $out/bin $bin/bin` in postInstall instead
          # — so `$DESTDIR<placeholder>` for that output is never
          # created at all. Failing on a missing directory there would
          # break every such package outright; skipping it just leaves
          # that real output empty for THIS script and lets the
          # package's own postInstall (which runs right after, per
          # nixpkgs hook order) populate it from the real $out however
          # it normally does. Confirmed necessary directly: openssl's
          # own bin split hit exactly this.
          #
          # `mkdir -p "${v}"` lives INSIDE that same guard, not before
          # it: a package whose own postInstall creates that output dir
          # itself may do so with a bare `mkdir` (no `-p`), expecting a
          # not-yet-existing directory — openssl's own `mkdir $dev`
          # (right before `mv $out/include $dev/`) is exactly this.
          # Pre-creating "$dev" unconditionally here made that `mkdir`
          # fail with "File exists". Confirmed necessary directly.
          #
          # Two possible source locations, not one: a package's
          # makeFlags/installFlags can point an output at its own real,
          # absolute final path instead of the placeholder scheme here
          # (openssl's `MANDIR=$(man)/share/man`, where $man is already
          # the real "*-openssl-3.6.3-man" store path — set that way
          # because phase 2, unlike phase 1, has real per-output paths
          # available). Once DESTDIR is threaded onto make's own
          # command line (see ggRestorePhase), THAT absolute path also
          # ends up DESTDIR-prefixed — "$DESTDIR/nix/store/...-man",
          # never under the placeholder tree at all. Confirmed directly
          # against openssl's man output.
          restoreOutputsScript = lib.concatMapStrings (
            o:
            let
              v = "$" + o;
              ph = outputPlaceholder o;
            in
            ''
              if [ -d "$DESTDIR${ph}" ]; then mkdir -p "${v}"; cp -a "$DESTDIR${ph}/." "${v}/"; fi
              if [ -d "$DESTDIR${v}" ]; then mkdir -p "${v}"; cp -a "$DESTDIR${v}/." "${v}/"; fi
            ''
          ) realOutputs;

          # cc-wrapper's ld-wrapper bakes a self-rpath into every linked
          # binary/shared-lib at LINK time (phase 1's buildPhase), using
          # whatever output path was live then — one of the placeholders
          # above, e.g. "/nonexistent/lib" for zstd's libzstd.so (its
          # outputLib defaults to "out", same placeholder as $out
          # itself). Confirmed directly: without this rewrite, the
          # dangling placeholder rpath survives into phase 2, and
          # fixupPhase's own patchelf-based shrinkRPath step (correctly)
          # drops it since nothing exists at that literal path — leaving
          # zstd's `bin/zstd` unable to find libzstd.so.1 at runtime,
          # `nix run` failing with "cannot open shared object file".
          # Longest-placeholder-first order (extraOutputs before "out")
          # matters: "/nonexistent" is a literal prefix of
          # "/nonexistent-bin", so substituting it first would also
          # mangle the longer placeholders.
          elfRpathFixupScript =
            let
              sedOrder = extraOutputs ++ [ "out" ];
              sedExprs = lib.concatMapStrings (
                o: " -e \"s|" + outputPlaceholder o + "|$" + o + "|g\""
              ) sedOrder;
              outputDirsList = lib.concatMapStrings (o: "\"$" + o + "\" ") realOutputs;
            in
            ''
              while IFS= read -r -d "" gg_f; do
                gg_rp="$(patchelf --print-rpath "$gg_f" 2>/dev/null)" || continue
                [ -z "$gg_rp" ] && continue
                gg_newrp="$(printf '%s' "$gg_rp" | sed${sedExprs})"
                if [ "$gg_newrp" != "$gg_rp" ]; then
                  chmod u+w "$gg_f"
                  patchelf --set-rpath "$gg_newrp" "$gg_f"
                fi
              done < <(find ${outputDirsList} -type f -print0 2>/dev/null)
            '';

          withPhase1Attrs =
            finalAttrs:
            let
              # Real finalAttrs fixed point, not probeArgs — some
              # packages read finalAttrs inside a hook (e.g. openssl's
              # own postPatch checks finalAttrs.finalPackage.doCheck),
              # which throws "attribute 'finalPackage' missing" against
              # probeArgs's throwaway {} application. probeArgs stays
              # for the STATIC pname/version/outputs reads above, which
              # only need a stable throwaway value, not the real fixed
              # point.
              orig = lib.toFunction argsOrFn finalAttrs;
              base =
                orig
                // {
                  name = "${outerName}.drv"; # submit-output requires this
                                              # to match outputPathName(outerName, "out")
                  # `phases` is deliberately not set here — setup.sh's
                  # own default construction splices in whatever
                  # pre*Phases setup hooks append at runtime
                  # (autoreconfHook, cmake, ...). A hardcoded phases
                  # string would silently drop those.
                  #
                  # Instead, stop phase 1 after buildPhase via the same
                  # dont*/do* toggles runPhase already checks. doCheck is
                  # forced off here too — checkPhase moves to phase 2.
                  doCheck = false;
                  dontInstall = true;
                  dontFixup = true;
                  doInstallCheck = false;
                  doDist = false;
                  # Phase 1 only ever produces one tree — force
                  # single-output regardless of what the package
                  # declares. A multi-output phase 1 (e.g. fmt's out+dev)
                  # can't be named "*.drv" (Nix requires single-output
                  # for that suffix). Phase 2 keeps the package's real
                  # `outputs`.
                  outputs = [ "out" ];
                  out = "/nonexistent";
                  # Same reason as `outputs` above: make-derivation.nix
                  # computes `outputs' = outputs ++ optional
                  # separateDebugInfo' "debug"` at its OWN layer,
                  # downstream of this override, so setting `outputs =
                  # ["out"]` alone doesn't stop a package's own
                  # `separateDebugInfo = true` (e.g. openssl) from
                  # silently reintroducing a second output and tripping
                  # the same "*.drv" single-output requirement.
                  separateDebugInfo = false;
                }
                # Extra outputs (bin/dev/man/...) are plain env vars
                # here, NOT declared Nix derivation outputs — phase 1
                # stays single-output. Setting them as ordinary attrs
                # (same mechanism as `out` above) makes them part of the
                # build's initial environment, so multiple-outputs.sh's
                # `_overrideFirst outputBin "bin" "out"` (sourced before
                # any postPatch/preConfigure runs) sees a non-empty $bin
                # and points outputBin at it instead of silently
                # collapsing to "out". Nothing is ever written to these
                # paths during phase 1 itself — dontInstall=true skips
                # installPhase, the only phase that would — they only
                # get baked as literal absolute paths into generated
                # build files (Makefile/cmake_install.cmake) inside
                # $NIX_BUILD_TOP, for phase 2's DESTDIR-relative install
                # to act on later.
                // builtins.listToAttrs (
                  map (o: {
                    name = o;
                    value = outputPlaceholder o;
                  }) extraOutputs
                )
                // {
                  # Forced off regardless of what the package requests:
                  # under __structuredAttrs, make-derivation.nix only
                  # honors env-nested attrs, not bare top-level ones like
                  # `out` above.
                  __structuredAttrs = false;
                  requiredSystemFeatures = (orig.requiredSystemFeatures or [ ]) ++ [ "builder-rpc-v0" ];
                  __contentAddressed = true;
                  outputHashMode = "text";
                  outputHashAlgo = "sha256";
                  nativeBuildInputs = (orig.nativeBuildInputs or [ ]) ++ [ patchedNix ];

                  # NIXGG_BYPASS gates acceleration per shim invocation
                  # (bypassed() short-circuits to a real passthrough
                  # exec), not whether shims are on PATH. Shims go on
                  # PATH from postPatch, not preBuild: cmake's own
                  # configurePhase bakes an ABSOLUTE compiler path into
                  # its generated Makefile, so the shim must already be
                  # on PATH at configure time or cmake bakes the real
                  # gcc-wrapper path and nothing ever routes through the
                  # shim (confirmed directly — build succeeds, zero
                  # stubs, no error at all). BYPASS itself must stay set
                  # through configure (autoreconfHook, cmake probes) —
                  # only buildPhase needs real acceleration.
                  postPatch = (orig.postPatch or "") + ''
                    export NIXGG_BYPASS=1
                    ${ggShimsOnPath knownStorePathsJSON}
                  '';
                  preBuild = ''
                    unset NIXGG_BYPASS
                  '' + (orig.preBuild or "");

                  postBuild = (orig.postBuild or "") + submitBuildTreeScript outerName;
                };
            in
            # extraPhase1Attrs runs last, over dynDrvStdenv's own base
            # attrs — see its docstring for why this is the only way to
            # patch phase 1 from outside.
            extraPhase1Attrs finalAttrs base;

          # Phase 1: unpack, patch, configure, build — real nixpkgs phase
          # functions and setup hooks, only the compiler on PATH and the
          # phase list differ.
          phase1 = mkDerivationSuper withPhase1Attrs;

          builtTree = builtins.outputOf phase1.outPath "out";
        in
        # Phase 2: install, fixup, installCheck, dist — the real
        # package's unmodified phases, seeded from phase 1's tree.
        #
        # DESTDIR: phase 1's configurePhase baked its own $out
        # (deliberately "/nonexistent") into the generated Makefile's
        # prefix/bindir. Phase 2 reuses that tree verbatim but has a
        # DIFFERENT real $out, so a plain `make install` would write to
        # /nonexistent again. DESTDIR is exported as a real bash var
        # (not passed via installFlags) — GNU make parses `$N` in an
        # installFlags argument as its OWN variable reference and
        # mangles $NIX_BUILD_TOP (confirmed directly).
        #
        # The tree-restore is its own custom phase (ggRestorePhase, not
        # a preCheck/preInstall hook): those hooks are skipped entirely
        # by runPhase's own guard when doCheck/dontInstall says so, which
        # is the common case, so the restore would never run for most
        # packages. A custom phase name has no such gate. checkPhase
        # comes right after it (with the package's REAL doCheck) so a
        # test suite that execs a freshly linked binary sees a resolved
        # ELF, not a stub.
        mkDerivationSuper (
          finalAttrs:
          let
            # Real finalAttrs fixed point, not probeArgs — see
            # withPhase1Attrs's own orig binding above for why.
            orig = lib.toFunction argsOrFn finalAttrs;
            base =
              orig
              // {
                phases = "ggRestorePhase checkPhase installPhase fixupPhase installCheckPhase distPhase";
                dontUnpack = true;
                __structuredAttrs = false;
                ggRestorePhase = ''
                  runHook preGgRestore
                  cp -a ${builtTree}/. "$NIX_BUILD_TOP/"
                  chmod -R u+w "$NIX_BUILD_TOP"
                  cd "$NIX_BUILD_TOP/$(cat "$NIX_BUILD_TOP/.gg-cwd")"
                  export DESTDIR="$NIX_BUILD_TOP/.gg-destdir"
                  # `export DESTDIR` alone is not enough: some packages'
                  # own Configure/Makefile (openssl's
                  # Configurations/unix-Makefile.tmpl is one) contain a
                  # plain `DESTDIR=` assignment of their own, which GNU
                  # Make's variable-precedence rules let silently
                  # override an inherited environment variable of the
                  # same name — only a value on make's OWN command line
                  # wins over that. Appended here, at ggRestorePhase
                  # RUNTIME (not baked into installFlags as a Nix
                  # string), so bash — not Nix, not make — expands
                  # $NIX_BUILD_TOP into a plain path with no literal `$`
                  # left in it: installFlags is passed straight through
                  # to make's argv, and a literal `$` there gets
                  # reinterpreted as make's OWN `$X`-style variable
                  # reference (confirmed directly, same reason this file
                  # never bakes $NIX_BUILD_TOP into installFlags via a
                  # Nix-side string either). Confirmed necessary
                  # directly: openssl's own `make install_sw` wrote
                  # straight to its literal `/nonexistent` prefix,
                  # never under $DESTDIR, until this was added.
                  installFlags="''${installFlags-} DESTDIR=$DESTDIR"
                  runHook postGgRestore
                '';
                installFlags = (orig.installFlags or "");
                postInstall = restoreOutputsScript + (orig.postInstall or "");
                # Must run before fixupPhase's own patchelf-based
                # rpath-shrinking, which (correctly) drops any rpath
                # entry pointing at a path that doesn't exist — exactly
                # what the placeholder rpaths would still be if this
                # ran any later. postInstall (above) has already split
                # outputs apart by the time preFixup hooks run, so the
                # real output paths this substitutes in are populated.
                preFixup = elfRpathFixupScript + (orig.preFixup or "");

                # Lets a caller reach phase 1 AFTER `.override { stdenv
                # = dynDrvStdenv; }`, not just at the `dynDrvStdenv {
                # ...; }` call site — a plain `.overrideAttrs` on the
                # returned package can't (see extraPhase1Attrs's own
                # docstring above for why). `f` has the same shape as
                # extraPhase1Attrs itself (`finalAttrs: old: old //
                # {...}`) and composes ON TOP of whatever
                # extraPhase1Attrs this build already has — same
                # left-to-right composition order `.overrideAttrs`
                # itself uses. Rebuilds phase 1 (and re-derives phase 2
                # from it) under the hood; there is no cheaper way to
                # patch phase 1, by construction — its own docstring
                # explains why.
                #
                #   (pkgs.openssl.override { stdenv = dynDrvStdenv; }).overridePhase1Attrs (
                #     finalAttrs: old: old // { postPatch = old.postPatch + "..."; }
                #   )
                passthru = (orig.passthru or { }) // {
                  overridePhase1Attrs =
                    f:
                    build argsOrFn (finalAttrs: old: f finalAttrs (extraPhase1Attrs finalAttrs old))
                      extraPhase2Attrs;
                };
              };
          in
          extraPhase2Attrs finalAttrs base
        );
      # First call: dynDrvStdenv's own extraPhase1Attrs/extraPhase2Attrs
      # constructor params (both default to the identity function).
      # passthru.overridePhase1Attrs above re-invokes `build` directly,
      # bypassing this outer function entirely — it never needs to
      # come back through mkDerivationFromStdenv.
      in
      argsOrFn: build argsOrFn extraPhase1Attrs extraPhase2Attrs;
  }
)
