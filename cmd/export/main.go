// Command export packages a finished layout run into the static assets
// the web galaxy viewer loads: positions, per-node attributes, label
// chunks, a search table, and neighbor lists for the top hubs. All
// binary files are little-endian and indexed by "kept index" — the node
// order of layout's positions.bin.
//
// Output (in -web):
//
//	galaxy.json      manifest: counts, chunking, file inventory
//	positions.bin    copied from the layout run (2 × float32 per node)
//	attrs.bin        n × uint32 full-graph in-degree, then n × uint8
//	                 first-seen year offset from 2019 (255 = unknown)
//	labels/<c>.json  node paths in kept order, -label-chunk per file
//	search.json      [path, keptIdx, inDegree] for the top -search-top
//	nbr-index.bin    kept indices that have a neighbor file (uint32)
//	nbr/<idx>.bin    uint32 nIn, nOut, then dependents + dependencies
//	                 as kept indices — powers click-to-highlight
//
// First-seen years come from the index feed timestamps and are cached
// in data/graph/analysis/first-seen.tsv (the 51M-line scan takes about
// a minute; the cache makes re-export instant).
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

func main() {
	var (
		layoutDir  = flag.String("layout", "data/galaxy", "layout output directory")
		graphDir   = flag.String("graph", "data/graph", "buildgraph output directory")
		indexPath  = flag.String("index", "data/index.jsonl", "module index feed (for first-seen years)")
		webDir     = flag.String("web", "web/public/data", "viewer asset output directory")
		searchTop  = flag.Int("search-top", 50000, "modules in the search table")
		nbrTop     = flag.Int("nbr-top", 50000, "top modules by in-degree that get a neighbor file (superset of the search table, so every search hit can highlight)")
		nbrOutTop  = flag.Int("nbr-out-top", 10000, "top modules by out-degree that also get a neighbor file")
		labelChunk = flag.Int("label-chunk", 4096, "labels per chunk file")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	start := time.Now()
	// The labels/ and nbr/ trees are keyed by kept index, which changes
	// between layout runs — stale files from a previous export would be
	// silently wrong, so both trees start from scratch.
	for _, d := range []string{filepath.Join(*webDir, "labels"), filepath.Join(*webDir, "nbr")} {
		if err := os.RemoveAll(d); err != nil {
			log.Fatal(err)
		}
	}
	for _, d := range []string{*webDir, filepath.Join(*webDir, "labels"), filepath.Join(*webDir, "nbr")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}

	kept := loadKept(filepath.Join(*layoutDir, "kept.tsv"))
	n := len(kept)
	nOrig, paths := loadKeptPaths(filepath.Join(*graphDir, "nodes.tsv"), kept)
	origToKept := make([]int32, nOrig)
	for i := range origToKept {
		origToKept[i] = -1
	}
	for k, orig := range kept {
		origToKept[orig] = int32(k)
	}
	from, to := loadEdges(filepath.Join(*graphDir, "edges.tsv"))
	log.Printf("loaded %d kept of %d nodes, %d edges in %s", n, nOrig, len(from), time.Since(start).Round(time.Millisecond))

	// Full-graph in-degree: the number shown next to a module must be
	// its real dependent count, not the count inside the kept subgraph.
	inDeg := make([]uint32, n)
	outDeg := make([]uint32, n)
	keptEdges := 0
	for i := range from {
		u, v := origToKept[from[i]], origToKept[to[i]]
		if v >= 0 {
			inDeg[v]++
		}
		if u >= 0 {
			outDeg[u]++
		}
		if u >= 0 && v >= 0 {
			keptEdges++
		}
	}

	years := firstSeenYears(*indexPath, filepath.Join(*graphDir, "analysis", "first-seen.tsv"), paths)

	writeAttrs(filepath.Join(*webDir, "attrs.bin"), inDeg, years)
	copyFile(filepath.Join(*layoutDir, "positions.bin"), filepath.Join(*webDir, "positions.bin"), 8*n)
	writeLabels(filepath.Join(*webDir, "labels"), paths, *labelChunk)
	writeSearch(filepath.Join(*webDir, "search.json"), paths, inDeg, *searchTop)
	hubs := writeNeighbors(*webDir, origToKept, from, to, inDeg, outDeg, *nbrTop, *nbrOutTop)

	var maxIn uint32
	for _, d := range inDeg {
		maxIn = max(maxIn, d)
	}
	manifest := map[string]any{
		"nodes":      n,
		"edges":      keptEdges,
		"maxInDeg":   maxIn,
		"labelChunk": *labelChunk,
		"searchTop":  *searchTop,
		"nbrTop":     len(hubs),
		"yearBase":   2019,
		"generated":  time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*webDir, "galaxy.json"), append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("exported %d nodes / %d edges, %d hub neighbor files → %s in %s\n",
		n, keptEdges, len(hubs), *webDir, time.Since(start).Round(time.Second))
}

// writeAttrs packs in-degrees then year offsets into one binary blob.
func writeAttrs(path string, inDeg []uint32, years []uint8) {
	buf := make([]byte, 4*len(inDeg)+len(years))
	for i, d := range inDeg {
		binary.LittleEndian.PutUint32(buf[4*i:], d)
	}
	copy(buf[4*len(inDeg):], years)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		log.Fatal(err)
	}
}

