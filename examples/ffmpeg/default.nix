# ffmpeg — the stress test for shim submission throughput.
# ~1200 translation units under a plain autoconf-like `./configure`
# (custom, not GNU autotools) + recursive make. No optional
# codec/library flags are on: this is the "core ffmpeg only" build,
# which still exercises the full TU count and gives us a clean
# fork+exec-per-drv timing without the noise of a dozen external
# dependency setups.
#
# Configure phase runs under NIXGG_BYPASS=1 like mosh's: ffmpeg's
# configure script compiles many probe programs that need runnable
# binaries; the shim's drvref stub doesn't help there. Once
# configure completes, `make -j"$NIX_BUILD_CORES"` recurses and
# every cc invocation becomes a nixgg drv.
#
# Verified: 106 MB `ffmpeg_g` ELF builds in ~45s cold. Two shim
# fixes were needed to get here:
#
#   1. The scanner used to add `srcDir` to userDirs (which drove both
#      projectRoot widening AND -I flag emission). For ffmpeg's
#      `cc -c libavutil/parseutils.c -I. -Ilibavutil`, that produced
#      a spurious `-Ilibavutil` in the drv, pointing at $src/libavutil
#      after `cd $src`. libavutil/time.h then shadowed glibc's
#      <time.h> (because -I dirs win over -isystem), and `struct tm`
#      went missing. Fix: separate projectRootHints (srcDir + cwd +
#      caller dirs) from callerDirs (only what the caller explicitly
#      passed). See internal/scan/scan.go.
#
#   2. The link shim emitted flags before inputs (`cc <flags> <objs>`).
#      ffmpeg's link line has `-lm -latomic` in flags but `libavutil.a`
#      etc. as inputs; ld resolves libraries against objects seen
#      *before* them, so `-lm` at the front left every math symbol
#      unresolved. Fix: split `-l<name>` out of flags and emit them
#      AFTER inputs. Applied symmetrically to nix/linker.nix so
#      native and sandbox drvs remain byte-identical (all four
#      existing fixtures still equivalent).
{
  mkNixggBuild,
  src,
  pkg-config,
  perl,
  nasm,
  yasm,
  gnumake,
  which,
  # batchGroups passthrough — see flake.nix's ffmpeg-batch entry.
  # ffmpeg's own link target is ffmpeg_g (the binary), not any one
  # of its 8 per-directory static libs (libavcodec.a/libavutil.a/
  # libavformat.a/libavdevice.a/libavfilter.a/libswresample.a/
  # libswscale.a/libpostproc.a), so unlike fmt-batch/gcc-batch none
  # of them collide with NIXGG_SANDBOX_TARGET — batching should
  # actually engage for all of them, on the largest TU count in the
  # repo (~1200).
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "ffmpeg";
  version = "7.1.2";
  inherit src batchGroups;
  # Stop at ffmpeg_g (unstripped). ffmpeg's Makefile normally does
  # `strip ffmpeg_g -o ffmpeg` after the link — but the link shim
  # left a drvref stub at ffmpeg_g (not a real ELF), so strip
  # would fail with "file format not recognized". Submitting the
  # unstripped variant sidesteps that; a downstream step could
  # realize+strip if needed, but for a build-throughput benchmark
  # the unstripped binary is what we want anyway.
  target = "ffmpeg_g";
  nativeBuildInputs = [ pkg-config perl nasm yasm gnumake which ];
  buildInputs = [ ];
  buildCommand = ''
    # ffmpeg configure has no autoconf; it's a bespoke shell script.
    # NIXGG_BYPASS so probe compilations hit the outer cc-wrapper
    # directly (drvref stubs are opaque to configure's `./conftest`
    # exec-then-check pattern).
    #
    # --disable-doc: avoids texinfo / makeinfo dependency.
    # --disable-htmlpages / manpages / podpages: same.
    # --disable-x86asm: skip nasm probing. Enable if you actually
    #   want the accelerated codepaths — for a shim-throughput test
    #   the pure-C path exercises more TUs per drv equally.
    # --cc="cc": explicit; ffmpeg's configure defaults to whatever's
    #   in $CC (which our preBuild unsets to let the argv0-based
    #   default win).
    NIXGG_BYPASS=1 ./configure \
      --cc=cc \
      --disable-doc \
      --disable-htmlpages \
      --disable-manpages \
      --disable-podpages \
      --disable-x86asm

    make -j"$NIX_BUILD_CORES" ffmpeg_g
  '';
}
