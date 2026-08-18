// Package expr constructs Nix derivations. A single Derivation struct
// holds every field that both wire formats — the `.nix` thunk file
// (native mode, run through `nix-instantiate`) and the JSON drv
// description (sandbox mode, run through `nix derivation add`) — need
// to agree on. Both serializers work off the same struct, so if you
// add a new attribute you can't accidentally set it in one format and
// forget the other.
//
// The invariant we care about: same Derivation → same drv hash,
// regardless of which serializer produces the wire bytes. Enforced
// externally by tests/drv-equivalence.sh (runs both paths, compares
// resulting drv-store-paths).
package expr

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind identifies which of the three build steps this derivation
// represents. Different Kinds emit different shell scripts and
// slightly different helper Nix files, but share the same env-dict
// shape.
type Kind int

const (
	KindCompile Kind = iota // per-TU compile: builder.nix
	KindLink                // linker step: linker.nix
	KindArchive             // ar step: archiver.nix
	// KindTransform: rewrite one existing object in place.
	//
	// Sandbox mode only — there is no native-mode helper for it, since
	// the tools that need it (objtool, and `ld -r` for partial links)
	// only appear in builds that already require the sandbox.
	//
	// Unlike the other three this produces no new artifact: it consumes
	// one object and emits the same object, modified. Keeping it a
	// separate derivation rather than folding it into the compile is
	// deliberate — the compile stays cacheable independently of the
	// transform's flags, so changing e.g. objtool's arguments rebuilds
	// only this thin layer.
	KindTransform
	// KindPartialLink: `ld -r` — combine several objects into one
	// object (not an executable, not an archive).
	//
	// Sandbox mode only, same as KindTransform. Reached when a build:
	// `cmd_ld_multi_m` fuses a multi-object module's parts, and
	// `cmd_ld_single` runs it over a single-object module on its own.
	KindPartialLink
)

// Derivation is the intermediate representation both serializers
// consume. Fields mirror the union of what builder.nix / linker.nix /
// archiver.nix accept plus the fields Nix itself requires (name,
// system, builder, args, env, inputs, outputs).
//
// Not every field applies to every Kind:
//   - Compile uses SrcStore, Source, OutName, Flags.
//   - Link uses Inputs, Flags.
//   - Archive uses Inputs, ARFlags.
//
// StoreDeps and WrapperEnv apply to all Kinds.
type Derivation struct {
	Kind Kind

	// The name Nix assigns to the derivation's .drv basename. For
	// compile this is "tu-<outName>"; link "bin-<outName>"; archive
	// "ar-<outName>".
	Name string

	// Nix system (e.g. "x86_64-linux").
	System string

	// Toolchain roots — full /nix/store/… paths. All three kinds use
	// bash (builder) + coreutils (PATH). Compile + link use the
	// compiler; archive uses the binutils dir (via AR).
	Bash, Coreutils string
	Compiler        string // gcc-wrapper root; unused by Archive
	AR              string // binutils root (parent of bin/ar); Archive only

	// Compile-specific.
	Tool     string // "cc", "gcc", "c++", "g++"; used by Compile + Link
	SrcStore string // /nix/store/…-<tuID>: the staged src tree
	Source   string // relative to SrcStore, e.g. "src/foo.c"
	OutName  string // e.g. "foo.o"

	// Link + Archive: inputs (either sibling drvs or already-realised
	// store paths). Compile leaves this empty.
	Inputs []derivInput

	// Link + Compile: compiler flags. Archive uses ARFlags instead.
	Flags []string

	// GroupInputs wraps the input list in --start-group/--end-group.
	//
	// Set when the caller's link line had those brackets. They cannot be
	// carried in Flags: they are positional — they bracket whatever sits
	// BETWEEN them — and buildScript emits all flags before all inputs,
	// which would leave the pair adjacent, spanning nothing, silently
	// defeating ld's multi-pass rescan for circular archive deps.
	//
	// The re-emitted group spans every input rather than the caller's
	// exact original span. Widening is safe (objects inside a group are
	// harmless, verified against ld) and the narrow span is not
	// expressible once inputs and flags have been separated.
	GroupInputs bool

	// Archive-only: `ar` modifier string (e.g. "rcs").
	ARFlags string

	// Link-only: a store path (or Nix path literal, in native mode)
	// holding local, non-store, non-stub files the link line
	// references by relative path — a linker script generated moments
	// earlier by an unshimmed tool (e.g. openssl's `perl
	// util/mkdef.pl > libcrypto.ld`, referenced via
	// `-Wl,--version-script=libcrypto.ld`). The link shim reads the
	// content at classify time (before anything downstream could turn
	// it into a stub), stages it via stage.ContentFiles, and passes
	// the resulting directory here — same shape as Compile's SrcStore,
	// just for KindLink. A real derivation input (env["src"], copied
	// in before the link command runs), NOT text embedded in the
	// script: a large generated linker script plus hundreds of real
	// object-file paths on one link line can exceed the kernel's argv
	// limit if baked into the script body directly (confirmed
	// directly against openssl's libcrypto.so.3 — "Argument list too
	// long").
	InlineFilesStore string

	// Transform-only: absolute /nix/store/… path of the binary that
	// rewrites the object. Not taken from PATH like the compiler and
	// `ar`, because this tool is built by the wrapped project itself
	// (the build compiles it in its own prepare step) and reaches the
	// store only because the shim adds it there — see
	// shim.storeAddTool.
	ToolBin string

	// /nix/store/… roots referenced by Flags or WrapperEnv content;
	// must be mounted in the sandbox. Serialized as _storeDeps env var
	// (colon-joined) and threaded into inputs.srcs (JSON mode) /
	// storeDepsJSON (native mode).
	StoreDeps []string

	// Nix's gcc-wrapper env (NIX_CFLAGS_COMPILE, NIX_LDFLAGS, etc.).
	// Preserved verbatim in the env dict.
	WrapperEnv map[string]string
}

