# Minimal thin-archive fixture — the smallest possible reproduction
# of the shape QEMU's meson build hits at scale (`ar --thin`, an
# archive whose consuming link needs the archive's own MEMBERS
# declared as its own inputs, not just the archive itself). See
# go/internal/members's own package docstring for the mechanism this
# proves, and tests/thin-archive-equivalence.sh for the native/sandbox
# byte-identity check that consumes this fixture.
#
# Two object files (foo.o, bar.o) go into a THIN archive (`ar csrDT`,
# no bytes embedded — just a path reference to each member), which is
# then linked into a real binary. If member propagation didn't work,
# the sandboxed link would either fail outright (member paths never
# mounted into its sandbox) or — worse — silently produce a
# byte-divergent drv between native and sandbox mode.
{
  mkNixggBuild,
  lib,
}:

let
  thinArchiveSrc = lib.cleanSourceWith {
    src = ./src;
    filter = path: type:
      let name = baseNameOf path; in
      name == "main.c" || name == "foo.c" || name == "bar.c" || name == "Makefile";
  };
in

mkNixggBuild {
  pname = "thin-archive";
  version = "0";
  src = thinArchiveSrc;
  targets = [ { name = "thin-archive"; path = "thin-archive"; } ];
  buildCommand = "make";
}
