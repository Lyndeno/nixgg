# Lua 5.4 through nixgg. Plain Makefile, ~30 TUs, one archive
# (liblua.a), two link targets in upstream (lua, luac) — both
# submitted here via mkNixggBuild's multi-target `targets` param
# (see nix/mkNixggBuild.nix's own docstring).
#
# `src` is passed in from the flake's `lua-src` input (a plain
# tarball fetcher). Nothing here knows or cares where it came from.
{
  mkNixggBuild,
  src,
  # batchGroups passthrough — see flake.nix's lua-batch entry, which
  # sets this so tests/batch-drv-equivalence.sh has a small (~30-TU),
  # fast, already-in-tree fixture that's ENTIRELY one archive (liblua.a)
  # feeding one link — the shape tryBatchArchive is built for, and
  # small enough to be the fast/legible check redis-batch's own
  # ~150-TU build isn't.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "lua";
  version = "5.4.7";
  inherit src batchGroups;
  targets = [ { name = "lua"; path = "lua"; } { name = "luac"; path = "luac"; } ];
  # `make linux` from src/ is upstream's Linux recipe: sets
  # SYSCFLAGS + SYSLIBS then recurses into `make all`. `all` builds
  # every .o, ar's them into liblua.a, and links both `lua` and
  # `luac` — each submitted under its own output key.
  buildCommand = ''
    cd src
    make linux CC=cc
  '';
}