// derivInput is the internal per-input record used by Derivation's
// serializers. Kept separate from the shim-facing Input type in
// expr.go (which uses Kind="store"/"nix" via a different field name
// for historical reasons). inputsFromExpr / inputsFromJSON convert.
type derivInput struct {
	// InputKind = "store" (already realised, use the store path as
	// builtins.storePath) or "nix" (a sibling drv/thunk we
	// reference — becomes `import /path/thunk.nix` in native mode
	// and inputs.drvs entry in JSON mode).
	InputKind string
	// Ref: for "store", a canonical /nix/store/<hash>-<name> root;
	// for "nix", the absolute path to the sibling drv/thunk.
	Ref string
	// Name: the path of the artifact relative to that ref's output dir.
	// Usually a basename ("foo.o"), but carries a subdirectory when the
	// producing derivation puts its artifact under one — see
	// inputSubdirFor.
	Name string
}

// inputSubdirFor returns the FHS subdirectory a sibling derivation's
// artifact sits in, inferred from the artifact's own filename.
//
// Producer and consumer are separate derivations built at different
// times, so the agreement between them cannot be passed along — it has
// to be derivable identically on both sides. The filename is the only
// thing both sides reliably share:
//
//	*.a          -> lib   (an ar drv wrote it to $out/lib/)
//	*.o          -> ""    (compile outputs stay flat)
//	anything else-> bin   (a link drv wrote it to $out/bin/)
//
// Keying on the filename rather than on the producing drv's NAME is
// deliberate and load-bearing. In sandbox mode a sibling reference is a
// drv path ("…-ar-libfoo.a.drv") whose kind is legible; in native mode it
// is a thunk path (".nixgg/thunks/<hash>.nix") that carries no kind at
// all. Inferring from the drv name would therefore resolve to "lib" in
// sandbox mode and "" in native mode for the same input — the two would
// emit different scripts and produce different drv hashes, breaking the
// one invariant this project rests on.
func inputSubdirFor(name string) string { return ArtifactSubdir(name) }

