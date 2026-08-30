# {fmt} — cmake-driven C++ header + library. Exercises three
# nixgg pieces the smaller examples don't:
#
#   - cmake+ninja driving the shims (not `make`);
#   - a *static library* (libfmt.a) as one of the outputs, so the
#     archive shim path matters (not just compile+link);
#   - realise-mode carveout for cmake's compiler probes
#     (CheckCXX*Compiler, CheckIncludeFile, …) — the compile shim's
#     filename heuristic auto-realises those synchronously, so
#     configure sees actual .o files, not thunks.
#
# nativeBuildInputs adds cmake + ninja + pkg-config to the sandbox's
# PATH. stdenv wraps their /bin dirs in and runs their setup hooks
# (cmake's setup hook doesn't auto-configure since we set
# dontConfigure=true in mkNixggBuild).
{
  mkNixggBuild,
  src,
  cmake,
  ninja,
  pkg-config,
  # batchGroups passthrough — see flake.nix's fmt-batch entry. Real
  # test of a documented limitation: fmt's own `target` IS the
  # archive (libfmt.a) batching would apply to, and
  # go/internal/shim/batcharchive.go's tryBatchArchive deliberately
  # refuses to batch whichever archive matches NIXGG_SANDBOX_TARGET
  # (a "batch-"-named drv can't satisfy submit-output's naming
  # contract). So fmt-batch is expected to build correctly but with
  # batching NOT actually engaging — confirms the fallback is safe,
  # not that batching helps here.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "fmt";
  version = "11.0.2";
  inherit src batchGroups;
  # libfmt.a is the "big" output; a header-only variant exists too
  # but we want the archive path exercised.
  target = "libfmt.a";
  nativeBuildInputs = [ cmake ninja pkg-config ];
  buildCommand = ''
    # Configure runs with NIXGG_BYPASS=1 so cmake's compiler probes
    # (CheckCXXCompiler, CheckIncludeFile, etc.) get real binaries.
    # Shims are still on PATH — they just exec-passthrough to the
    # real tool. cmake happily hard-codes the shim path into its
    # generated build files; once we unset NIXGG_BYPASS, subsequent
    # cmake --build invocations go through nixgg normally.
    NIXGG_BYPASS=1 cmake -S . -B build -G Ninja \
      -DCMAKE_BUILD_TYPE=Release \
      -DFMT_TEST=OFF -DFMT_DOC=OFF \
      -DBUILD_SHARED_LIBS=OFF

    cmake --build build --target fmt
  '';
}
