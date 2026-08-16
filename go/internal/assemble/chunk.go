package assemble

import (
	"path"
	"sort"
	"strings"

	"github.com/tbereknyei/nixgg/internal/expr"
)

// The assembly drv passes its whole builder script as a single
// `bash -c <script>` argument, one `cp -a` line per stub. That is fine
// until the stub count gets large, and then it stops being fine in a way
// no configuration can fix:
//
//	error: executing '…/bash': Argument list too long
//
// Linux caps a SINGLE argv entry at MAX_ARG_STRLEN = 32 pages = 131072
// bytes. That is a compile-time kernel constant, not a sysctl — unlike
// ARG_MAX (2 MiB) and unlike fs.mount-max, both of which can be raised.
// A full kernel build assembles 21,966 stubs into a ~5 MB script, so it
// exceeds the per-argument limit roughly 38x and can never exec.
//
// So past a threshold the assembly is split: each chunk becomes its own
// drv that materialises only its own stubs at their relative paths, and
// the final drv restores the captured tree and overlays the chunk
// outputs. Chunks are disjoint by relative path, so overlay order does
// not matter and they build in parallel.
//
// Below the threshold nothing changes — Build still emits one drv, byte
// for byte. That keeps every existing build (and the drv-equivalence
// fixtures) on exactly the path they were on; chunking is reachable only
// by builds that could not otherwise run at all.

// MaxScriptBytes is the per-chunk script budget.
//
// Deliberately well under MAX_ARG_STRLEN (131072): the estimate driving
// the packing is computed from the same strings the emitter uses, but
// the drv also carries a preamble, and being wrong here costs a build
// that fails at exec with no useful diagnostic. Half the limit buys a
// large margin for a handful of extra derivations.
const MaxScriptBytes = 64 << 10