// StoreBasename returns the text after the last '/' in p — the
// hash+name basename `nix derivation add` wants for an inputs.srcs
// entry, or a drv's own store-path basename.
func StoreBasename(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ArtifactSubdir is inputSubdirFor, exported for the one caller outside
// this package that has to agree with it: native mode's PromoteToStore
// copies the realised artifact out of the store into the working tree, so
// it must look where the emitted script actually wrote it.
//
// That call site is not covered by tests/drv-equivalence.sh, which
// compares drv HASHES and never reads a realised output. Missing it left
// `make` in native mode failing with "expected …/bin-hello/hello to
// exist" after outputs moved under bin/ — the drvs were correct and
// identical across modes, but nothing fetched the result.
func ArtifactSubdir(name string) string {
	base := StoreBasename(name)
	switch {
	case strings.HasSuffix(base, ".o"):
		return ""
	case strings.HasSuffix(base, ".a"):
		return "lib"
	}
	return "bin"
}

// outputPlaceholder returns the `builtins.placeholder "out"` value —
// what every derivation's `$out` env var interpolates to at build
// time. Same digest for every CA derivation with a single `out`
// output; caching for one call site is fine.
func outputPlaceholder() string { return "/" + OutPlaceholderNix32 }

// Native mode's script carries markers where only Nix knows the value at
// eval time: an input may be an unrealised sibling thunk whose drv hash —
// hence CA output placeholder — doesn't exist yet, and the toolchain roots
// are the helper's own arguments. nix/resolve-script.nix substitutes them;
// store context survives replaceStrings, so dependency edges stay intact.
//
// The tag is per-script, not fixed. Markers are just text, so a flag
// spelling one (`-DAT=@NIXGG_COMPILER@`) would be substituted too.
const markerTagBase = "NIXGG"

// markerTag returns a tag whose markers cannot collide with anything
// already in `body`. Deterministic: same body → same tag, which matters
// because the tag ends up in the thunk text and thus in its hash.
func markerTag(body string) string {
	for n := 0; ; n++ {
		tag := markerTagBase
		if n > 0 {
			tag = markerTagBase + strconv.Itoa(n)
		}
		if !strings.Contains(body, "@"+tag+"_") {
			return tag
		}
	}
}

// Marker spellings. The helper reconstructs these from the tag it is
// passed, so any change here needs the matching change in
// nix/resolve-script.nix — TestNativeTemplateResolvesToSandboxScript runs
// the real helper and fails if they disagree.
func coreutilsMarker(tag string) string { return "@" + tag + "_COREUTILS@" }
func compilerMarker(tag string) string  { return "@" + tag + "_COMPILER@" }
func inputMarker(tag string, i int) string {
	return "@" + tag + "_INPUT" + strconv.Itoa(i) + "@"
}

// script returns the bash `-c` body with every store path resolved.
// This is what sandbox mode bakes into its JSON drv.
func (d *Derivation) script() string {
	return d.buildScript("", d.Coreutils, d.compilerOrAR())
}

// scriptTemplate returns the same script with markers where native mode
// cannot know the value yet, plus the tag those markers use. The thunk
// passes both to nix/resolve-script.nix.
func (d *Derivation) scriptTemplate() (template, tag string) {
	// Choose the tag against the resolved body: it contains the same flag
	// text as the template, and unlike the template it has no markers of
	// its own to confuse the scan.
	tag = markerTag(d.script())
	return d.buildScript(tag, coreutilsMarker(tag), compilerMarker(tag)), tag
}

// outSubdir is the FHS directory inside $out that this Kind's artifact
// belongs in, "" for none.
//
// Nix store paths follow the FHS internally: an executable lives at
// $out/bin/<name>, a library at $out/lib/<name>. That is not decoration
// — it is what makes a store path installable. `nix profile install`,
// `nix run`, buildEnv/symlinkJoin and NixOS's environment.systemPackages
// all locate artifacts by scanning bin/, lib/, share/ and friends. A
// binary sitting flat at $out/<name> is invisible to every one of them,
// so a nixgg-built program could not be installed the way any other Nix
// package can.
//
// Compile outputs stay flat, deliberately. A per-TU output holding one
// .o is not a package and has no FHS home; relocating it would rewrite
// every sibling reference and every drv hash in the project to move
// files nothing user-facing ever reads. FHS matters exactly where an
// output is consumed by a person or by Nix's own tooling.
func (d *Derivation) outSubdir() string {
	switch d.Kind {
	case KindLink:
		return "bin"
	case KindArchive:
		return "lib"
	}
	return ""
}

// The producer side (outSubdir, keyed on Kind) and the consumer side
// (inputSubdirFor, keyed on the artifact filename) must agree for every
// artifact nixgg produces, or a link would look for an archive somewhere
// the archive drv never wrote it. They are separate functions because
// each side only has one of those two pieces of information.
// TestOutSubdirAgreesWithInputSubdirFor pins the agreement.

// outPath renders the artifact's destination inside the builder, e.g.
// `$out/bin/llc`. Callers embed this directly in the shell script.
func (d *Derivation) outPath() string {
	if sub := d.outSubdir(); sub != "" {
		return "$out/" + sub + "/" + d.OutName
	}
	return "$out/" + d.OutName
}

// outDir renders the directory the builder must create first.
func (d *Derivation) outDir() string {
	if sub := d.outSubdir(); sub != "" {
		return "$out/" + sub
	}
	return "$out"
}

// inlineFilesScript renders shell that copies every file out of
// $src (the staged directory referenced by InlineFilesStore, set via
// the same "src" env var / derivation attribute Compile already uses
// for its own SrcStore — see envDict/ToNix) into the build root,
// before the link command runs. A real `cp`, not text embedded in
// the script: see InlineFilesStore's own docstring for why (kernel
// argv limit).
func (d *Derivation) inlineFilesScript() string {
	if d.InlineFilesStore == "" {
		return ""
	}
	return "cp -a \"$src/.\" .\n"
}

// compilerOrAR reports which store path provides the tools on PATH.
// Compile and Link use the compiler; Archive uses whatever supplies
// `ar` — which native mode passes as compilerRoot and sandbox mode as
// AR. Two names, one meaning.
func (d *Derivation) compilerOrAR() string {
	if d.Kind == KindArchive {
		return d.AR
	}
	return d.Compiler
}

// buildScript is the single source of the shell body — the layout,
// quoting, and argv order that both wire formats must agree on.
//
// tag == "" means resolve everything (sandbox mode). A non-empty tag
// means emit input markers with that tag (native mode); coreutils and
// compiler are then marker text too.
func (d *Derivation) buildScript(tag, coreutils, compiler string) string {
	pathPrefix := fmt.Sprintf(`export PATH="%s/bin:%s/bin"`, coreutils, compiler)

	inputs := func() string {
		parts := make([]string, 0, len(d.Inputs))
		for i, in := range d.Inputs {
			if tag != "" {
				parts = append(parts, "'"+inputMarker(tag, i)+"'")
				continue
			}
			switch in.InputKind {
			case "store":
				ref := in.Ref
				if !strings.HasPrefix(ref, "/nix/store/") {
					ref = "/nix/store/" + ref
				}
				parts = append(parts, fmt.Sprintf("'%s/%s'", ref, in.Name))
			case "nix":
				// A sibling drv we reference. Its artifact sits under the
				// FHS subdir its own Kind implies, so reach it there.
				name := in.Name
				if sub := inputSubdirFor(in.Name); sub != "" {
					name = sub + "/" + name
				}
				parts = append(parts,
					fmt.Sprintf("'%s/%s'", caOutputPlaceholder(in.Ref, "out"), name))
			}
		}
		return strings.Join(parts, " ")
	}

	switch d.Kind {
	case KindCompile:
		return fmt.Sprintf(
			`set -euo pipefail
%s
mkdir -p "$out"
cd "$src"
"%s" %s -c "$source" -o "$out/$outName"
`, pathPrefix, d.Tool, shellQuoteFlags(d.Flags))
	case KindLink:
		// Split flags into non-`-l` and `-l<name>` — the classic
		// single-pass ld resolves libraries against object files
		// mentioned BEFORE them. Emit inputs (objects/archives)
		// between the two groups so `-lm`/`-lc` come last, after
		// every object/archive that might reference libm/libc.
		// When there are no `-l` flags, fall through to the plain
		// flags-then-inputs layout so drv content stays byte-
		// identical to the pre-`-l`-split era (no trailing empty
		// slot). This keeps the equivalence set for hello/lua/mosh
		// stable — those never had `-l` flags to begin with.
		var lflags, nonLflags []string
		for _, f := range d.Flags {
			if strings.HasPrefix(f, "-l") && len(f) > 2 {
				lflags = append(lflags, f)
			} else {
				nonLflags = append(nonLflags, f)
			}
		}
		// Re-emit the archive group around the whole input list. Only
		// reached when the caller asked for it, so the no-group layout
		// below stays byte-identical for every existing derivation.
		inputList := inputs()
		if d.GroupInputs && inputList != "" {
			inputList = "-Wl,--start-group " + inputList + " -Wl,--end-group"
		}
		if len(lflags) == 0 {
			return fmt.Sprintf(
				`set -euo pipefail
%s
mkdir -p "%s"
%s"%s" %s %s -o "%s"
`, pathPrefix, d.outDir(), d.inlineFilesScript(), d.Tool, shellQuoteFlags(d.Flags), inputList, d.outPath())
		}
		return fmt.Sprintf(
			`set -euo pipefail
%s
mkdir -p "%s"
%s"%s" %s %s %s -o "%s"
`, pathPrefix, d.outDir(), d.inlineFilesScript(), d.Tool, shellQuoteFlags(nonLflags), inputList, shellQuoteFlags(lflags), d.outPath())
	case KindArchive:
		// `ar` is taken from PATH (set above) and `D` is prepended to
		// arFlags for a deterministic archive.
		return fmt.Sprintf(
			`set -euo pipefail
%s
mkdir -p "%s"
ar D%s "%s" %s
`, pathPrefix, d.outDir(), d.ARFlags, d.outPath(), inputs())
	case KindPartialLink:
		// `-r` is supplied by the caller's own flag list, not added
		// here: it is what identified this invocation as a partial link
		// in the first place, so re-adding it would be a second source
		// of truth.
		return fmt.Sprintf(
			`set -euo pipefail
export PATH="%s/bin"
mkdir -p "%s"
"%s" %s -o "%s" %s
`, coreutils, d.outDir(), d.ToolBin, shellQuoteFlags(d.Flags), d.outPath(), inputs())
	case KindTransform:
		// Copy first, then rewrite the copy: these tools edit in place
		// and the input is a read-only store path. chmod because store
		// paths arrive without write permission.
		//
		// PATH carries coreutils only — no compiler is involved, and the
		// transform binary is invoked by absolute path.
		return fmt.Sprintf(
			`set -euo pipefail
export PATH="%s/bin"
mkdir -p "%s"
cp %s "%s"
chmod u+w "%s"
"%s" %s "%s"
`, coreutils, d.outDir(), inputs(), d.outPath(), d.outPath(),
			d.ToolBin, shellQuoteFlags(d.Flags), d.outPath())
	}
	return ""
}

// ToNix serialises this Derivation as an `import <helper>.nix { … }`
// expression suitable for writing to .nixgg/thunks/<id>.nix. The
// helper (builder.nix / linker.nix / archiver.nix, all in nix/) is
// still what actually calls `derivation`; this emitter just fills
// in its arguments from our shared struct — so if we add a field
// to Derivation, both this native path and toJSON pick it up.
//
// helpers must be the /nix/store/…-nixgg-nix root.
//
// The bytes this produces are byte-equivalent to what the pre-
// Derivation-struct emitters produced (Compile/Link/Archive in
// expr.go). Verified externally by tests/drv-equivalence.sh.
func (d *Derivation) ToNix(helpers string) string {
	tmpl, tag := d.scriptTemplate()
	var b strings.Builder
	switch d.Kind {
	case KindCompile:
		fmt.Fprintf(&b, "import %s/builder.nix {\n", helpers)
		fmt.Fprintf(&b, "  srcTree        = %s;\n", d.SrcStore) // Nix path literal, unquoted
		fmt.Fprintf(&b, "  source         = %q;\n", d.Source)
		fmt.Fprintf(&b, "  outName        = %q;\n", d.OutName)
		fmt.Fprintf(&b, "  scriptTemplate = %s;\n", nixIndentedStringLiteral(tmpl))
		fmt.Fprintf(&b, "  markerTag      = %q;\n", tag)
		fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(d.StoreDeps))
		fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(d.WrapperEnv))
	case KindLink:
		fmt.Fprintf(&b, "import %s/linker.nix {\n", helpers)
		fmt.Fprintf(&b, "  outName        = %q;\n", d.OutName)
		if d.Name != "" {
			fmt.Fprintf(&b, "  name           = %q;\n", d.Name)
		}
		fmt.Fprintf(&b, "  inputs         = %s;\n", derivInputsList(d.Inputs))
		fmt.Fprintf(&b, "  scriptTemplate = %s;\n", nixIndentedStringLiteral(tmpl))
		fmt.Fprintf(&b, "  markerTag      = %q;\n", tag)
		fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(d.StoreDeps))
		fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(d.WrapperEnv))
		if d.InlineFilesStore != "" {
			fmt.Fprintf(&b, "  srcTree        = %s;\n", d.InlineFilesStore) // Nix path literal, unquoted
		}
	case KindArchive:
		fmt.Fprintf(&b, "import %s/archiver.nix {\n", helpers)
		fmt.Fprintf(&b, "  outName        = %q;\n", d.OutName)
		if d.Name != "" {
			fmt.Fprintf(&b, "  name           = %q;\n", d.Name)
		}
		fmt.Fprintf(&b, "  inputs         = %s;\n", derivInputsList(d.Inputs))
		fmt.Fprintf(&b, "  scriptTemplate = %s;\n", nixIndentedStringLiteral(tmpl))
		fmt.Fprintf(&b, "  markerTag      = %q;\n", tag)
		fmt.Fprintf(&b, "  storeDepsJSON  = ''%s'';\n", jsonArrayIndented(d.StoreDeps))
		fmt.Fprintf(&b, "  wrapperEnvJSON = ''%s'';\n", jsonObjectSorted(d.WrapperEnv))
	}
	b.WriteString("}\n")
	return b.String()
}

