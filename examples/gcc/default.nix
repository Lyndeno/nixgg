# GCC's own libiberty/ — a real static archive (~65 .o members:
# regex.o, cplus-dem.o, hashtab.o, splay-tree.o, xmalloc.o, …) built
# from GCC 15.3.0's source tree.
#
# SCOPE, stated plainly: this does NOT build cc1/cc1plus/xgcc, or any
# part of the actual C/C++ compiler. It builds libiberty/ alone, via
# libiberty's OWN standalone, pre-shipped `./configure` — not gcc's
# top-level multi-package configure.ac/Makefile.def orchestration.
# That distinction is the whole point: a full GCC bootstrap pulls in
# hazards none of nixgg's shims model today, and every one of them is
# injected specifically by the TOP-LEVEL build, not by libiberty on
# its own:
#
#   - GMP/MPFR/MPC hard version requirements (top-level configure.ac
#     only; libiberty's configure never mentions them).
#   - `ar --plugin <liblto_plugin.so path> --plugin <path>` decoration
#     on every AR/RANLIB invocation, injected by top-level
#     configure.ac's unconditional `GCC_PLUGIN_OPTION` call and
#     threaded in via `AR = @AR@ @AR_PLUGIN_OPTION@` (Makefile.tpl).
#     nixgg's `ar` shim (go/internal/shim/archive.go: parseARArgs)
#     expects args[0] to be a bare modifier string; `--plugin` fails
#     that check and falls through to full passthrough — libiberty's
#     own Makefile.in has no AR_PLUGIN_OPTION substitution at all, so
#     this never triggers here.
#   - `ar rcT` thin-archive mode for gcc/'s libbackend.a (458 objects),
#     gated on gcc/Makefile.in's own THIN_ARCHIVE_SUPPORT plumbing.
#     libiberty/Makefile.in has zero USE_THIN_ARCHIVES machinery.
#   - Generated sources exec'd mid-build (insn-*.cc from
#     gcc/config/*/*.md via build/genmodes et al., gtype-desc.cc via
#     gengtype, …) — entirely gcc/-subdir machinery. libiberty has no
#     .md/.def-driven codegen and no build/gen* tools to exec.
#
# None of that is guesswork: this was verified empirically by running
# libiberty's real `./configure --disable-shared && make` against the
# actual gcc-15.2.0/15.3.0 tarball (both share an identical
# libiberty/Makefile.in) and reading the resulting Makefile's AR/
# AR_FLAGS/RANLIB lines and captured build log directly — plain
# `ar  rc ./libiberty.a <65 .o files>` / `ranlib ./libiberty.a`, no
# --plugin, no `T`, no build/gen* tool anywhere in the graph.
#
# If a follow-up wants the real compiler (cc1/cc1plus), the natural
# next step is the phase-chain pattern examples/llvm and
# examples/two-phase establish: one mkNixggBuild per build/gen* tool
# (gengtype, genmodes, genattrtab, …, per gcc/Makefile.in's genprog
# list), merged into a toolbin dir, mounted into a second phase that
# runs `make all-gcc` with AR_FLAGS=rcs / AR_PLUGIN_OPTION= overrides
# on the command line to defeat the plugin/thin-archive hazards above.
# That is real, multi-phase surgery on GCC's own Makefile graph and is
# deliberately NOT attempted here — a small target that genuinely
# builds beats a large one that doesn't.
{
  mkNixggBuild,
  src,
  # batchGroups passthrough — see flake.nix's gcc-batch entry. Same
  # target-is-the-archive limitation as fmt-batch: libiberty.a is
  # this build's own submission target, so tryBatchArchive refuses to
  # batch it (see fmt/default.nix's own comment for why). Wired up
  # anyway to confirm the fallback is correct on a second, larger
  # (~65-member) archive-as-target case.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "gcc-libiberty";
  version = "15.3.0";
  inherit src batchGroups;
  # A target ending in ".a" is produced by the archive shim, not link
  # — fmt's libfmt.a is the existing precedent for an archive-only,
  # no-link target.
  targets = [ { name = "libiberty"; path = "libiberty.a"; } ];
  buildCommand = ''
    cd libiberty
    NIXGG_BYPASS=1 ./configure --disable-shared
    make -j"$NIX_BUILD_CORES"
  '';
}