// ChunkStubs partitions stubs so each chunk's script stays under
// budget. Order is preserved so the partition is deterministic — the
// resulting drv hashes must not depend on map iteration or timing.
func ChunkStubs(stubs []Stub, budget int) [][]Stub {
	var chunks [][]Stub
	var cur []Stub
	size := 0
	for _, s := range stubs {
		n := stubLineCost(s)
		// Always place at least one stub per chunk: a single line that
		// exceeds the budget on its own cannot be split any further, and
		// an empty chunk would loop forever.
		if len(cur) > 0 && size+n > budget {
			chunks = append(chunks, cur)
			cur, size = nil, 0
		}
		cur = append(cur, s)
		size += n
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// stubLineCost estimates the script bytes one stub contributes: its
// `cp -a` line plus a `mkdir -p` for its parent. Over-counting is safe
// (more, smaller chunks); under-counting is not, hence the mkdir is
// charged per stub even though duplicates are emitted once.
func stubLineCost(s Stub) int {
	// "cp -a " + placeholder + "/" + subdir + "/" + base + ` "$out/` + rel + "\"\n"
	base := path.Base(s.RelPath)
	return len("cp -a ") + len(expr.CAOutputPlaceholder(s.DrvPath, "out")) +
		len(expr.ArtifactSubdir(base)) + 2*len(base) + len(s.RelPath) +
		len(` "$out/`) + len("\"\n") +
		len(`mkdir -p "$out/`) + len(path.Dir(s.RelPath)) + len("\"\n")
}

// ChunkParams describes one chunk drv: materialise these stubs, and
// nothing else, at their relative paths under $out.
type ChunkParams struct {
	Name      string
	System    string
	Bash      string
	Coreutils string
	Stubs     []Stub
}

// BuildChunk emits a drv whose output holds only its own stubs'
// artifacts, each at its relative path.
//
// Unlike the single-drv path there is no captured tree to copy into, so
// parent directories have to be created explicitly.
func BuildChunk(p ChunkParams) expr.JSONDrv {
	drvs := map[string]expr.JSONDrvRef{}
	srcs := []string{expr.StoreBasename(p.Bash), expr.StoreBasename(p.Coreutils)}

	var script strings.Builder
	script.WriteString("set -euo pipefail\n")
	script.WriteString(`export PATH="` + p.Coreutils + `/bin"` + "\n")
	script.WriteString(`mkdir -p "$out"` + "\n")

	// One mkdir per distinct parent, sorted so the script is a pure
	// function of the stub list.
	dirs := map[string]bool{}
	for _, s := range p.Stubs {
		if d := path.Dir(s.RelPath); d != "." {
			dirs[d] = true
		}
	}
	sorted := make([]string, 0, len(dirs))
	for d := range dirs {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted)
	for _, d := range sorted {
		script.WriteString(`mkdir -p "$out/` + d + `"` + "\n")
	}

	for _, s := range p.Stubs {
		key := expr.StoreBasename(s.DrvPath)
		ref := drvs[key]
		ref.Outputs = []string{"out"}
		ref.DynamicOutputs = map[string]any{}
		drvs[key] = ref

		base := path.Base(s.RelPath)
		subdir := expr.ArtifactSubdir(base)
		src := expr.CAOutputPlaceholder(s.DrvPath, "out")
		if subdir != "" {
			src += "/" + subdir
		}
		src += "/" + base
		script.WriteString(`cp -a ` + shellQuote(src) + ` "$out/` + s.RelPath + `"` + "\n")
	}

	return expr.JSONDrv{
		Name:    p.Name,
		System:  p.System,
		Builder: p.Bash + "/bin/bash",
		Args:    []string{"-c", script.String()},
		Env: map[string]string{
			"out":            "/" + expr.OutPlaceholderNix32,
			"name":           p.Name,
			"system":         p.System,
			"builder":        p.Bash + "/bin/bash",
			"outputHashAlgo": "sha256",
			"outputHashMode": "nar",
		},
		Inputs: expr.JSONDrvInputs{
			Drvs: drvs,
			Srcs: srcs,
		},
		Outputs: map[string]expr.JSONOut{
			"out": {Method: "nar", HashAlgo: "sha256"},
		},
		Version: 4,
	}
}

// BuildOverlay emits the final drv for the chunked path: restore the
// captured tree, then overlay each chunk output over it.
//
// `cp -a <chunk>/. "$out/"` merges rather than replaces, and chunks are
// disjoint by relative path, so the result matches what the single-drv
// script produces file for file.
func BuildOverlay(p BuildParams, chunkDrvPaths []string) expr.JSONDrv {
	drvs := map[string]expr.JSONDrvRef{}
	srcs := []string{expr.StoreBasename(p.Bash), expr.StoreBasename(p.Coreutils), p.TreeSrc}

	var script strings.Builder
	script.WriteString("set -euo pipefail\n")
	script.WriteString(`export PATH="` + p.Coreutils + `/bin"` + "\n")
	script.WriteString(`mkdir -p "$out"` + "\n")
	script.WriteString(`cp -a "/nix/store/` + p.TreeSrc + `/." "$out/"` + "\n")
	script.WriteString(`chmod -R u+w "$out"` + "\n")

	for _, dp := range chunkDrvPaths {
		key := expr.StoreBasename(dp)
		ref := drvs[key]
		ref.Outputs = []string{"out"}
		ref.DynamicOutputs = map[string]any{}
		drvs[key] = ref
		script.WriteString(`cp -a ` + shellQuote(expr.CAOutputPlaceholder(dp, "out")+"/.") + ` "$out/"` + "\n")
	}

	return expr.JSONDrv{
		Name:    p.Name,
		System:  p.System,
		Builder: p.Bash + "/bin/bash",
		Args:    []string{"-c", script.String()},
		Env: map[string]string{
			"out":            "/" + expr.OutPlaceholderNix32,
			"name":           p.Name,
			"system":         p.System,
			"builder":        p.Bash + "/bin/bash",
			"outputHashAlgo": "sha256",
			"outputHashMode": "nar",
		},
		Inputs: expr.JSONDrvInputs{
			Drvs: drvs,
			Srcs: srcs,
		},
		Outputs: map[string]expr.JSONOut{
			"out": {Method: "nar", HashAlgo: "sha256"},
		},
		Version: 4,
	}
}

// ScriptFits reports whether the single-drv script for these stubs stays
// within budget — i.e. whether chunking is needed at all.
func ScriptFits(stubs []Stub, budget int) bool {
	total := 0
	for _, s := range stubs {
		total += stubLineCost(s)
		if total > budget {
			return false
		}
	}
	return true
}