// derivInputsList renders `inputs = [ ... ]` for linker/archiver
// helpers, so they can interpolate each input into the script template.
func derivInputsList(inputs []derivInput) string {
	if len(inputs) == 0 {
		return "[ ]"
	}
	var b strings.Builder
	b.WriteString("[ ")
	for _, in := range inputs {
		var drv string
		switch in.InputKind {
		case "store":
			ref := in.Ref
			if strings.HasPrefix(ref, "/nix/store/") {
				drv = fmt.Sprintf("builtins.storePath %q", ref)
			} else {
				drv = fmt.Sprintf("builtins.storePath \"/nix/store/%s\"", ref)
			}
		case "nix":
			drv = "import " + in.Ref
		}
		// Same FHS reach-in as the JSON path: resolve-script.nix renders
		// "${i.drv}/${i.name}", so name must carry the producer's subdir
		// or native mode would look for an archive at the output root
		// while the archive drv wrote it to lib/. One rule, both formats.
		name := in.Name
		if in.InputKind == "nix" {
			if sub := inputSubdirFor(in.Name); sub != "" {
				name = sub + "/" + name
			}
		}
		fmt.Fprintf(&b, "{ drv = %s; name = %q; } ", drv, name)
	}
	b.WriteString("]")
	return b.String()
}

