# PostgreSQL — a much larger real-world autoconf project than mosh
# (src/backend alone is ~1000+ TUs).
#
# SCOPE, stated plainly: this builds src/backend ONLY — the real
# `postgres` server binary itself, via `./configure && make -C
# src/backend`. It does NOT run `make world`, does NOT build docs,
# contrib/, the client libraries (libpq), or any of the src/bin/
# tools (psql, pg_dump, initdb, ...). Same "deliberately smaller
# target beats a large one that doesn't build" philosophy
# examples/gcc's libiberty-only scope models: src/backend's own
# recursive make graph is real and large enough to stress-test nixgg
# at production scale without pulling in hazards unrelated to that
# goal (ICU's collation-generation codegen, readline detection,
# zlib/openssl linking).
#
# --without-icu/--without-readline/--without-zlib deliberately trim
# nixpkgs' full modern postgresql recipe (which also pulls in
# liburing, systemd, krb5, PAM, numactl, curl, llvm's JIT, and a
# docbook toolchain for docs) down to the minimum that still produces
# a real, runnable postgres binary.
#
# CFLAGS="-std=gnu17" works around GCC 15 treating `bool` as a
# reserved keyword under its new C23 default (`-std=gnu23`) —
# src/include/c.h's own `typedef unsigned char bool;` (pre-C23
# compat shim) fails to compile under that default. Same hazard
# class as examples/llvm's own GCC-15/C++ standard-library note, just
# on the C side.
#
# `make -C src/backend generated-headers` runs FIRST, unshimmed
# (NIXGG_BYPASS=1) and non-parallel, before the real (also
# non-parallel — see below) build: it's what materializes
# `gram.c`/`scan.c` (bison/flex), `errcodes.h`
# (generate-errcodes.pl), and the catalog headers
# (genbki.pl/Catalog.pm) — pure Perl/bison/flex TEXT generation, not
# a compiled-tool-exec-mid-build hazard the way zstd's gen_html is,
# so no phase-chaining fix is needed here.
#
# Deliberately NOT run with `make -j`: confirmed directly that `-j`
# on this SAME subdir-only `make -C src/backend` invocation hits a
# real recursive-make ordering race in PostgreSQL's own
# Makefile.global (parallel sub-targets don't all depend on
# submake-generated-headers the way a top-level `make -j` run would
# see it) — "No rule to make target
# '../../src/common/libpgcommon_srv.a'" — unrelated to nixgg's shims;
# sequential `make` builds cleanly. This means every cc call the
# shim sees here is serialized at the Makefile level, not
# nixgg-imposed — real per-TU acceleration still applies once Nix
# itself schedules the resulting derivations.
{
  mkNixggBuild,
  src,
  bison,
  flex,
  perl,
  # batchGroups passthrough, same shape as every other example's.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "postgresql";
  version = "17.2";
  inherit src batchGroups;
  targets = [ { name = "postgres"; path = "src/backend/postgres"; } ];
  nativeBuildInputs = [ bison flex perl ];
  buildCommand = ''
    NIXGG_BYPASS=1 ./configure \
      --without-icu --without-readline --without-zlib \
      CFLAGS="-std=gnu17 -O2"
    NIXGG_BYPASS=1 make -C src/backend generated-headers
    make -C src/backend
  '';
}
