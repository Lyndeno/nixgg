# dynDrvConfigureCacheStdenv — dynDrvStdenv's per-TU acceleration,
# with configureCacheStdenv's early-cutoff applied to the configure
# step it wraps around.
#
# dynDrvStdenv (nix/dynDrvStdenv.nix) runs unpack..build as ONE
# builder-rpc-v0 sandboxed derivation, with shims live but BYPASSED
# (a genuine syscall.Exec passthrough — see go/internal/shim/passthrough.go's
# bypassed()) through configure, and only turned on for buildPhase.
# That means configure under dynDrvStdenv is behaviorally identical
# to configure with no shims at all — it doesn't need the sandbox,
# it's just running inside one that happens to be inert around it.
# So it can be pulled into its own group and given
# configureCacheStdenv's exact early-cutoff treatment
# (configureSrcFilter + CA), same as any other package.
#
# Three groups:
#   group A (configure)      — configureCacheStdenv's group A,
#                               unmodified mechanism, PLUS shims on
#                               PATH (bypassed, never unset — there's
#                               no buildPhase in this group to unset
#                               them before) so the shim's own store
#                               path gets baked into generated build
#                               files instead of the real compiler's.
#   group B (build only)     — dynDrvStdenv's phase 1, minus
#                               configurePhase. Real per-TU
#                               acceleration, unchanged shim
#                               activation/assemble/submit machinery.
#   group C (install onward) — dynDrvStdenv's phase 2, copied
#                               verbatim. Doesn't care whether the
#                               tree it restores came from a 1-group
#                               or 2-group build side.
#
# Usage:
#   hello = pkgs.hello.override {
#     stdenv = dynDrvConfigureCacheStdenv { stdenv = pkgs.stdenv; };
#   };
{
  lib,
  patchedNix,
  nixgg, # nixggBin: $out/bin/nixgg + $out/shims/{cc,c++,ar,...}
  bash,
  coreutils,
  gcc,
  gnumake,
  system, # target platform for the JSON drv `nixgg assemble` builds.
  nixpkgsPath,
  config,
  stdenvNoCC, # for the configureSrcFilter derivation (copies files only).
}:

{
  stdenv,
  # .overrideAttrs on the RETURNED package can't reach group A or
  # group B — nixpkgs' .override always re-invokes with the
  # original attrs first, before either is closed over. Group C IS
  # reachable via .overrideAttrs; extraGroupCAttrs exists for
  # symmetry, same rationale as dynDrvStdenv's extraPhase2Attrs.
  extraGroupAAttrs ? (finalAttrs: old: old),
  extraGroupBAttrs ? (finalAttrs: old: old),
  extraGroupCAttrs ? (finalAttrs: old: old),
  # Opt-in only, default null (no filtering — always safe). Same
  # shape as configureCacheStdenv's own param — see
  # nix/configureSrcFilter.nix.
  configureSrcFilter ? null,
}:

