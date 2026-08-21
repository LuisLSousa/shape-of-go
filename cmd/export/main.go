// Command export packages a finished layout run into the static assets
// the web galaxy viewer loads: positions, per-node attributes, label
// chunks, a search table, and neighbor lists for the top hubs. All
// binary files are little-endian and indexed by "kept index", the node
// order of layout's positions.bin.
//
// Output (in -web):
//
//	galaxy.json      manifest: counts, chunking, file inventory
//	positions.bin    copied from the layout run (2 * float32 per node)
//	attrs.bin        n * uint32 full-graph in-degree, then n * uint8
//	                 first-seen year offset from 2019 (255 = unknown)
//	labels/<c>.json  node paths in kept order, -label-chunk per file
//	search.json      [path, keptIdx, inDegree] for the top -search-top
//	nbr-index.bin    hub lookup table: header, then sorted kept indices,
//	                 byte offsets, byte lengths, shard ids
//	nbr/<s>.bin      shard holding many hubs' neighbor records;
//	                 powers click-to-highlight
//
// Neighbor records are delta-varint encoded (see writeNeighbors) and
// packed into byte-budgeted shards rather than one file per hub: a file
// per hub meant 59,702 files holding 40 MB of data in 258 MB of disk
// blocks (median record: 96 bytes in a 4 KB block), which also made the
// dev server's file watcher spin and the Pages artifact needlessly deep.
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
	"math"
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
		nbrShardKB = flag.Int("nbr-shard-kb", 192, "target shard size; one oversized hub becomes its own shard")
		labelChunk = flag.Int("label-chunk", 4096, "labels per chunk file")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	start := time.Now()
	// The labels/ and nbr/ trees are keyed by kept index, which changes
	// between layout runs; stale files from a previous export would be
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
	hubs, nbrShards := writeNeighbors(*webDir, origToKept, from, to, inDeg, outDeg, *nbrTop, *nbrOutTop, *nbrShardKB<<10)

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
		"nbrShards":  nbrShards,
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
	fmt.Printf("exported %d nodes / %d edges, %d hubs in %d neighbor shards → %s in %s\n",
		n, keptEdges, len(hubs), nbrShards, *webDir, time.Since(start).Round(time.Second))
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

// writeNeighbors emits every hub's dependent/dependency lists, packed
// into byte-budgeted shards, plus a lookup table locating each hub's
// record. Coverage is the union of the top modules by in-degree
// (matching how the search table ranks, so anything findable is also
// highlightable) and the top by out-degree (the 1000-dependency
// monorepos are fun to click too).
//
// Hubs are packed in rank order, so the most-clicked ones share the
// first shards and one download serves many subsequent clicks. A hub
// whose record alone exceeds the budget becomes its own shard, which
// keeps the handful of giants (testify's record is by far the largest)
// from bloating a shard shared with small fry.
//
// Each record is: uvarint dependent count, uvarint dependency count,
// then both lists as ascending delta uvarints. Sorting makes the deltas
// small (a hub with 300k dependents scattered over 1.2M nodes averages
// a delta near 4, so one byte replaces four) and makes output
// deterministic. Binaries are served uncompressed, so this encoding is
// the only compression lever available for them.
func writeNeighbors(webDir string, origToKept []int32, from, to []int32, inDeg, outDeg []uint32, topIn, topOut, shardBudget int) (hubs []int32, shards int) {
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
	// Encode every hub, packing records into shards in rank order.
	type location struct {
		key           int32
		shard         uint16
		offset, bytes uint32
	}
	locs := make([]location, 0, len(idx))
	var shard []byte
	flush := func() {
		if len(shard) == 0 {
			return
		}
		path := filepath.Join(webDir, "nbr", fmt.Sprintf("%d.bin", shards))
		if err := os.WriteFile(path, shard, 0o644); err != nil {
			log.Fatal(err)
		}
		shards++
		shard = shard[:0]
	}
	for _, k := range idx {
		rec := encodeNeighbors(ins[k], outs[k])
		if len(shard) > 0 && len(shard)+len(rec) > shardBudget {
			flush()
		}
		locs = append(locs, location{key: k, shard: uint16(shards), offset: uint32(len(shard)), bytes: uint32(len(rec))})
		shard = append(shard, rec...)
		if len(shard) >= shardBudget {
			flush()
		}
	}
	flush()
	if shards > math.MaxUint16 {
		log.Fatalf("%d shards exceeds the uint16 shard id in nbr-index.bin; raise -nbr-shard-kb", shards)
	}

	// Lookup table, sorted by kept index for binary search in the
	// viewer: header, then parallel arrays. The uint16 shard ids come
	// last so the uint32 arrays stay 4-byte aligned for typed-array
	// views.
	slices.SortFunc(locs, func(a, b location) int { return int(a.key) - int(b.key) })
	buf := make([]byte, 16, 16+14*len(locs))
	binary.LittleEndian.PutUint32(buf, nbrIndexMagic)
	binary.LittleEndian.PutUint32(buf[4:], 1) // format version
	binary.LittleEndian.PutUint32(buf[8:], uint32(len(locs)))
	binary.LittleEndian.PutUint32(buf[12:], uint32(shards))
	for _, l := range locs {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(l.key))
	}
	for _, l := range locs {
		buf = binary.LittleEndian.AppendUint32(buf, l.offset)
	}
	for _, l := range locs {
		buf = binary.LittleEndian.AppendUint32(buf, l.bytes)
	}
	for _, l := range locs {
		buf = binary.LittleEndian.AppendUint16(buf, l.shard)
	}
	if err := os.WriteFile(filepath.Join(webDir, "nbr-index.bin"), buf, 0o644); err != nil {
		log.Fatal(err)
	}
	return idx, shards
}

// nbrIndexMagic is "NBR1" read as a little-endian uint32; the viewer
// checks it so a stale asset tree fails loudly instead of decoding
// garbage.
const nbrIndexMagic = 0x3152424E

// encodeNeighbors packs one hub's record: both list lengths, then each
// list as ascending delta uvarints. The input slices are sorted in
// place.
func encodeNeighbors(in, out []int32) []byte {
	slices.Sort(in)
	slices.Sort(out)
	rec := make([]byte, 0, 2+len(in)+len(out))
	rec = binary.AppendUvarint(rec, uint64(len(in)))
	rec = binary.AppendUvarint(rec, uint64(len(out)))
	for _, list := range [][]int32{in, out} {
		prev := int32(0)
		for _, v := range list {
			rec = binary.AppendUvarint(rec, uint64(v-prev))
			prev = v
		}
	}
	return rec
}
