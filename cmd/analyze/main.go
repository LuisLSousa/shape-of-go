// Command analyze computes the ecosystem-level network science on the
// graph produced by buildgraph: degree distributions (with a power-law
// exponent estimate for the scale-free question; cmd/powerlaw does the
// full Clauset treatment), weakly-connected components, PageRank, and
// the headline extremes. Results are written as TSVs plus a log-log
// degree-distribution SVG, and a summary is printed.
//
// The directed-graph machinery that first shipped here (dual-CSR
// adjacency, pull-based PageRank with dangling redistribution, weak
// components) was extracted into gonx v1.1 (Digraph, metrics.PageRank,
// metrics.WeaklyConnectedComponents). analyze now consumes the library
// it seeded.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LuisLSousa/gonx"
	"github.com/LuisLSousa/gonx/metrics"
)

func main() {
	var (
		graphDir = flag.String("graph", "data/graph", "buildgraph output directory")
		outDir   = flag.String("out", "data/graph/analysis", "analysis output directory")
		topN     = flag.Int("top", 25, "how many top modules to report")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	start := time.Now()
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	paths := loadNodes(filepath.Join(*graphDir, "nodes.tsv"))
	g := loadDigraph(filepath.Join(*graphDir, "edges.tsv"), len(paths))
	n, m := g.NumNodes(), g.NumEdges()
	log.Printf("loaded %d nodes, %d edges in %s", n, m, time.Since(start).Round(time.Millisecond))

	// Degree histograms (for the log-log plot and the TSVs).
	inDeg := make([]int32, n)
	outDeg := make([]int32, n)
	for u := range n {
		inDeg[u] = int32(g.InDegree(u))
		outDeg[u] = int32(g.OutDegree(u))
	}
	inHist := histogram(inDeg)
	outHist := histogram(outDeg)
	writeHist(filepath.Join(*outDir, "in-degree-hist.tsv"), inHist)
	writeHist(filepath.Join(*outDir, "out-degree-hist.tsv"), outHist)

	// Power-law exponent for the in-degree tail (quick fixed-kmin MLE;
	// see cmd/powerlaw for the KS-optimal fit and CCDF figure).
	const kmin = 10
	alpha, tail := powerLawAlpha(inDeg, kmin)

	// Isolation and connectivity.
	var noIn, noOut, isolated int
	for i := range inDeg {
		switch {
		case inDeg[i] == 0 && outDeg[i] == 0:
			isolated++
		case inDeg[i] == 0:
			noIn++
		case outDeg[i] == 0:
			noOut++
		}
	}
	components := metrics.WeaklyConnectedComponents(g)
	giant := 0
	for _, c := range components {
		giant = max(giant, len(c))
	}

	// PageRank on depends-on edges: importance flows from dependent to
	// dependency, so central infrastructure accumulates rank. The
	// tolerance asks for more precision than 100 damped iterations can
	// certify, so the run uses the full budget; the returned ranks are
	// converged far beyond what the top-N ordering needs.
	pr, err := metrics.PageRank(g, 0.85, 1e-12, 100)
	if err != nil && !errors.Is(err, metrics.ErrNoConvergence) {
		log.Fatal(err)
	}

	summary := &strings.Builder{}
	fmt.Fprintf(summary, "nodes: %d   direct edges: %d\n", n, m)
	fmt.Fprintf(summary, "isolated (no edges at all):        %d (%.1f%%)\n", isolated, pct(isolated, n))
	fmt.Fprintf(summary, "never imported (in-degree 0):      %d (%.1f%%)\n", noIn+isolated, pct(noIn+isolated, n))
	fmt.Fprintf(summary, "no dependencies (out-degree 0):    %d (%.1f%%)\n", noOut+isolated, pct(noOut+isolated, n))
	fmt.Fprintf(summary, "weakly connected components:       %d\n", len(components))
	fmt.Fprintf(summary, "giant component:                   %d nodes (%.1f%%)\n", giant, pct(giant, n))
	fmt.Fprintf(summary, "in-degree power law:               alpha = %.2f (MLE, k >= %d, tail n = %d)\n", alpha, kmin, tail)
	fmt.Fprintf(summary, "\ntop %d by PageRank:\n", *topN)
	for _, i := range topK(pr, *topN) {
		fmt.Fprintf(summary, "%12.6f  %-50s (in-degree %d)\n", pr[i]*1e3, paths[i], inDeg[i])
	}
	fmt.Fprintf(summary, "\ntop %d by out-degree (most declared direct deps):\n", 10)
	outF := make([]float64, n)
	for i, d := range outDeg {
		outF[i] = float64(d)
	}
	for _, i := range topK(outF, 10) {
		fmt.Fprintf(summary, "%6d  %s\n", outDeg[i], paths[i])
	}
	fmt.Fprintf(summary, "\nelapsed: %s\n", time.Since(start).Round(time.Second))

	if err := os.WriteFile(filepath.Join(*outDir, "summary.txt"), []byte(summary.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	writeDegreeSVG(filepath.Join(*outDir, "in-degree-loglog.svg"), inHist, alpha, kmin)
	fmt.Print(summary.String())
}

func loadNodes(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	var paths []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 3)
		if len(parts) < 2 {
			continue
		}
		paths = append(paths, parts[1])
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	return paths
}

// loadDigraph streams the edge list straight into a gonx builder.
// buildgraph guarantees deduplicated, self-loop-free ordered pairs, so
// the unchecked insert path applies and the build stays O(m).
func loadDigraph(path string, n int) *gonx.Digraph {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	b := gonx.NewDigraphBuilder(n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		us, vs, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		u, err1 := strconv.Atoi(us)
		v, err2 := strconv.Atoi(vs)
		if err1 != nil || err2 != nil {
			continue
		}
		b.AddEdgeUnchecked(u, v)
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	return b.Build()
}

func histogram(deg []int32) map[int32]int64 {
	h := make(map[int32]int64)
	for _, d := range deg {
		h[d]++
	}
	return h
}

func writeHist(path string, h map[int32]int64) {
	ks := make([]int32, 0, len(h))
	for k := range h {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	var b strings.Builder
	for _, k := range ks {
		fmt.Fprintf(&b, "%d\t%d\n", k, h[k])
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
}

// powerLawAlpha estimates the exponent of P(k) ~ k^-alpha for the
// degree tail k >= kmin via the discrete MLE approximation
// alpha = 1 + n / sum(ln(k_i/(kmin-0.5))).
func powerLawAlpha(deg []int32, kmin int32) (alpha float64, tail int) {
	var sum float64
	for _, d := range deg {
		if d >= kmin {
			sum += math.Log(float64(d) / (float64(kmin) - 0.5))
			tail++
		}
	}
	if tail == 0 {
		return 0, 0
	}
	return 1 + float64(tail)/sum, tail
}

func topK(score []float64, k int) []int {
	idx := make([]int, len(score))
	for i := range idx {
		idx[i] = i
	}
	// Ties break on node id so the listing is byte-stable across runs.
	sort.Slice(idx, func(a, b int) bool {
		if score[idx[a]] != score[idx[b]] {
			return score[idx[a]] > score[idx[b]]
		}
		return idx[a] < idx[b]
	})
	if len(idx) > k {
		idx = idx[:k]
	}
	return idx
}

func pct(a, b int) float64 { return 100 * float64(a) / float64(b) }
