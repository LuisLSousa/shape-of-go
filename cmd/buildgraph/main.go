// Command buildgraph turns the fetched go.mod shards into the Go
// ecosystem's dependency graph: a node table and a directed edge list.
//
// Nodes are module paths. Every fetched module is a node; so is every
// module that appears as a requirement, even if it was never fetched
// (marked unresolved: retracted, private, or renamed modules).
//
// Edges follow direct dependencies only: one edge per (module,
// require) pair for requires not marked "// indirect", so the graph
// reflects declared use, not the transitive closure. Self-edges and
// duplicate requires are dropped. Modules whose go.mod declares a
// different path than the one they are published under (vanity drift)
// keep their published (index) identity, and the mismatch is counted.
//
// Output (in -out):
//
//	nodes.tsv  id, path, resolved(1|0)
//	edges.tsv  from-id, to-id   (from depends on to)
//	stats.txt  summary counters
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"golang.org/x/mod/modfile"
)

type shardRecord struct {
	Path    string `json:"p"`
	Version string `json:"v"`
	Mod     string `json:"mod,omitempty"`
	Err     string `json:"err,omitempty"`
}

// parsed is one module's contribution, produced by a worker.
type parsed struct {
	src      string
	requires []string
	mismatch bool // go.mod declares a different module path
	broken   bool // go.mod failed to parse
}

func main() {
	var (
		modsDir = flag.String("mods", "data/mods", "fetchmods shard directory")
		outDir  = flag.String("out", "data/graph", "output directory")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	start := time.Now()

	shards, err := filepath.Glob(filepath.Join(*modsDir, "shard-*.jsonl"))
	if err != nil || len(shards) == 0 {
		log.Fatalf("no shards in %s (run fetchmods first)", *modsDir)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	// Workers parse shard files; the aggregator below owns all maps, so
	// no locks are needed anywhere.
	results := make(chan parsed, 4096)
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for _, shard := range shards {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			parseShard(path, results)
		}(shard)
	}
	go func() { wg.Wait(); close(results) }()

	var (
		ids      = make(map[string]int32, 6<<20)
		paths    []string
		resolved []bool
		edges    = make([][2]int32, 0, 24<<20)
		seen     = make(map[[2]int32]bool, 1<<20) // dedupe per-module requires
		errs     = struct{ fetch, parse, mismatch, dupEdge, selfEdge int }{}
	)
	id := func(path string, isResolved bool) int32 {
		if i, ok := ids[path]; ok {
			if isResolved {
				resolved[i] = true
			}
			return i
		}
		i := int32(len(paths))
		ids[path] = i
		paths = append(paths, path)
		resolved = append(resolved, isResolved)
		return i
	}

	modules := 0
	for p := range results {
		switch {
		case p.broken:
			errs.parse++
			continue
		case p.src == "":
			errs.fetch++
			continue
		}
		modules++
		if p.mismatch {
			errs.mismatch++
		}
		u := id(p.src, true)
		for _, req := range p.requires {
			v := id(req, false)
			if u == v {
				errs.selfEdge++
				continue
			}
			k := [2]int32{u, v}
			if seen[k] {
				errs.dupEdge++
				continue
			}
			seen[k] = true
			edges = append(edges, k)
		}
		if modules%500_000 == 0 {
			log.Printf("%dk modules parsed, %dM edges", modules/1000, len(edges)/1_000_000)
		}
	}

	writeNodes(filepath.Join(*outDir, "nodes.tsv"), paths, resolved)
	writeEdges(filepath.Join(*outDir, "edges.tsv"), edges)

	unresolvedN := 0
	for _, r := range resolved {
		if !r {
			unresolvedN++
		}
	}
	stats := fmt.Sprintf(`modules parsed:      %d
nodes total:         %d
  unresolved:        %d (required but never fetched)
direct edges:        %d
fetch errors:        %d (no go.mod available)
unparsable go.mod:   %d
vanity mismatches:   %d (declared path != published path)
self/dup requires:   %d / %d
elapsed:             %s
`, modules, len(paths), unresolvedN, len(edges),
		errs.fetch, errs.parse, errs.mismatch, errs.selfEdge, errs.dupEdge,
		time.Since(start).Round(time.Second))
	if err := os.WriteFile(filepath.Join(*outDir, "stats.txt"), []byte(stats), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Print(stats)
}

func parseShard(path string, results chan<- parsed) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r shardRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue // torn tail line from a crash-resume
		}
		if r.Err != "" || r.Mod == "" {
			results <- parsed{} // fetch error: counted, no node
			continue
		}
		mf, err := modfile.ParseLax(r.Path+"/go.mod", []byte(r.Mod), nil)
		if err != nil {
			results <- parsed{broken: true}
			continue
		}
		p := parsed{src: r.Path}
		if mf.Module != nil && mf.Module.Mod.Path != r.Path {
			p.mismatch = true
		}
		for _, req := range mf.Require {
			if req.Indirect || req.Mod.Path == "" {
				continue
			}
			p.requires = append(p.requires, req.Mod.Path)
		}
		results <- p
	}
	if err := sc.Err(); err != nil {
		log.Printf("warning: reading %s: %v", path, err)
	}
}

func writeNodes(path string, paths []string, resolved []bool) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 4<<20)
	defer w.Flush()
	for i, p := range paths {
		r := 0
		if resolved[i] {
			r = 1
		}
		fmt.Fprintf(w, "%d\t%s\t%d\n", i, p, r)
	}
}

func writeEdges(path string, edges [][2]int32) {
	// Deterministic output: sort by (from, to) regardless of shard
	// scheduling. Node ids depend on aggregation order, so full
	// determinism comes from re-running with the same shard set; the
	// sort makes diffs stable within a run.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i][0] != edges[j][0] {
			return edges[i][0] < edges[j][0]
		}
		return edges[i][1] < edges[j][1]
	})
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 4<<20)
	defer w.Flush()
	for _, e := range edges {
		fmt.Fprintf(w, "%d\t%d\n", e[0], e[1])
	}
}
