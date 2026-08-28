# Same as hello-mkbuild.nix, but with rpcHelper = true — a dedicated
# fixture for internal/helper (the optional persistent daemon-side
# relay for internal/rpc's three sandbox ops). See mkNixggBuild.nix's
# own `rpcHelper` parameter docstring for the mechanism and the
# ~4.3ms-daemon-handshake-vs-~23µs-pooled-op measurement it rests on.
#
# Kept as its own fixture rather than a flag on hello-mkbuild.nix
# because the two need to stay independently buildable: this repo's
# tests need a way to build .#hello WITHOUT the helper (the default,
# already-verified path) and .#hello-helper WITH it, side by side, not
# a single toggle that changes what .#hello itself means.
{
  mkNixggBuild,
  lib,
}:

let
  exampleSrc = lib.cleanSourceWith {
    src = ./../example;
    filter = path: type:
      let name = baseNameOf path; in
      name == "main.cc" || name == "util.cc" || name == "util.h"
      || name == "Makefile";
  };
in

mkNixggBuild {
  pname = "hello-helper";
  version = "0";
  src = exampleSrc;
  target = "hello";
  buildCommand = "make";
  rpcHelper = true;
}
