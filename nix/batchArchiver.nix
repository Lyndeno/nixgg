# Combined compile+archive CA derivation for a same-group batch (see
# go/internal/batch and go/internal/expr/batcharchive.go).
#
# Unlike builder.nix/archiver.nix, this Kind never needs
# resolve-script.nix's marker substitution: every input is a plain
# staged source tree the caller (go/internal/shim's tryBatchArchive)
# already confirmed belongs to this one batch — never a not-yet-
# realized sibling drv/thunk, which is the only thing markers exist
# to defer. Go renders each member's own compile line as fully
# shell-quoted, already-resolved text (compileLine); the ONE thing
# left for this file to interpolate is each member's own srcTree path
# literal, the same way builder.nix interpolates its single srcTree —
# Nix resolves a path literal to a store path at eval time, so Go
# never needs to know the resolved path to reference it correctly.
#
# `compilerRoot` here (as in archiver.nix) is whatever provides `ar`
# on PATH — go/internal/expr/batcharchive.go's own BatchArchiveJSON
# puts the SAME root on PATH for every member's own compile, matching
# every current fixture's assumption of one toolchain per build (see
# BatchArchiveJSONParams.AR's own docstring).
{
  compilerRoot ? (import ./toolchain.nix).compilerRoot,
  bashRoot ? (import ./toolchain.nix).bashRoot,
  coreutilsRoot ? (import ./toolchain.nix).coreutilsRoot,
  outName,
  arFlags,
  # [ { srcTree, outName, compileLine } ... ], in the archive's own ar
  # argv order — srcTree is a Nix path literal (unquoted at the Go
  # call site, see batcharchive.go's batchMembersList), outName is
  # this member's own object filename (matches the $objroot/<outName>
  # compileLine already writes to), compileLine is a complete,
  # already shell-quoted `"$tool" ...flags... -c "source" -o
  # "$objroot/outName"` invocation missing only its own `cd`.
  members,
  storeDepsJSON ? "[]",
  wrapperEnvJSON ? "{}",
}:
let
  pureStorePath = import ./pure-store-path.nix;
  bash = pureStorePath bashRoot;
  coreutils = pureStorePath coreutilsRoot;
  compiler = pureStorePath compilerRoot;
  storeDeps = map pureStorePath (builtins.fromJSON storeDepsJSON);
  wrapperEnv = builtins.fromJSON wrapperEnvJSON;

  # One `(cd <srcTree> && <compileLine>)` per member, in order, then
  # one `ar` invocation over every member's own $objroot/<outName>.
  # Mirrors go/internal/expr/batcharchive.go's batchArchiveScript
  # exactly — this file's job is ONLY to splice in each member's own
  # srcTree, everything else is already-resolved text from Go.
  #
  # Plain builtins.concatStringsSep, not lib.concatMapStrings(Sep) —
  # this file must not depend on <nixpkgs> (see builder.nix's own
  # docstring on why: parallel invocations racing on nixpkgs input
  # evaluation), so it can't take `lib` as a param the way the
  # flake-level stdenv wrappers do.
  compileLines = builtins.concatStringsSep ""
    (map (m: "(cd ${m.srcTree} && ${m.compileLine})\n") members);
  objList = builtins.concatStringsSep " "
    (map (m: ''"$objroot/${m.outName}"'') members);

  script = ''
    set -euo pipefail
    export PATH="${coreutils}/bin:${compiler}/bin"
    mkdir -p "$out/lib" .nixgg-objs
    objroot="$PWD/.nixgg-objs"
    ${compileLines}ar D${arFlags} "$out/lib/${outName}" ${objList}
  '';
in
derivation ({
  name = "batch-${outName}";
  system = builtins.currentSystem;

  __contentAddressed = true;
  outputHashMode = "nar";
  outputHashAlgo = "sha256";

  builder = "${bash}/bin/bash";
  args = [ "-c" script ];

  _storeDeps = builtins.concatStringsSep ":" storeDeps;
} // wrapperEnv)
