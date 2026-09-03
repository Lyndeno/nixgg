# Archive CA derivation.
#
# `inputs` is a native Nix list of { drv, name }. Same shape as linker.nix.
#
# The `ar` command comes from Go — see resolve-script.nix.
#
# Note `compilerRoot` here is whatever provides `ar` (binutils), not a
# compiler. The name is historical: it is the third positional toolchain
# root every helper takes, and this one puts it on PATH for `ar`. Go's
# side of the same value is the Derivation.AR field.
{
  compilerRoot  ? (import ./toolchain.nix).compilerRoot,
  bashRoot      ? (import ./toolchain.nix).bashRoot,
  coreutilsRoot ? (import ./toolchain.nix).coreutilsRoot,
  outName,
  # See linker.nix's own docstring for this param — same mechanism,
  # default "ar-<outName>" convention preserved.
  name ? "ar-${outName}",
  inputs,
  scriptTemplate,
  markerTag,
  storeDepsJSON ? "[]",
  wrapperEnvJSON ? "{}",
}:
let
  pureStorePath = import ./pure-store-path.nix;
  bash        = pureStorePath bashRoot;
  coreutils   = pureStorePath coreutilsRoot;
  compiler    = pureStorePath compilerRoot;
  storeDeps   = map pureStorePath (builtins.fromJSON storeDepsJSON);
  wrapperEnv  = builtins.fromJSON wrapperEnvJSON;
  script      = import ./resolve-script.nix {
    inherit scriptTemplate markerTag coreutils compiler inputs;
  };
in
derivation ({
  name = name;
  system = builtins.currentSystem;

  __contentAddressed = true;
  outputHashMode = "nar";
  outputHashAlgo = "sha256";

  builder = "${bash}/bin/bash";
  args = [ "-c" script ];

  _storeDeps = builtins.concatStringsSep ":" storeDeps;
} // wrapperEnv)
