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
  # Stage each TU as a farm of symlinks into per-file store objects
  # instead of copying its whole header closure. Sandbox mode only:
  # it relies on `nix store add --scan` recording symlink targets as
  # references, and native mode imports its staging dir as a plain path
  # literal, which does no scanning — the targets would dangle.
  #
  # Off by default because that asymmetry suspends the native/sandbox
  # drv-equivalence guarantee, which is the project's core invariant.
  sharedStaging ? false,

  # Subtrees whose build reads object BYTES inline, or expects a compile
  # to FAIL and reads the diagnostic. A derivation cannot stand in for
  # either, so the shims pass them through and let the build do the work.
  #
  # Project-specific by nature, which is why it is a parameter rather
  # than a list compiled into internal/mode.
  passthroughPaths ? [ ],
}:

let
  stdenv0 = stdenv;

  # This pinned nixpkgs' make-derivation.nix curries `lib: config:
  # stdenv: {...}` (three positional args), not the older `{lib,
  # config}: stdenv: {...}` shape — call it the way this pin wants.
  defaultMkDerivationFromStdenv =
    stdenv:
    (import "${nixpkgsPath}/pkgs/stdenv/generic/make-derivation.nix" lib config stdenv).mkDerivation;

  # Bindings that are byte-for-byte identical between this file and
  # dynDrvConfigureCacheStdenv.nix (confirmed via diff; see git
  # history — 1d605d5, 9bec045 — for two real bugs that had to be
  # fixed by hand in both copies) — see nix/dynDrvShared.nix's own
  # docstring for what's in it and why.
  shared = import ./dynDrvShared.nix {
    inherit
      lib
      patchedNix
      nixgg
      bash
      coreutils
      gcc
      gnumake
      passthroughPaths
      sharedStaging
      system
      ;
  };
  inherit (shared) ggShimsOnPath submitBuildTreeScript outputPlaceholder;

  # Replay phase 1's exports, gap-filling only: a variable
  # phase 2 already set always wins, so its own outputs and
  # stdenv-managed state cannot be clobbered. Splitting one
  # mkDerivation across two derivations splits the shell too, and
  # packages routinely export in one phase and read in a later one.
  ggRestoreEnv = ''
    if [ -f "$NIX_BUILD_TOP/.gg-env" ]; then
      while IFS= read -r ggLine; do
        case "$ggLine" in
          "declare -x "*) ;;
          *) continue ;;
        esac
        ggKV=''${ggLine#declare -x }
        ggName=''${ggKV%%=*}
        case "$ggName" in
          PATH|PWD|OLDPWD|HOME|SHLVL|_) continue ;;
          TMP|TMPDIR|TEMP|TEMPDIR) continue ;;
          NIX_BUILD_TOP|NIX_STORE|NIX_BUILD_CORES|NIX_LOG_FD) continue ;;
          out|outputs) continue ;;
          NIXGG_*) continue ;;
          # Phase control. Derivation attributes are exported like any
          # other variable, so phase 1's own `dontInstall = true`
          # arrives here as dontInstall=1 — and phase 2, which never
          # sets it, would gap-fill it and then SKIP ITS OWN
          # installPhase. That failure is silent: the builder exits 0
          # having run nothing, and Nix reports only "failed to produce
          # output path". Same hazard for dontFixup/doCheck/doDist and
          # for the phase list itself.
          dont*|do[A-Z]*|phases|*Phase|*Phases) continue ;;
        esac
        # Gap-fill only: never override what phase 2 already decided.
        if [ -z "''${!ggName+x}" ]; then
          eval "export $ggKV"
        fi
      done < "$NIX_BUILD_TOP/.gg-env"
    fi
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

          # Honour what the package asked for: phases written against
          # structuredAttrs use bash array syntax, which does not exist
          # when it is off. Default matches make-derivation.nix.
          structuredAttrs = probeArgs.__structuredAttrs or (config.structuredAttrsByDefault or false);

          # builder-rpc-v0 wants $out unset; /nonexistent keeps stdenv's
          # _assignFirst happy while making a real write fail loudly.
          # Under structuredAttrs only `env` reaches the derivation as
          # environment variables, so the placeholder goes there.
          nonexistentOut =
            if structuredAttrs then
              { env = (probeArgs.env or { }) // { out = "/nonexistent"; }; }
            else
              { out = "/nonexistent"; };

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

          # restoreOutputsScript/elfRpathFixupScript: see
          # nix/dynDrvShared.nix's own docstring on both for the
          # openssl-derived rationale (DESTDIR guard ordering, two
          # possible source locations per output, rpath rewrite
          # ordering).
          restoreOutputsScript = shared.restoreOutputsScript outputPlaceholder realOutputs;
          elfRpathFixupScript = shared.elfRpathFixupScript outputPlaceholder realOutputs extraOutputs;

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
                  # Honoured, not forced: the package's phases may be
                  # written against structuredAttrs. The `out`
                  # placeholder moves into `env` to match — see
                  # nonexistentOut.
                  __structuredAttrs = structuredAttrs;
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

                  # A real phase, not a postBuild hook: postBuild only
                  # runs if the package's buildPhase calls runHook, and
                  # many hand-written ones do not (nixpkgs' own
                  # linux-config among them) — those built fine,
                  # submitted nothing, and failed with Nix's opaque
                  # "failed to submit output path for 'out'".
                  # postPhases is spliced on unconditionally by setup.sh.
                  postPhases = (lib.toList (orig.postPhases or [ ])) ++ [ "ggSubmitPhase" ];
                  ggSubmitPhase = submitBuildTreeScript outerName;
                }
                // nonexistentOut;
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
                __structuredAttrs = structuredAttrs;
                ggRestorePhase = ''
                  runHook preGgRestore
                  cp -a ${builtTree}/. "$NIX_BUILD_TOP/"
                  chmod -R u+w "$NIX_BUILD_TOP"
                  cd "$NIX_BUILD_TOP/$(cat "$NIX_BUILD_TOP/.gg-cwd")"
                  ${ggRestoreEnv}

                  # Phase 2 needs the shims reachable but inert: build
                  # systems bake absolute tool paths at configure time
                  # (cmake does; a caller can via makeFlags), so those
                  # paths get invoked here whether or not we planned for
                  # it. Without the env the shim cannot build its config
                  # and exits non-zero. Nothing is left to accelerate, so
                  # BYPASS makes each one exec the real tool.
                  export NIXGG_BYPASS=1
                  ${ggShimsOnPath knownStorePathsJSON}
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
                  # installFlagsArray, not installFlags: under
                  # __structuredAttrs `installFlags` is a bash ARRAY, and
                  # assigning a scalar to an array name writes element 0
                  # — which silently welded this onto the package's first
                  # real flag. nixpkgs' kernel sets
                  # INSTALL_PATH=$out there, so `make install` received
                  # one token "INSTALL_PATH=… DESTDIR=…" and died on
                  # `cp: target 'DESTDIR=…': No such file or directory`.
                  # setup.sh concatenates installFlagsArray in both
                  # modes (concatTo, installPhase), and it is always a
                  # plain array, so appending there is mode-independent.
                  installFlagsArray+=( "DESTDIR=$DESTDIR" )
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
