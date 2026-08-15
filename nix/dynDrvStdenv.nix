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
      in
      argsOrFn:
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

        # Honor whatever the wrapped package asked for rather than
        # forcing it off. Packages that set `__structuredAttrs = true`
        # write their phases against it — the kernel's own
        # configurePhase/postInstall use bash ARRAY syntax
        # (`make "''${makeFlags[@]}"`, `installFlags+=(…)`), which
        # simply does not exist when structuredAttrs is off, so forcing
        # it false turned those into silent garbage.
        #
        # Default matches make-derivation.nix's own: a package that
        # never mentions __structuredAttrs still inherits the
        # nixpkgs-wide setting.
        structuredAttrs = probeArgs.__structuredAttrs or (config.structuredAttrsByDefault or false);

        # builder-rpc-v0 wants $out unset; /nonexistent keeps stdenv's
        # _assignFirst happy while making any real write fail loudly.
        # Where that value has to go depends on the mode: with
        # structuredAttrs, make-derivation.nix routes bare top-level
        # attrs into .attrs.json and only `env` reaches the derivation
        # as environment variables (it merges `env` into the derivation
        # args wholesale — see its `derivation (derivationArg //
        # checkedEnv)`).
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

        withPhase1Attrs =
          finalAttrs:
          let
            base =
              (lib.toFunction argsOrFn finalAttrs)
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
                __structuredAttrs = structuredAttrs;
                requiredSystemFeatures = (probeArgs.requiredSystemFeatures or [ ]) ++ [ "builder-rpc-v0" ];
                __contentAddressed = true;
                outputHashMode = "text";
                outputHashAlgo = "sha256";
                nativeBuildInputs = (probeArgs.nativeBuildInputs or [ ]) ++ [ patchedNix ];

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
                postPatch = (probeArgs.postPatch or "") + ''
                  export NIXGG_BYPASS=1
                  ${ggShimsOnPath knownStorePathsJSON}
                '';
                preBuild = ''
                  unset NIXGG_BYPASS
                '' + (probeArgs.preBuild or "");

                # The submit is its own phase appended via postPhases,
                # NOT a postBuild hook — for the same reason phase 2's
                # tree-restore is ggRestorePhase rather than a preCheck
                # hook, and the failure mode is just as quiet.
                #
                # postBuild only runs if the package's buildPhase calls
                # `runHook postBuild`. Plenty of hand-written
                # buildPhases don't; nixpkgs' own linux-config is one.
                # Those packages built fine, submitted nothing, and
                # failed with Nix's opaque "failed to submit output
                # path for 'out'" — with no hint that a hook had been
                # skipped.
                #
                # setup.sh splices ${postPhases[*]} onto the end of the
                # phase list unconditionally, gated by no dont*/do*
                # toggle. Since phase 1 disables everything after
                # buildPhase, this lands immediately after it.
                postPhases = (lib.toList (probeArgs.postPhases or [ ])) ++ [ "ggSubmitPhase" ];
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
          base =
            (lib.toFunction argsOrFn finalAttrs)
            // {
              phases = "ggRestorePhase checkPhase installPhase fixupPhase installCheckPhase distPhase";
              dontUnpack = true;
              # Same reasoning as phase 1: the package's own
              # installPhase/fixupPhase run verbatim here, so they must
              # get the attribute shape they were written against.
              __structuredAttrs = structuredAttrs;
              ggRestorePhase = ''
                runHook preGgRestore
                cp -a ${builtTree}/. "$NIX_BUILD_TOP/"
                chmod -R u+w "$NIX_BUILD_TOP"
                cd "$NIX_BUILD_TOP/$(cat "$NIX_BUILD_TOP/.gg-cwd")"
                export DESTDIR="$NIX_BUILD_TOP/.gg-destdir"
                runHook postGgRestore
              '';
              # Collect what the install landed under DESTDIR, IF it went
              # there at all.
              #
              # The guard is load-bearing, not defensive padding.
              # DESTDIR only catches packages whose install is a
              # `make install`/`cmake --install` honouring it — phase 1
              # baked prefix=/nonexistent into the generated Makefile,
              # so those land in $DESTDIR/nonexistent. Plenty of
              # packages do something else entirely: a hand-written
              # installPhase that writes straight to $out (very common
              # in nixpkgs), or explicit destination flags — the kernel
              # passes INSTALL_PATH/INSTALL_MOD_PATH and never consults
              # DESTDIR. For those, $DESTDIR/nonexistent is never
              # created and an unconditional `cp -a` failed the build
              # outright, after a successful install.
              #
              # If $out comes out empty, suspect an install that wrote
              # somewhere neither branch covers.
              # The mkdir lives INSIDE the guard, and that matters as
              # much as the guard itself: `mkdir -p "$out"` assumes $out
              # is a directory, and not every derivation's is. nixpkgs'
              # linux-config installs with `mv $buildRoot/.config $out`
              # — a single FILE. Pre-creating $out as a directory made
              # that mv drop the config *inside* it, and the failure
              # surfaced two derivations later as the kernel's
              # `ln -sv ${configfile} $buildRoot/.config` producing
              # ".config: Is a directory".
              #
              # Packages that install themselves need nothing from us
              # here; only the DESTDIR path needs a destination created.
              postInstall = ''
                if [ -d "$DESTDIR/nonexistent" ]; then
                  mkdir -p "$out"
                  cp -a "$DESTDIR/nonexistent/." "$out/"
                fi
              '' + (probeArgs.postInstall or "");
            };
        in
        extraPhase2Attrs finalAttrs base
      );
  }
)
