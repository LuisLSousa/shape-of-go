// Command layout computes 2-D positions for the dependency-graph galaxy.
// It loads the buildgraph node/edge tables, keeps the giant weakly-
// connected component (optionally thinned to a minimum in-degree for
// fast development runs), and runs ForceAtlas2 with Barnes-Hut
// repulsion — the standard layout for large scale-free networks.
//
// The run is deterministic (seeded init, fixed reduction order),
// parallel across cores, and checkpointed: positions are written every
// -checkpoint-every iterations together with a PNG preview, so a long
// run can be watched and resumed (-resume) after interruption.
//
// Output (in -out):
//
//	positions.bin  kept-node positions, 2 × float32 little-endian each
//	kept.tsv       kept index (line number) → original node id
//	meta.json      node/edge counts and the exact parameters used
//	preview.png    additive scatter render of the current positions
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type params struct {
	Nodes           int     `json:"nodes"`
	Edges           int     `json:"edges"`
	MinInDeg        int     `json:"minInDeg"`
	Iterations      int     `json:"iterations"`
	Theta           float64 `json:"theta"`
	Scaling         float64 `json:"scaling"`
	Gravity         float64 `json:"gravity"`
	StrongGravity   bool    `json:"strongGravity"`
	LinLog          bool    `json:"linLog"`
	JitterTolerance float64 `json:"jitterTolerance"`
	Seed            uint64  `json:"seed"`
}

func main() {
	var (
		graphDir  = flag.String("graph", "data/graph", "buildgraph output directory")
		outDir    = flag.String("out", "data/galaxy", "layout output directory")
		minInDeg  = flag.Int("min-indeg", 0, "keep only nodes with at least this in-degree (0 = keep all; >0 gives a fast dev subgraph)")
		iters     = flag.Int("iters", 600, "force-directed iterations")
		theta     = flag.Float64("theta", 1.2, "Barnes-Hut accuracy (higher = faster, coarser)")
		scaling   = flag.Float64("scaling", 10, "repulsion scaling (kr)")
		gravity   = flag.Float64("gravity", 1.0, "gravity toward the origin")
		strongG   = flag.Bool("strong-gravity", false, "distance-proportional gravity (compacts stray satellites)")
		linlog    = flag.Bool("linlog", false, "logarithmic attraction (tighter clusters)")
		tolerance = flag.Float64("tolerance", 1.0, "jitter tolerance (higher = faster, less precise)")
		seed      = flag.Uint64("seed", 42, "layout init seed")
		ckEvery   = flag.Int("checkpoint-every", 50, "write positions + preview every N iterations")
		resume    = flag.Bool("resume", false, "start from existing positions.bin instead of a fresh init")
		workers   = flag.Int("workers", runtime.NumCPU(), "parallel force workers")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	start := time.Now()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	nNodes := countNodes(filepath.Join(*graphDir, "nodes.tsv"))
	from, to := loadEdges(filepath.Join(*graphDir, "edges.tsv"))
	log.Printf("loaded %d nodes, %d edges in %s", nNodes, len(from), time.Since(start).Round(time.Millisecond))

	keep := giantComponent(nNodes, from, to)
	if *minInDeg > 0 {
		inDeg := make([]int32, nNodes)
		for _, v := range to {
			inDeg[v]++
		}
		for i := range keep {
			keep[i] = keep[i] && inDeg[i] >= int32(*minInDeg)
		}
		// Thinning strands nodes whose every neighbor was filtered
		// out; with no edges left they would just be blasted outward
		// as debris rings. Drop them (a no-op for the unthinned run).
		subDeg := make([]int32, nNodes)
		for i := range from {
			if keep[from[i]] && keep[to[i]] {
				subDeg[from[i]]++
				subDeg[to[i]]++
			}
		}
		for i := range keep {
			keep[i] = keep[i] && subDeg[i] > 0
		}
	}

	kept, keptIdx := reindex(keep)
	adjOff, adj := subgraphCSR(kept, keptIdx, from, to)
	n, m := len(kept), len(adj)/2
	log.Printf("kept %d nodes, %d edges (giant component, min in-degree %d)", n, m, *minInDeg)

	l := newLayout(n, adjOff, adj, fa2Config{
		Theta:           *theta,
		Scaling:         *scaling,
		Gravity:         *gravity,
		StrongGravity:   *strongG,
		LinLog:          *linlog,
		JitterTolerance: *tolerance,
		Workers:         *workers,
	})
	posPath := filepath.Join(*outDir, "positions.bin")
	if *resume {
		if err := l.loadPositions(posPath); err != nil {
			log.Fatalf("resume: %v", err)
		}
		log.Printf("resumed from %s", posPath)
	} else {
		l.initPositions(*seed)
	}

	writeKept(filepath.Join(*outDir, "kept.tsv"), kept)
	meta := params{
		Nodes: n, Edges: m, MinInDeg: *minInDeg, Iterations: *iters,
		Theta: *theta, Scaling: *scaling, Gravity: *gravity,
		StrongGravity: *strongG, LinLog: *linlog,
		JitterTolerance: *tolerance, Seed: *seed,
	}
	writeMeta(filepath.Join(*outDir, "meta.json"), meta)

	checkpoint := func(iter int) {
		if err := l.writePositions(posPath); err != nil {
			log.Fatal(err)
		}
		writePreview(filepath.Join(*outDir, "preview.png"), l.pos)
		log.Printf("checkpoint at iteration %d (%s elapsed)", iter, time.Since(start).Round(time.Second))
	}

	for iter := 1; iter <= *iters; iter++ {
		l.step()
		if iter%10 == 0 || iter == 1 {
			log.Printf("iter %4d  speed %8.3f  swinging/traction %.3f  radius %.0f",
				iter, l.speed, l.swinging/l.traction, l.radius())
		}
		if iter%*ckEvery == 0 && iter != *iters {
			checkpoint(iter)
		}
	}
	checkpoint(*iters)
	fmt.Printf("laid out %d nodes / %d edges in %s → %s\n", n, m, time.Since(start).Round(time.Second), *outDir)
}

func writeMeta(path string, meta params) {
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		log.Fatal(err)
	}
}