func copyFile(src, dst string, wantBytes int) {
	b, err := os.ReadFile(src)
	if err != nil {
		log.Fatal(err)
	}
	if len(b) != wantBytes {
		log.Fatalf("%s is %d bytes, want %d (layout and kept.tsv out of sync?)", src, len(b), wantBytes)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		log.Fatal(err)
	}
}

func writeLabels(dir string, paths []string, chunk int) {
	for c := 0; c*chunk < len(paths); c++ {
		lo := c * chunk
		hi := min(lo+chunk, len(paths))
		b, err := json.Marshal(paths[lo:hi])
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", c)), b, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func writeSearch(path string, paths []string, inDeg []uint32, top int) {
	idx := make([]int32, len(paths))
	for i := range idx {
		idx[i] = int32(i)
	}
	sort.Slice(idx, func(a, b int) bool { return inDeg[idx[a]] > inDeg[idx[b]] })
	if len(idx) > top {
		idx = idx[:top]
	}
	rows := make([][3]any, len(idx))
	for i, k := range idx {
		rows[i] = [3]any{paths[k], k, inDeg[k]}
	}
	b, err := json.Marshal(rows)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		log.Fatal(err)
	}
}

// writeNeighbors emits per-hub dependent/dependency lists (as kept
// indices), plus the index of which nodes have one. Coverage is the
// union of the top modules by in-degree (matching how the search table
// ranks, so anything findable is also highlightable) and the top by
// out-degree (the 1000-dependency monorepos are fun to click too).
// Files are small for most hubs; testify's is the biggest at ~1.4 MB.
func writeNeighbors(webDir string, origToKept []int32, from, to []int32, inDeg, outDeg []uint32, topIn, topOut int) []int32 {
	n := len(inDeg)
	rank := func(score []uint32, top int) []int32 {
		idx := make([]int32, n)
		for i := range idx {
			idx[i] = int32(i)
		}
		sort.Slice(idx, func(a, b int) bool { return score[idx[a]] > score[idx[b]] })
		if len(idx) > top {
			idx = idx[:top]
		}
		return idx
	}
	isHub := make([]bool, n)
	var idx []int32
	for _, k := range rank(inDeg, topIn) {
		if !isHub[k] {
			isHub[k] = true
			idx = append(idx, k)
		}
	}
	for _, k := range rank(outDeg, topOut) {
		if !isHub[k] {
			isHub[k] = true
			idx = append(idx, k)
		}
	}
	ins := make(map[int32][]int32, len(idx))
	outs := make(map[int32][]int32, len(idx))
	for i := range from {
		u, v := origToKept[from[i]], origToKept[to[i]]
		if u < 0 || v < 0 {
			continue
		}
		if isHub[v] {
			ins[v] = append(ins[v], u)
		}
		if isHub[u] {
			outs[u] = append(outs[u], v)
		}
	}
	for _, k := range idx {
		in, out := ins[k], outs[k]
		buf := make([]byte, 8+4*len(in)+4*len(out))
		binary.LittleEndian.PutUint32(buf, uint32(len(in)))
		binary.LittleEndian.PutUint32(buf[4:], uint32(len(out)))
		for i, v := range in {
			binary.LittleEndian.PutUint32(buf[8+4*i:], uint32(v))
		}
		for i, v := range out {
			binary.LittleEndian.PutUint32(buf[8+4*len(in)+4*i:], uint32(v))
		}
		if err := os.WriteFile(filepath.Join(webDir, "nbr", fmt.Sprintf("%d.bin", k)), buf, 0o644); err != nil {
			log.Fatal(err)
		}
	}
	sorted := slices.Clone(idx)
	slices.Sort(sorted)
	buf := make([]byte, 4*len(sorted))
	for i, k := range sorted {
		binary.LittleEndian.PutUint32(buf[4*i:], uint32(k))
	}
	if err := os.WriteFile(filepath.Join(webDir, "nbr-index.bin"), buf, 0o644); err != nil {
		log.Fatal(err)
	}
	return idx
}