// jsonObjectSorted renders a map as a sorted-key JSON object,
// byte-deterministic. Used for wrapperEnvJSON.
func jsonObjectSorted(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kj, _ := jsonMarshal(k)
		vj, _ := jsonMarshal(m[k])
		b.Write(kj)
		b.WriteByte(':')
		b.Write(vj)
	}
	b.WriteByte('}')
	return b.String()
}

// jsonMarshal is a thin wrapper for readability.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// toJSON serialises this Derivation for `nix derivation add`. The
// caller passes extraSrcs (basenames of already-realised store paths
// the sandbox must mount) and optional wrapper-env overrides via
// WrapperEnv on the struct. Inputs are split into inputs.drvs (for
// Kind=="nix") and inputs.srcs (for Kind=="store").
func (d *Derivation) toJSON(extraSrcs []string, _reserved any) JSONDrv {
	drvs := map[string]JSONDrvRef{}
	srcs := append([]string{}, extraSrcs...)
	seenSrc := map[string]bool{}
	for _, s := range srcs {
		seenSrc[s] = true
	}
	for _, in := range d.Inputs {
		switch in.InputKind {
		case "nix":
			refKey := StoreBasename(in.Ref)
			ref := drvs[refKey]
			ref.Outputs = appendUnique(ref.Outputs, "out")
			if ref.DynamicOutputs == nil {
				ref.DynamicOutputs = map[string]any{}
			}
			drvs[refKey] = ref
		case "store":
			base := StoreBasename(in.Ref)
			if !seenSrc[base] {
				srcs = append(srcs, base)
				seenSrc[base] = true
			}
		}
	}
	for _, sd := range d.StoreDeps {
		base := StoreBasename(sd)
		if !seenSrc[base] {
			srcs = append(srcs, base)
			seenSrc[base] = true
		}
	}
	return JSONDrv{
		Name:    d.Name,
		System:  d.System,
		Builder: d.Bash + "/bin/bash",
		Args:    []string{"-c", d.script()},
		Env:     d.envDict(),
		Inputs: JSONDrvInputs{
			Drvs: drvs,
			Srcs: srcs,
		},
		Outputs: map[string]JSONOut{
			"out": {Method: "nar", HashAlgo: "sha256"},
		},
		Version: 4,
	}
}

