# redis — plain-Makefile server. Cross-referenced against
# nixpkgs/pkgs/by-name/re/redis/package.nix; same nativeBuildInputs,
# same use-system-lua strategy. No autoconf, no cmake, but the
# top-level Makefile recurses into deps/{hiredis,linenoise,lua,
# hdr_histogram,fpconv,fast_float,jemalloc} — each is its own make
# that produces a .a. The shim's archive path handles them the same
# way it handles mosh's src/*/lib*.a.
#
# Two redis-specific quirks worth noting:
#
#   1. SOURCE_DATE_EPOCH. src/mkreleasehdr.sh bakes
#      `<hostname>-<epoch>` into release.o. Without a pinned epoch,
#      every build produces a different CA hash for redis-server —
#      cache misses forever. Any fixed integer works; the historical
#      value in the native-mode script is 1700000000.
#
#   2. `make PREFIX=$out` mirrors nixpkgs' flag — routes install
#      paths to $out even though our submit-output path doesn't
#      really need it (the outer drv's out is /nonexistent). Kept
#      for consistency and in case a follow-up phase installs.
{
  mkNixggBuild,
  src,
  which,
  pkg-config,
  python3,
  lua,           # system lua, matches nixpkgs's approach
  gnugrep,
  gnused,
  gawk,
  # rpcHelper passthrough — see flake.nix's redis-helper entry, which
  # sets this true to benchmark internal/helper against a build large
  # enough (175 TUs) to show whether the win scales with TU count the
  # way mosh's own 30-TU measurement (README.md's "Optional: a
  # persistent helper" section) predicted it should.
  rpcHelper ? false,
  # batchGroups passthrough — see flake.nix's redis-batch-probe entry,
  # which sets this to nix/batchGroupPresets.nix's vendorDeps preset
  # to confirm real classification against redis's own deps/ tree —
  # exactly the vendored-and-rarely-edited case that motivated
  # go/internal/batch. Only 5 of deps/'s 7 subtrees are reachable
  # from this build's redis-server target at all (MALLOC=libc excludes
  # jemalloc; linenoise is redis-cli-only), and all 5 batch cleanly:
  # 5 of 5 collapse into 5 batch-lib*.a.drv derivations. jemalloc
  # (its own autotools ./configure + make, a separate integration
  # effort) and linenoise (never archived — its Makefile links
  # linenoise.o directly as a single object, so it can never be a
  # batch target regardless of MALLOC) are out of scope here, not
  # missed coverage.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "redis";
  version = "8.2.2";
  inherit src rpcHelper batchGroups;
  targets = [ { name = "redis-server"; path = "redis-server"; } ];
  # Same set nixpkgs uses, plus grep/sed/awk that redis's release
  # scripts shell out to.
  nativeBuildInputs = [ pkg-config which python3 gnugrep gnused gawk ];
  buildInputs = [ lua ];
  buildCommand = ''
    export SOURCE_DATE_EPOCH=1700000000

    # Nixpkgs style: just `make` — no persist-settings dance, no
    # per-deps explicit invocation. Redis's top-level Makefile
    # recurses through deps/ before src/ on its own.
    make -j"$NIX_BUILD_CORES" MALLOC=libc redis-server
  '';
}
