# mkNixggBuild per-TU rebuild-scope test fixture, driven by
# tests/perf-regression.sh. Same shape as
# tests/configure-cache-cutoff-fixture.nix (constructs the package
# DIRECTLY from the pinned flake input, not via the flake's own
# `.#lua` output — that output's src is fixed, so there is no way to
# thread an edited src through it): a one-file source edit, built
# through the exact same mkNixggBuild call examples/lua/default.nix
# makes, so the resulting drv graph is the real one, not a synthetic
# stand-in.
{
  flakeDir, # path to the nixgg checkout, passed by the driver script
  edit ? null, # null | a relative path inside lua's src/ to touch
}:
let
  flake = builtins.getFlake (toString flakeDir);
  system = builtins.currentSystem;
  pkgs = flake.inputs.nixpkgs.legacyPackages.${system};
  mkNixggBuild = flake.outputs.packages.${system}.mkNixggBuild;

  realSrc = flake.inputs.lua-src;

  editedSrc =
    pkgs.runCommand "lua-src-edited" { } ''
      mkdir -p "$out"
      cp -a ${realSrc}/. "$out"
      chmod -R u+w "$out"
      printf '\n/* perf-regression touch */\n' >> "$out"/${edit}
    '';

  src = if edit == null then realSrc else editedSrc;
in
import ../examples/lua { inherit mkNixggBuild src; }