let
  stdenv0 = stdenv;

  defaultMkDerivationFromStdenv =
    stdenv:
    (import "${nixpkgsPath}/pkgs/stdenv/generic/make-derivation.nix" lib config stdenv).mkDerivation;

  mkConfigureSrcFilter = import ./configureSrcFilter.nix { inherit lib stdenvNoCC; };

  # Same NIXGG_* env as dynDrvStdenv.nix's ggShimsOnPath — group A
  # needs this too (shims must be on PATH before configure bakes an
  # absolute compiler path into the generated Makefile), but group A
  # never unsets NIXGG_BYPASS, so it never needs
  # NIXGG_SANDBOX_TARGET/NIXGG_KNOWN_STORE_PATHS to be exactly right
  # — bypassed() short-circuits before any of that matters. Kept
  # anyway for parity/debuggability.
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
        probeArgs = lib.toFunction argsOrFn { };
        drvName = if probeArgs ? name then probeArgs.name else "${probeArgs.pname}-${probeArgs.version}";
        outerName = "gg-build-${drvName}";

        knownStorePathsJSON = builtins.toJSON (
          map toString (
            builtins.concatMap (p: p.all or [ p ]) (
              (probeArgs.buildInputs or [ ]) ++ (probeArgs.propagatedBuildInputs or [ ])
              ++ [ bash coreutils gcc nixgg patchedNix ]
            )
          )
        );

        realOutputs = probeArgs.outputs or [ "out" ];
        extraOutputs = builtins.filter (o: o != "out") realOutputs;
        # Same placeholder scheme as dynDrvStdenv.nix's own
        # outputPlaceholder — group B's environment already expects
        # these exact names (see withGroupBAttrs below), so group A's
        # real output content needs rewriting to match them, not the
        # other way around.
        outputPlaceholder = o: if o == "out" then "/nonexistent" else "/nonexistent-${o}";

        existenceStubs = if configureSrcFilter == null then [ ] else configureSrcFilter.existenceStubs or [ ];

        # configureCacheStdenv's own snapshotScript, unchanged in
        # shape — group A is otherwise byte-for-byte that mechanism.
        snapshotScript = ''
          mkdir -p ${lib.concatMapStrings (o: "\"$" + o + "\" ") realOutputs} "$ggtree"
          cp -a "$NIX_BUILD_TOP/$sourceRoot/." "$ggtree/tree"
          ${lib.concatMapStrings (
            p: "rm -f \"$ggtree\"/tree/${lib.escapeShellArg p}\n"
          ) existenceStubs}
          realpath --relative-to="$NIX_BUILD_TOP/$sourceRoot" "$PWD" > "$ggtree/.gg-cwd"
          printf '%s' "$NIX_BUILD_TOP/$sourceRoot" > "$ggtree/.gg-buildroot"
        '';

        withGroupAAttrs =
          finalAttrs:
          let
            orig = lib.toFunction argsOrFn finalAttrs;

            groupASrc =
              if configureSrcFilter == null then
                orig.src
              else
                mkConfigureSrcFilter {
                  name = "${orig.pname or orig.name}-configure-src";
                  src = orig.src;
                  includePatterns = configureSrcFilter.includePatterns;
                  existenceStubs = configureSrcFilter.existenceStubs or [ ];
                };

            base =
              orig
              // {
                name =
                  if orig ? name then "${orig.name}-configure" else "${orig.pname}-configure-${orig.version}";
                __structuredAttrs = false;
                dontBuild = true;
                dontInstall = true;
                dontFixup = true;
                doCheck = false;
                doInstallCheck = false;
                doDist = false;
                src = groupASrc;
                outputs = realOutputs ++ [ "ggtree" ];
                __contentAddressed = true;
                outputHashMode = "nar";
                outputHashAlgo = "sha256";
                # Shims on PATH from postPatch (same reason as
                # dynDrvStdenv.nix's own Gotcha #2: cmake bakes an
                # ABSOLUTE compiler path into its generated Makefile
                # at configure time, so the shim must already be on
                # PATH then or the real gcc-wrapper's path gets baked
                # instead and buildPhase never routes through the
                # shim at all). NIXGG_BYPASS is set and NEVER unset —
                # group A has no buildPhase to unset it before, and
                # every shim call here is a real, inert passthrough
                # exec (bypassed() in go/internal/shim/passthrough.go),
                # so nothing here needs builder-rpc-v0 or
                # requiredSystemFeatures at all.
                postPatch = (orig.postPatch or "") + ''
                  export NIXGG_BYPASS=1
                  ${ggShimsOnPath knownStorePathsJSON}
                '';
                postConfigure = (orig.postConfigure or "") + snapshotScript;
              };
          in
          extraGroupAAttrs finalAttrs base;

        groupA = mkDerivationSuper withGroupAAttrs;

        # Group A's configure baked ITS OWN real output paths into
        # generated build files. Group B's environment uses
        # dynDrvStdenv's PLACEHOLDER scheme, not a second set of real
        # paths (unlike configureCacheStdenv's pathRewriteScript,
        # which rewrites real→real) — so every occurrence needs
        # rewriting to the placeholder strings group B already
        # declares, before buildPhase runs.
        pathRewriteScript =
          let
            sedOrder = extraOutputs ++ [ "out" ];
            sedExprs = lib.concatMapStrings (
              o: " -e \"s|" + toString groupA.${o} + "|" + outputPlaceholder o + "|g\""
            ) sedOrder;
          in
          ''
            while IFS= read -r -d "" gg_f; do
              gg_ref="$(mktemp)"
              touch -r "$gg_f" "$gg_ref"
              sed -i${sedExprs} "$gg_f"
              touch -r "$gg_ref" "$gg_f"
              rm -f "$gg_ref"
            done < <(grep -rlZI -F "/nix/store/" "$NIX_BUILD_TOP" 2>/dev/null)
          '';

        withGroupBAttrs =
          finalAttrs:
          let
            # Real finalAttrs fixed point, not probeArgs — see group
            # A's own orig binding above for why (some packages read
            # finalAttrs inside a hook).
            orig = lib.toFunction argsOrFn finalAttrs;
            base =
              orig
              // {
                name = "${outerName}.drv"; # submit-output requires this
                                            # to match outputPathName(outerName, "out")
                # Group B always runs its OWN real unpack+patch
                # against the REAL, unfiltered src, unconditionally:
                # when configureSrcFilter is set, group A's snapshot
                # only has the filtered subset, so buildPhase needs
                # the full real tree present before overlaying group
                # A's configure output on top. A hardcoded phases
                # string (rather than dontConfigure toggles) is
                # needed here to splice ggRestorePhase in between
                # patchPhase and buildPhase — same reasoning as
                # dynDrvStdenv's own phase 2 (checkPhase/installPhase
                # use their own custom-phase insertion for the same
                # structural reason).
                phases = "unpackPhase patchPhase ggRestorePhase buildPhase";
                doCheck = false;
                dontInstall = true;
                dontFixup = true;
                doInstallCheck = false;
                doDist = false;
                outputs = [ "out" ];
                out = "/nonexistent";
                # Same reason as dynDrvStdenv.nix's own phase 1:
                # make-derivation.nix appends "debug" to outputs at
                # its own layer when separateDebugInfo is set,
                # downstream of this override, so it needs forcing
                # off too or a package like openssl reintroduces a
                # second output and trips the "*.drv" single-output
                # requirement.
                separateDebugInfo = false;
                # Same convention as dynDrvStdenv's own phase 1: shims go
                # on PATH from postPatch (runs before ggRestorePhase),
                # not inline in ggRestorePhase itself — keeps the restore
                # phase focused on the tree-copy/path-rewrite it's there
                # for. NIXGG_BYPASS stays set through the restore/rewrite
                # (nothing execs a shim there); preBuild unsets it below.
                postPatch = (orig.postPatch or "") + ''
                  export NIXGG_BYPASS=1
                  ${ggShimsOnPath knownStorePathsJSON}
                '';
              }
              // builtins.listToAttrs (
                map (o: {
                  name = o;
                  value = outputPlaceholder o;
                }) extraOutputs
              )
              // {
                __structuredAttrs = false;
                requiredSystemFeatures = (orig.requiredSystemFeatures or [ ]) ++ [ "builder-rpc-v0" ];
                __contentAddressed = true;
                outputHashMode = "text";
                outputHashAlgo = "sha256";
                nativeBuildInputs = (orig.nativeBuildInputs or [ ]) ++ [ patchedNix ];

                # Custom phase name, not a postPatch/preConfigure
                # hook: dontConfigure means runPhase skips
                # configurePhase (and any hook attached to it)
                # entirely, so restoring group A's tree needs its own
                # always-run phase.
                ggRestorePhase = ''
                  runHook preGgRestore
                  cp -a ${groupA.ggtree}/tree/. .
                  chmod -R u+w .
                  gg_oldroot="$(cat ${groupA.ggtree}/.gg-buildroot)"
                  gg_newroot="$PWD"
                  if [ "$gg_oldroot" != "$gg_newroot" ]; then
                    grep -rlZI -F "$gg_oldroot" . 2>/dev/null | while IFS= read -r -d "" gg_f; do
                      gg_ref="$(mktemp)"
                      touch -r "$gg_f" "$gg_ref"
                      sed -i "s|$gg_oldroot|$gg_newroot|g" "$gg_f"
                      touch -r "$gg_ref" "$gg_f"
                      rm -f "$gg_ref"
                    done
                  fi
                  cd "$(cat ${groupA.ggtree}/.gg-cwd)"
                  ${pathRewriteScript}
                  runHook postGgRestore
                '';
                # NIXGG_BYPASS unset here, same as dynDrvStdenv's own
                # preBuild — buildPhase is where real acceleration
                # turns on.
                preBuild = ''
                  unset NIXGG_BYPASS
                '' + (orig.preBuild or "");

                postBuild = (orig.postBuild or "") + submitBuildTreeScript outerName;
              };
          in
          extraGroupBAttrs finalAttrs base;

        groupB = mkDerivationSuper withGroupBAttrs;

        builtTree = builtins.outputOf groupB.outPath "out";

        # See dynDrvStdenv.nix's own restoreOutputsScript for why the
        # `[ -d ... ]` guard is needed (openssl-style packages that
        # split outputs themselves in postInstall against the real
        # $out, never populating $DESTDIR<placeholder> for that
        # output at all), why `mkdir -p "${v}"` must live INSIDE that
        # guard rather than run unconditionally first (a package's own
        # postInstall may `mkdir` — no `-p` — an output dir itself,
        # expecting it not to exist yet; openssl's own `mkdir $dev` is
        # exactly this), and why there are TWO possible source
        # locations per output, not one (a package's own
        # makeFlags/installFlags can point straight at the real,
        # absolute final path instead of the placeholder scheme —
        # openssl's `MANDIR=$(man)/share/man` — which also ends up
        # DESTDIR-prefixed once DESTDIR is threaded onto make's
        # command line).
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
      in
      mkDerivationSuper (
        finalAttrs:
        let
          # Real finalAttrs fixed point, not probeArgs — see group A's
          # own orig binding above for why.
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
            };
        in
        extraGroupCAttrs finalAttrs base
      );
  }
)
