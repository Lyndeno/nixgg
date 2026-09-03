# llvm — C++/cmake+ninja stress test. Three-phase build to satisfy
# LLVM's mid-build tool-exec pattern:
#
#   phase1 — build `llvm-min-tblgen`. LLVM's build depends on this
#            bootstrap tablegen to produce a small set of .inc
#            headers early on (RISCVTargetParserDef.inc etc.),
#            consumed by libLLVMSupport and libLLVMTableGen. The
#            sandbox's compile+archive+link shims produce a drvref-
#            stub graph; the link drv for llvm-min-tblgen submits
#            as phase1's outer output so phase2/phase3 can exec
#            real bytes.
#
#   phase2 — build the full `llvm-tblgen`. Depends on
#            libLLVMTableGen + libLLVMSupport (heavier than
#            llvm-min-tblgen). Consumed later by LLVM's proper
#            target-generation steps (`-gen-instr-info` etc.). We
#            configure with `-DLLVM_TABLEGEN=${phase1}/llvm-min-tblgen`
#            so cmake doesn't try to rebuild llvm-min-tblgen from
#            scratch.
#
#   phase3 — the actual LLVM build. Target is `llc`, the static
#            compiler: its dependency graph pulls in libLLVMCore,
#            CodeGen, Analysis, MC, AsmPrinter, and the X86 backend —
#            i.e. most of what people mean by "LLVM". Both tblgens are
#            pre-realised store paths in `toolbin`, a symlink-dir passed
#            via `-DLLVM_NATIVE_TOOL_DIR=${toolbin}`. cmake's
#            LLVM_NATIVE_TOOL_DIR hook finds each binary there and skips
#            its build target; every downstream `.inc` generation step
#            now execs a real ELF, not a drvref stub.
#
#            Target choice matters more than it looks: ninja builds only
#            what the named target needs, so an earlier version of this
#            file targeted `llvm-config` and quietly built just ~480 TUs
#            across 5 libraries (llvm-config is a config-printing
#            utility whose whole graph is Support + TargetParser +
#            Demangle — it never touches the compiler). `llc` is a real
#            consumer of the library graph. Don't "simplify" this back
#            to a smaller tool.
#
# Scope: llvm only (no clang/lld/lldb/mlir/polly/etc.), X86 target
# only. Still ~2000 TUs; enough to exercise every code path the
# shim/drv-graph might trip on.
#
# `src` is a monorepo checkout, so llvm/, cmake/, and third-party/ are
# already siblings — which is what llvm/CMakeLists.txt's `include()`s
# expect. Pinned to 19.x rather than 18.x because 18 predates GCC 15's
# stricter transitive-include behavior; see flake.nix's llvm-src comment.
#
# Verified end to end: all three phases build and every artifact is a
# real, runnable ELF. The phase chain is what makes that possible:
# phase2's cmake execs phase1's binary mid-build, which a drvref stub
# could never satisfy.
{
  mkNixggBuild,
  runCommand,
  src,               # llvm monorepo checkout (llvm/ + cmake/ + third-party/)
  cmake,
  ninja,
  pkg-config,
  python3,
  perl,
  which,
  libffi,
  libxml2,
  ncurses,
  zlib,
  # batchGroups passthrough — see flake.nix's llvm-min-tblgen-batch
  # entry. Applied to all three phases uniformly (same param name,
  # same patterns): each libLLVM<Name>.a archive lives in its own
  # llvm/lib/<Name>/ subdirectory (Support, TableGen, IR, MC, CodeGen,
  # Analysis, Target/X86, ...), the same per-subsystem-archive shape
  # ffmpeg's per-codec libavcodec.a/libavutil.a/etc. already exercises
  # batching against. Unlike fmt-batch/gcc-batch, none of phase1/2's
  # own link targets (llvm-min-tblgen, llvm-tblgen) are archives
  # themselves, so no NIXGG_SANDBOX_TARGET collision risk the way
  # fmt/gcc's archive-is-the-target case has.
  batchGroups ? [ ],
}:

let
  # Shared cmake configure flags, as a single space-separated line.
  # Deliberately NOT a multi-line ''…'' block with trailing `\`
  # continuations: a phase that appends its own `\`-continued flag after
  # the interpolation would hit the block's trailing newline first, ending
  # the command early and running the next flag as its own command
  # ("-DLLVM_TABLEGEN=…: No such file or directory"). One line composes
  # safely everywhere.
  commonCmakeFlags = builtins.concatStringsSep " " [
    "-DCMAKE_BUILD_TYPE=Release"
    "-DLLVM_TARGETS_TO_BUILD=X86"
    "-DLLVM_ENABLE_PROJECTS="
    "-DLLVM_ENABLE_RTTI=ON"
    "-DLLVM_LINK_LLVM_DYLIB=OFF"
    "-DLLVM_BUILD_LLVM_DYLIB=OFF"
    "-DLLVM_BUILD_TESTS=OFF"
    "-DLLVM_INCLUDE_TESTS=OFF"
    "-DLLVM_BUILD_EXAMPLES=OFF"
    "-DLLVM_INCLUDE_EXAMPLES=OFF"
    "-DLLVM_BUILD_DOCS=OFF"
    "-DLLVM_INCLUDE_DOCS=OFF"
    "-DLLVM_BUILD_BENCHMARKS=OFF"
    "-DLLVM_INCLUDE_BENCHMARKS=OFF"
    "-DLLVM_ENABLE_ZSTD=OFF"
    "-DLLVM_ENABLE_LIBEDIT=OFF"
  ];

  commonNativeBuildInputs = [ cmake ninja pkg-config python3 perl which ];
  commonBuildInputs = [ libffi libxml2 ncurses zlib ];

  phase1 = mkNixggBuild {
    pname = "llvm-min-tblgen";
    version = "19.1.7";
    inherit src batchGroups;
    # Key is a plain name (becomes the output key "llvm-min-tblgen.drv"
    # — a "/" there would be an invalid Nix output name); value is the
    # real relative path the shim matches against -o, same as before.
    targets = [ { name = "llvm-min-tblgen"; path = "bin/llvm-min-tblgen"; } ];
    nativeBuildInputs = commonNativeBuildInputs;
    buildInputs = commonBuildInputs;
    buildCommand = ''
      NIXGG_BYPASS=1 cmake -S llvm -B build -G Ninja ${commonCmakeFlags}
      ninja -C build -j"$NIX_BUILD_CORES" llvm-min-tblgen
    '';
  };

  phase2 = mkNixggBuild {
    pname = "llvm-tblgen";
    version = "19.1.7";
    inherit src batchGroups;
    targets = [ { name = "llvm-tblgen"; path = "bin/llvm-tblgen"; } ];
    nativeBuildInputs = commonNativeBuildInputs;
    # phase1.result gives us a real llvm-min-tblgen at
    # ${phase1.result}/bin/llvm-min-tblgen. Mount it via buildInputs so
    # it's on-disk when cmake fires.
    buildInputs = commonBuildInputs ++ [ phase1.result ];
    buildCommand = ''
      NIXGG_BYPASS=1 cmake -S llvm -B build -G Ninja ${commonCmakeFlags} \
        -DLLVM_TABLEGEN=${phase1.result}/bin/llvm-min-tblgen
      ninja -C build -j"$NIX_BUILD_CORES" llvm-tblgen
    '';
  };

  # Merge phase1 + phase2 outputs into a single tool dir so
  # LLVM_NATIVE_TOOL_DIR can find both binaries.
  toolbin = runCommand "llvm-toolbin-19.1.7" { } ''
    mkdir -p $out
    ln -s ${phase1.result}/bin/llvm-min-tblgen $out/llvm-min-tblgen
    ln -s ${phase2.result}/bin/llvm-tblgen $out/llvm-tblgen
  '';

  phase3 = mkNixggBuild {
    pname = "llvm";
    version = "19.1.7";
    inherit src batchGroups;
    targets = [ { name = "llc"; path = "bin/llc"; } ];
    nativeBuildInputs = commonNativeBuildInputs;
    buildInputs = commonBuildInputs ++ [ toolbin ];
    buildCommand = ''
      NIXGG_BYPASS=1 cmake -S llvm -B build -G Ninja ${commonCmakeFlags} \
        -DLLVM_NATIVE_TOOL_DIR=${toolbin}
      ninja -C build -j"$NIX_BUILD_CORES" llc
    '';
  };
in
{
  inherit (phase3) drv shell result package;
  # Expose intermediates for isolated smoke tests / debugging — the
  # full mkNixggBuild attrset, so flake.nix can pull either .result or
  # .package from these the same way it does for phase3/the top-level
  # examples.
  llvm-min-tblgen = phase1;
  llvm-tblgen = phase2;
  llvm-toolbin = toolbin;
}
