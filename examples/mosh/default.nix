# mosh — mobile shell. Real-world autoconf + protoc + libtool +
# pkg-config example.
#
# Configure runs with NIXGG_BYPASS=1 (see nixgg/nix/mkNixggBuild.nix)
# — autoconf conftests need real .o files, which sandbox mode
# doesn't provide. Once configure completes, `make` runs with
# shims on and every cc/c++/ar becomes a nixgg drv.
#
# Library deps go through `buildInputs` and stdenv handles the rest:
# NIX_CFLAGS_COMPILE gets -isystem for each .dev output, NIX_LDFLAGS
# gets -L / -rpath, and propagatedBuildInputs pulls in transitive
# deps (that's how protobuf's absl requirement is satisfied).
{
  mkNixggBuild,
  src,
  autoconf,
  automake,
  libtool,
  pkg-config,
  perl,
  protobuf,
  which,
  gnum4,
  gnugrep,
  gnused,
  gawk,
  file,
  ncurses,
  openssl,
  zlib,
  abseil-cpp,
  # rpcHelper passthrough — see flake.nix's mosh-helper entry, which
  # sets this true to benchmark internal/helper against this same
  # 30-TU/6-archive fixture (the one drv-equivalence/smoke already use
  # to correctness-verify the helper's concurrent path).
  rpcHelper ? false,
  # batchGroups passthrough — see flake.nix's mosh-batch entry, which
  # sets this to cover every one of mosh's 6 lib*.a archives
  # (crypto/network/terminal/util/statesync/protobufs) at once — a
  # real multi-archive build to measure the batch-derivation feature's
  # actual derivation-count reduction against, distinct from
  # lua-batch's single-archive case.
  batchGroups ? [ ],
}:

mkNixggBuild {
  pname = "mosh";
  version = "unstable";
  inherit src rpcHelper batchGroups;
  # Both default to `--enable-client`/`--enable-server` (yes) in
  # configure.ac — no configure flag needed to get both binaries.
  # mkNixggBuild's multi-target `targets` param (see
  # nix/mkNixggBuild.nix's own docstring) submits both from this one
  # buildCommand invocation instead of discarding mosh-client's link
  # the way a single `target` string used to.
  # A LIST, not an attrset — order matters: the FIRST entry is "the"
  # target for .#mosh's own result/package (see mkNixggBuild.nix's
  # own targets docstring for why an attrset's alphabetical iteration
  # order silently picked mosh-client here during development).
  targets = [ { name = "mosh-server"; path = "mosh-server"; } { name = "mosh-client"; path = "mosh-client"; } ];
  nativeBuildInputs = [
    autoconf
    automake
    libtool
    pkg-config
    perl
    protobuf
    which
    gnum4
    gnugrep
    gnused
    gawk
    file
  ];
  buildInputs = [ ncurses openssl zlib protobuf abseil-cpp ];
  buildCommand = ''
    [[ -x configure ]] || NIXGG_BYPASS=1 ./autogen.sh
    # --disable-dependency-tracking: autoconf-generated Makefiles
    # try to update .deps/*.Tpo sidecar files during compile via
    # gcc's -M flag. Our shim drops -M* (dep files target paths
    # outside the sandbox), and the missing .Tpo trips a
    # `mv: cannot stat` in the Makefile. Turning off tracking
    # sidesteps the whole mechanism; we don't need it — Nix's own
    # CA hashing handles rebuild correctness.
    NIXGG_BYPASS=1 ./configure --disable-hardening --disable-dependency-tracking

    # Recursive SUBDIRS walk. Automake serialises the outer subdir
    # order (crypto → terminal → frontend), so the frontend's
    # `../crypto/libmoshcrypto.a` stub is guaranteed present by the
    # time make enters the frontend subdir even under -jN. Within
    # each subdir the shims fire in parallel — each drv submission
    # is an independent fork+exec into `nix derivation add`.
    make -j"$NIX_BUILD_CORES"
  '';
}
