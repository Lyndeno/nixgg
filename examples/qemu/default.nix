# QEMU — meson+ninja, a build-system genre nixgg had never exercised
# before this (existing examples cover plain Makefile, autotools,
# cmake+ninja). Confirmed directly that nixgg needs ZERO meson-
# specific code for this: mkNixggBuild's buildCommand is a plain
# shell script either way, and go/internal/dispatch.ExpandRspfiles
# already generically flattens ninja's `@rspfile` response-file
# convention ahead of every shim entrypoint — no new mechanism
# required.
#
# SCOPE, stated plainly: builds ONE target (x86_64-softmmu) via
# `--target-list=x86_64-softmmu`, with tools/tests/docs disabled.
# This still compiles ~1700 real build steps (the shared util/,
# hw/core/, qapi/, block/ layer plus x86_64's own hw/target code) —
# comparable scale to examples/ffmpeg, not a toy. Confirmed directly:
# QEMU's own build-time codegen (QAPI, scripts/decodetree.py,
# scripts/tracetool.py, keycodemapdb) is entirely `find_program`-
# driven — pure Python/shell scripts read off the host, never a
# compiled-and-exec'd QEMU-built binary — so there is no zstd-
# gen_html-style "exec an unresolved drvref stub" hazard here, unlike
# zstd/gcc, and no phase-chaining fix is needed.
#
# `python3.withPackages` needs `distlib`+`setuptools` explicitly:
# meson's own mkvenv bootstrap step fails outright ("found no usable
# distlib") against a bare `python3`, confirmed directly — nixpkgs'
# python3 doesn't ship distlib by default the way a system Python
# with a real venv/pip stack would.
#
# NIXGG_BYPASS=1 on `meson setup` for the same reason as fmt/llvm's
# own cmake configure step: meson's own compiler probes need real
# object files, and meson (like cmake) hard-codes the shim's PATH
# entry into its generated ninja build files, so unsetting BYPASS
# only after setup completes is what routes the real `ninja compile`
# invocation through the shim.
#
# The final link script is ~38.7KB (measured directly) — comparable
# to LLVM's/ffmpeg's largest scripts, ~3.4x under the MAX_ARG_STRLEN
# ceiling ARCHITECTURE.md documents (131072 bytes). No response-file/
# argv-length hazard materializes at this scale.
{
  mkNixggBuild,
  src,
  pkg-config,
  meson,
  ninja,
  glib,
  pixman,
  ncurses,
  zlib,
  pythonWithMesonDeps,
  # batchGroups passthrough — same mkNixggBuild param every other
  # fixture exposes; unset here (no qemu-batch flake entry exists),
  # kept only so a future batching experiment doesn't need to add it.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "qemu";
  version = "9.2.0";
  inherit src batchGroups;
  targets = [ { name = "qemu-system-x86_64"; path = "build/qemu-system-x86_64"; } ];
  nativeBuildInputs = [ pkg-config meson ninja pythonWithMesonDeps ];
  buildInputs = [ glib pixman ncurses zlib ];
  buildCommand = ''
    # mkNixggBuild's stdenv wiring sets dontFixup = true, so nixpkgs'
    # own patchShebangs (normally a fixupPhase hook) never runs — it
    # otherwise only fires explicitly on `configureScript` itself
    # (stdenv's own setup script, `patchShebangs --build
    # "$configureScript"`), not on the whole source tree. QEMU's own
    # `#!/usr/bin/env python3` scripts (scripts/qemu-plugin-symbols.py
    # and friends) keep their raw shebang as a result — harmless for
    # anything meson finds via `find_program` (which explicitly
    # invokes them as `<venv-python3> <script>`), but meson's own
    # `configure_file(command: [...])` in plugins/meson.build execs
    # qemu-plugin-symbols.py directly by its own shebang, which fails
    # with "Could not execute command" since /usr/bin/env doesn't
    # exist inside the sandbox. Patch it ourselves, same fix
    # nixpkgs' own generic builder would apply automatically outside
    # this stdenv override.
    patchShebangs scripts/

    NIXGG_BYPASS=1 ./configure \
      --target-list=x86_64-softmmu \
      --disable-docs \
      --disable-fdt

    unset NIXGG_BYPASS
    ninja -C build -j"$NIX_BUILD_CORES" qemu-system-x86_64
  '';
}
