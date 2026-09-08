# Linux kernel — the largest, most structurally demanding fixture in
# this repo: real Kbuild recursive-make, thousands of TUs, hundreds of
# nested `built-in.a` archives, a raw (non-gcc-driven) `ld` link step,
# GNU as's `.incbin` directive, and host tools (fixdep, Kconfig's
# `conf`, modpost) whose own build/exec timing doesn't fit any prior
# fixture's shape.
#
# SCOPE, stated plainly: `make tinyconfig` (the smallest buildable
# x86_64 config Kbuild ships), target `vmlinux` only — no modules
# (`CONFIG_MODULES` is off in tinyconfig), no `bzImage`/install steps.
# genksyms/modpost's own tool-dispatch situation is UNCONFIRMED for a
# modules-enabled build; widening to full defconfig + modules is
# deliberately a separate, later step, not attempted here.
#
# BOTH MODES WORK, confirmed and retested, not a guess. Native:
# `nix develop .#linux-kernel-shell` running this fixture's own
# buildCommand produces a real, valid ELF vmlinux. Sandbox: `nix
# build .#linux-kernel` (mkNixggBuild's own default) ALSO succeeds —
# confirmed via a real end-to-end sandbox build producing a real,
# valid ELF vmlinux (`file vmlinux` → "ELF 32-bit LSB executable,
# Intel 80386 ... statically linked, not stripped").
#
# Getting sandbox mode working required a two-phase split (see
# `phase1`/`phase2` below), because a single mkNixggBuild derivation
# is structurally unable to satisfy Kbuild's own recipe shape:
# scripts/link-vmlinux.sh reads vmlinux.o/vmlinux back SYNCHRONOUSLY,
# in the SAME recursive make, moments after producing them (objcopy
# for .modinfo; nm+sorttable for System.map/table-sorting). A
# builder-rpc-v0 sandbox's op allowlist for a RecursiveSubmitted
# connection (confirmed directly at the Nix daemon's own source,
# src/libstore/daemon.cc's performOp) has no operation that builds a
# derivation at all — exactly AddToStore{,Multiple,Nar,Scanning},
# SubmitOutput, AddTempRoot, IsValidPath; any other op throws
# "Operation %d not allowed inside derivation" (deliberate upstream
# design, not a client-side gap nixgg's own code could close). Every
# nixgg output is a REGISTERED drv, not real bytes, until Nix's OWN
# outer scheduler resolves it — which only happens after the
# derivation's build script exits, too late for a script that needs
# to read its own output back before it exits. See phase2's own
# docstring for why the fix is a plain `stdenv.mkDerivation`, not
# another mkNixggBuild call — the fix even had to survive ONE more
# instance of the identical wall, self-inflicted by initially trying
# to route phase2's own final link through mkNixggBuild too.
#
# Getting this far required TEN real, previously-unknown gaps in
# nixgg's shims/expression layer, each found and fixed by running this
# exact build for real and reading the failure precisely rather than
# guessing:
#
#   1. `-Wp,-MMD,<path>` depfile synthesis (compile.go) — Kbuild's own
#      `cmd_and_fixdep` macro runs `fixdep <depfile> ...` synchronously,
#      same recipe, right after each compile; nixgg's deferred-compile
#      model means the real depfile is never written in time, so the
#      shim now synthesizes one from scan's own already-computed
#      header list instead.
#   2. Nested `built-in.a` archive members (archive.go) — Kbuild's own
#      recursive archive construction lists a CHILD directory's
#      built-in.a as a MEMBER of the parent's, which the shim's
#      `.o`-only member check used to reject outright.
#   3. A raw `ld`/`ld.bfd`/`ld.gold`/`ld.lld` dispatch role — vmlinux's
#      own final link (and the unconditional arch/x86/realmode/
#      subtree) invoke `ld` directly, never through `cc`/`gcc`.
#   4. `.incbin` targets invisible to header scanning (scan.go) —
#      arch/x86/realmode/rmpiggy.S embeds a previously-built binary
#      blob via GNU as's `.incbin`, which gcc's own `-M`/`-MM` never
#      sees (it only tracks what the PREPROCESSOR consumed).
#   5. scripts/mod/empty.o, arch/x86/realmode/rm/*.o, and
#      arch/x86/entry/vdso/vdso32/*.o (compile.go's own
#      isKbuildElfProbe/isKbuildRealmodeObj/isKbuildVDSO32Obj) — each
#      is read back synchronously by an unshimmed tool (mk_elfconfig,
#      nm, or the vdso32.so.dbg link itself) in the same recursive
#      make. mode.Realise's own synchronous-build carveout (the
#      mechanism conftests/cmake probes use) turned out to be
#      fundamentally incompatible with sandbox mode — confirmed
#      directly, "no substituter" from a nested, network-isolated
#      `nix build` that could never work inside a real sandbox. Fixed
#      with a plain Passthrough instead: none of these probes need
#      anything from nixgg's own graph, and Passthrough is mode-
#      agnostic and sandbox-safe by construction.
#   6. scripts/mod/modpost, vmlinux.o, and vmlinux itself (mode.go's
#      own isKbuildModpost/isKbuildVmlinuxO/isKbuildVmlinux, via
#      mode.ForLink) — each is a build-time artifact an unshimmed tool
#      execs or reads back IN THE SAME recursive make right after it's
#      produced: modpost is EXEC'D DIRECTLY by later Makefile rules
#      (no NIXGG_BYPASS pre-pass window, unlike fixdep/Kconfig's
#      `conf`); vmlinux.o is read by `objcopy -j .modinfo`
#      (scripts/Makefile.vmlinux_o); vmlinux itself is read by both
#      `nm` (System.map) and `sorttable`, both inside
#      scripts/link-vmlinux.sh, the same script that ran the link.
#      Fixed with mode.Realise's synchronous-build carveout, same
#      shape as realmode.elf/vdso32.so.dbg.
#   7. `--whole-archive`/`--no-whole-archive` scoping (expr's own
#      WholeArchiveInputs) — Kbuild's own vmlinux.o link
#      (cmd_ld_vmlinux.o) wraps ONLY vmlinux.a in --whole-archive,
#      alongside a SEPARATE --start-group/--end-group spanning the
#      libs. The pre-existing GroupInputs mechanism widens its own
#      single span to cover every input (safe for --start-group,
#      confirmed against ld), but --whole-archive changes archive
#      MEMBER SELECTION, not just resolution order — widening it the
#      same way silently forced extra objects in and produced a
#      vmlinux.o with no sections at all. Fixed by tracking the exact
#      named subset (parseLinkArgsWholeArchive) instead of a boolean.
#   8. Raw `ld`'s own group-bracket spelling (Derivation.isRawLinker)
#      — `-Wl,--start-group` is a COMPILER-DRIVER convention (pass
#      this through to your linker); a raw `ld` invocation (Kbuild's
#      own vmlinux.o link) rejects it outright ("unrecognized
#      option") and needs the bare form instead. Every GroupInputs
#      case before this fixture went through a compiler driver, so
#      the distinction was never reachable before.
#   9. realiseAndLink's write access (realise.PromoteToStoreSubdir) —
#      the realise-mode carveout above used to SYMLINK output at the
#      resolved store path, fine for every prior carveout (all
#      read-only from a downstream tool's perspective) but wrong for
#      vmlinux itself: scripts/link-vmlinux.sh's own `sorttable`
#      rewrites sorted-table sections IN PLACE, and a symlink into the
#      read-only Nix store fails "Permission denied". Fixed by
#      switching to the same real, writable copy mechanism
#      realise.Realise's own force-promotion already uses (and for
#      the identical underlying reason — see PromoteToStore's own
#      docstring on Nix's pinned mtimes and store-path permissions).
#  10. bare `--script=<path>` linker-script recognition
#      (linkerScriptPath) — scripts/link-vmlinux.sh sets `wl=""` for
#      every arch but um and emits `--script=${objtree}/vmlinux.lds`
#      with no `-Wl,`/`-Xlinker` wrapper at all, since $(LD) is
#      invoked directly. Every prior linker-script case (openssl,
#      QEMU) went through a compiler driver's own `-Wl,`-wrapped
#      spelling; without this case the script was never staged into
#      the link derivation's sandbox and ld failed "cannot open
#      linker script file ./arch/x86/kernel/vmlinux.lds".
#
# Plus a cross-cutting fix in Passthrough itself
# (RealiseThunkArgsAndPassthrough): whenever ar/ld gives up modeling a
# call because ONE sibling input is unmodelable, every OTHER sibling
# that's still one of nixgg's own not-yet-realized thunks is realized
# FIRST — otherwise a thin archive's own "store the path, don't read
# the content" semantics let the real tool succeed silently while
# baking a `.nixgg/thunks/<id>.nix` path into the result as if it were
# a real object.
#
# Two `NIXGG_BYPASS=1` pre-passes, same pattern postgresql/qemu/fmt's
# own configure steps already use: Kconfig's own `syncconfig` step
# probes the host assembler/compiler directly (same shape as an
# autoconf/cmake probe), and `make prepare` builds fixdep/Kconfig's
# own host tools + generates headers — both need to run for real,
# unshimmed, before the real (shimmed) per-TU build starts.
#
# `cmd_ar_vmlinux.a` is overridden on the command line to skip the
# real recipe's OWN second `ar mPiT <member> vmlinux.a <members>` step
# (a member-reordering pass for architectures with a `head-object-
# list.txt` entry — checked directly: x86 has ZERO entries in that
# file, so the reorder is a verified no-op here). That second `ar`
# call runs `ar t vmlinux.a` synchronously on an archive nixgg just
# registered as its own derivation — the same "no synchronous realize"
# wall genksyms/several other Kbuild steps hit, and (unlike modpost/
# vmlinux.o/vmlinux above) not fixable by adding another mode.ForLink
# carveout, since it's an ARCHIVE read (`ar t`), not a link. `T` is
# kept (thin archive — required for nixgg's own archive model) but `S`
# is dropped (suppress-symbol-table) so ar still builds vmlinux.a's
# real index itself, since the skipped second call was ALSO
# responsible for rebuilding it as a side effect of the reorder —
# confirmed directly: keeping `S` produced "archive has no index; run
# ranlib to add one" at the final `ld`.
#
# `KCFLAGS` overrides route around this environment's own GCC being
# newer than what linux-6.12 was written against — NOT a nixgg
# workaround: `-Wno-error=unterminated-string-initialization` silences
# a strict new warning in ACPI table code (irrelevant here — ACPI is
# off in tinyconfig — kept for robustness against config drift), and
# `-std=gnu11` avoids GCC 15's C23-by-default `bool`/`true`/`false`
# keyword conflict with the kernel's own pre-C23 `typedef _Bool bool`
# compat shim in include/linux/stddef.h.
{
  mkNixggBuild,
  stdenv,
  src,
  flex,
  bison,
  elfutils,
  pkg-config,
  bc,
  # batchGroups passthrough — see flake.nix's own linux-kernel entry
  # (if/when one adds batching for this fixture; not wired up yet).
  batchGroups ? [ ],
}:

let
  # Phase 1: everything through the top-level `built-in.a` (Kbuild's
  # own bare-dot descend target, `$(build-dir): prepare` in the top
  # Makefile — `build-dir := .`), plus the two `lib.a`s vmlinux.o's
  # own link needs and one extra `ar` call to fold built-in.a into
  # vmlinux.a. This is the expensive part: ~2800 real compiles,
  # hundreds of nested archives — every one of them a real nixgg drv,
  # none of it touching vmlinux.o/vmlinux/modpost's own synchronous-
  # read wall (see phase 2's own docstring for why that wall exists
  # and can't be crossed from inside ONE derivation's build script).
  #
  # `vmlinux.a` (not bare `built-in.a`) is the one target phase 2
  # actually needs by that name: its basename is globally unique
  # across the whole build (confirmed directly — grep the full build
  # log for "built-in.a" and every OTHER hit has a directory prefix,
  # unlike "built-in.a" itself, which recurs at dozens of nesting
  # levels and would basename-collide as a target — see
  # matchesTarget's own docstring for the fix this required). `ar
  # cDPrT vmlinux.a ./built-in.a` after `make .` reproduces exactly
  # what Kbuild's own cmd_ar_vmlinux.a does, minus its second `ar
  # mPiT` reorder call (see the buildCommand's own comment on that —
  # same no-op-on-x86 reasoning as the single-phase attempt this
  # fixture's history already validated).
  phase1 = mkNixggBuild {
    pname = "linux-kernel-phase1";
    version = "6.12";
    inherit src batchGroups;
    targets = [
      { name = "vmlinux-a"; path = "vmlinux.a"; }
      { name = "lib-a"; path = "lib/lib.a"; }
      { name = "arch-lib-a"; path = "arch/x86/lib/lib.a"; }
    ];
    nativeBuildInputs = [ flex bison pkg-config bc ];
    buildInputs = [ elfutils ];
    buildCommand = ''
      NIXGG_BYPASS=1 make tinyconfig
      NIXGG_BYPASS=1 make prepare
      make -j"$NIX_BUILD_CORES" \
        KCFLAGS="-Wno-error=unterminated-string-initialization -std=gnu11" \
        .
      ar cDPrT vmlinux.a ./built-in.a
    '';
  };

  # Phase 2: the final link, entirely OUTSIDE Kbuild's own recursive
  # make — no dependency-graph rebuild risk, because nothing here
  # asks `make` to produce vmlinux.a/lib.a again; phase1.result*
  # (real, Nix-resolved store paths by the time phase 2's build even
  # starts, since they're ordinary buildInputs) are mounted directly
  # and used as-is.
  #
  # WHY a phase split at all, and why it stops here: `nix build
  # .#linux-kernel` (single-phase, sandbox mode) is CONFIRMED to fail
  # — see the single-phase fixture history preserved in this file's
  # own git log / the session notes this came out of — at
  # vmlinux.o/vmlinux themselves, because Kbuild's own recipes read
  # each one back SYNCHRONOUSLY, in the SAME recursive make, moments
  # after producing it (objcopy on vmlinux.o for .modinfo; nm +
  # sorttable on vmlinux itself, both inside scripts/link-vmlinux.sh).
  # A single sandbox-mode derivation has no way to satisfy that: every
  # nixgg output is a REGISTERED drv, not real bytes, until Nix's OWN
  # outer scheduler resolves it — which only happens AFTER the
  # derivation's build script exits. No reordering of Kbuild's own
  # Makefiles changes that; the wall is about WHEN artifacts become
  # real relative to one script's lifetime, not where in that script
  # the read happens.
  #
  # A phase boundary is the one thing that actually crosses this: a
  # second derivation's `buildInputs` forces Nix to resolve phase 1's
  # declared targets to real bytes BEFORE phase 2's build script even
  # starts. Given that, phase 2 does NOT hand the problem back to
  # `make` (which has no idea phase 1 already built vmlinux.a/lib.a —
  # handing it a tree with those present but their own upstream
  # inputs, built-in.a's ~2800 objects, absent would make it try to
  # rebuild the world). Instead it replicates Kbuild's own last few
  # steps BY HAND, using the real tools directly: `ld` (the exact
  # flags scripts/link-vmlinux.sh's own cmd_link_vmlinux uses for this
  # config — no kallsyms, no BTF, no objtool, no arch postlink pass;
  # all four are off in tinyconfig, confirmed directly from
  # link-vmlinux.sh's own is_enabled() guards and this config's own
  # generated include/config/auto.conf), then `nm`+mksysmap for
  # System.map, then `sorttable` in place. Verified byte-for-byte
  # reproducible against this exact fixture's own real, complete
  # single-phase native-mode build (md5sum-identical vmlinux, modulo
  # BuildID/mtime noise) before being written up here.
  #
  # `NIXGG_BYPASS=1 make tinyconfig && make prepare` reruns the SAME
  # cheap pre-pass phase 1 already did (Kconfig sync, host-tool
  # bootstrap, header generation) to regenerate `scripts/sorttable`
  # (a genuine BYPRODUCT of `make prepare`, a hostprogs link never
  # routed through nixgg's own graph even in the single-phase build —
  # HOSTCC dispatch isn't one of the shim's recognized roles) fresh.
  # `arch/x86/kernel/vmlinux.lds` is NOT a byproduct of `prepare` —
  # confirmed directly, it's only reached during the FULL recursive
  # descend `make prepare` deliberately doesn't do — so buildCommand
  # names it as its own explicit single-target goal (Kbuild's own
  # single-target dispatch handles `%.lds` directly; see the
  # buildCommand's own comment on this). Rerunning `prepare` (rather
  # than inventing a second output-passing mechanism to ship
  # sorttable out of phase 1) costs nothing extra: it's Kconfig sync
  # + host-tool bootstrap, a tiny fraction of the full build's own
  # wall-clock.
  #
  # phase2 is a PLAIN stdenv.mkDerivation, not mkNixggBuild — a second,
  # newly-discovered instance of the exact wall the phase split exists
  # to cross. mkNixggBuild's own `targets` mechanism is "register a
  # dyn-drv output now, resolve it to real bytes later" — the real
  # bytes only exist after Nix's OUTER scheduler resolves the
  # derivation, i.e. after THIS build script has already exited. That
  # is fundamentally incompatible with `nm`/`sorttable` needing to read
  # `vmlinux` back synchronously, moments later, in the SAME script
  # that just linked it — confirmed directly: routing this same
  # ld/nm/sorttable sequence through mkNixggBuild submitted vmlinux
  # correctly as a registered drv, but the very next `nm -n vmlinux`
  # in the same script read the still-unresolved drvref-stub marker
  # ("file format not recognized" / "unrecognized ELF data encoding").
  # None of `ld`/`nm`/`sorttable` need any nixgg acceleration here —
  # each runs exactly once — so a plain derivation with no nixgg shims
  # on PATH at all is the correct fix, not a workaround.
  phase2 = stdenv.mkDerivation {
    pname = "linux-kernel";
    version = "6.12";
    inherit src;
    nativeBuildInputs = [ flex bison pkg-config bc ];
    buildInputs = [
      elfutils
      phase1.results.vmlinux-a
      phase1.results.lib-a
      phase1.results.arch-lib-a
    ];
    dontConfigure = true;
    buildPhase = ''
      runHook preBuild

      make tinyconfig
      make prepare
      # vmlinux.lds is NOT produced by `make prepare` — it's a plain
      # extra-y target only reached during the FULL recursive descend
      # (`make .`/`make vmlinux`, scripts/Makefile.build's own
      # targets-for-builtin), which phase 2 deliberately never runs
      # (that's phase 1's own job). Kbuild's single-target dispatch
      # (`%.lds` is in scripts/Makefile's own single-targets list)
      # builds it directly and correctly without pulling in anything
      # else — confirmed directly, byte-identical (11964 bytes) to
      # the copy produced by a full recursive build.
      make arch/x86/kernel/vmlinux.lds

      ln -s ${phase1.results.vmlinux-a}/lib/vmlinux.a vmlinux.a
      mkdir -p lib arch/x86/lib
      ln -s ${phase1.results.lib-a}/lib/lib.a lib/lib.a
      ln -s ${phase1.results.arch-lib-a}/lib/lib.a arch/x86/lib/lib.a

      ld -m elf_i386 -z noexecstack --no-warn-rwx-segments \
        --build-id=sha1 --orphan-handling=warn \
        -o vmlinux \
        -T arch/x86/kernel/vmlinux.lds \
        --whole-archive vmlinux.a --no-whole-archive \
        --start-group lib/lib.a arch/x86/lib/lib.a --end-group

      nm -n vmlinux | sed -f scripts/mksysmap > System.map
      scripts/sorttable vmlinux

      runHook postBuild
    '';
    installPhase = ''
      runHook preInstall
      mkdir -p "$out"
      cp -a vmlinux System.map "$out/"
      runHook postInstall
    '';
  };
in
phase2 // {
  # flake.nix's own `examples`/`exampleResults`/`exampleShells` wiring
  # expects every example to carry `.package`/`.shell` (every OTHER
  # fixture gets these for free from mkNixggBuild). phase2 is a plain
  # stdenv.mkDerivation, so it has neither — `package` is just itself
  # (already a real derivation, unlike mkNixggBuild's own dyn-drv
  # `result`, there's no separate copy-out-of-the-sandbox step
  # needed), and `shell` is itself too: `nix develop` on any plain
  # stdenv.mkDerivation already drops into its own build environment,
  # no separate mkShell wrapper required the way mkNixggBuild's own
  # sandboxed dyn-drv (which can't be an env-target at all) needs one.
  package = phase2;
  shell = phase2;
  # Expose phase1 too, for isolated smoke tests — the full
  # mkNixggBuild attrset, same as examples/llvm's own llvm-min-tblgen/
  # llvm-tblgen exposure.
  linux-kernel-phase1 = phase1;
}