// envDict is the env-var dict every derivation gets. Both serializers
// use it as-is — that's what pins them to the same hash.
func (d *Derivation) envDict() map[string]string {
	env := map[string]string{
		"out":            outputPlaceholder(),
		"name":           d.Name,
		"system":         d.System,
		"builder":        d.Bash + "/bin/bash",
		"outputHashAlgo": "sha256",
		"outputHashMode": "nar",
		"_storeDeps":     strings.Join(d.StoreDeps, ":"),
	}
	// Compile derivations pass source/outName/src via env vars too
	// (that's what builder.nix's script consults — see the
	// `cd "$src"` / `"$source"` / `"$outName"` in script()). Link
	// derivations reuse the same "src" slot for InlineFilesStore, when
	// set — see inlineFilesScript's own docstring.
	switch {
	case d.Kind == KindCompile:
		env["src"] = d.SrcStore
		env["source"] = d.Source
		env["outName"] = d.OutName
	case d.Kind == KindLink && d.InlineFilesStore != "":
		env["src"] = d.InlineFilesStore
	}
	for k, v := range d.WrapperEnv {
		env[k] = v
	}
	return env
}

// nixIndentedStringLiteral renders `s` as a Nix indented-string literal.
//
// Two constructs inside that form are not literal, and both are reachable
// from user flags: a doubled apostrophe closes the string, and `${` opens
// an interpolation. Unescaped, they give a thunk that won't parse or that
// interpolates at eval time.
//
// Order matters: the apostrophe rule runs first, or it would also rewrite
// the apostrophes introduced when escaping `${`.
func nixIndentedStringLiteral(s string) string {
	e := strings.ReplaceAll(s, "''", "'''")
	e = strings.ReplaceAll(e, "${", "''${")
	return "''" + e + "''"
}
