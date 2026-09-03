# dynDrvStdenv applied to zstd, demonstrating the phase-chaining
# pattern for packages that exec one of their own binaries mid-build.
#
# zstd's cmake graph (with ZSTD_BUILD_CONTRIB on, nixpkgs' own default)
# execs `contrib/gen_html` mid-build to render zstd_manual.html. In
# sandbox mode that fails: the shim's link step leaves a drvref stub in
# place of a real executable, so `./gen_html` gets "Permission
# denied".
#
# A plain .overrideAttrs patch can't fix this: nixpkgs' own
# .override/.overrideAttrs reapplication always rebuilds the wrapped
# package from its ORIGINAL attrs first, and dynDrvStdenv's phase1 is
# closed over inside that call — before any .overrideAttrs the caller
# wrote gets a chance to run (verified directly: both orderings produce
# a byte-identical phase1 hash to the unpatched build). Use
# dynDrvStdenv's `extraPhase1Attrs` instead — spliced in before phase1
# is computed.
#
# Two-phase structure, same shape as examples/two-phase.nix:
#   phase A (mkNixggBuild) -> builds gen_html.cpp standalone (one TU,
#            no cmake) into a real binary.
#   phase B (dynDrvStdenv, extraPhase1Attrs) -> zstd's real cmake
#            build, patched so gen_html's CMakeLists.txt calls phase
#            A's binary directly instead of building+execing its own.
{
  pkgs,
  mkNixggBuild,
  dynDrvStdenv,
}:

let
  # gen_html.cpp is self-contained (iostream/fstream/sstream/vector
  # only) — a plain single-TU mkNixggBuild call, same shape as
  # examples/two-phase/codegen.
  genHtml = mkNixggBuild {
    pname = "zstd-gen-html";
    version = "0";
    src = pkgs.zstd.src;
    targets = [ { name = "gen_html"; path = "gen_html"; } ];
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
      # Removes gen_html's add_executable + DEPENDS edge, and points
      # GENHTML_BINARY at phase A's binary instead. Every other TU
      # still goes through dynDrvStdenv's real shim acceleration
      # unmodified.
      postPatch =
        old.postPatch
        + ''
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
