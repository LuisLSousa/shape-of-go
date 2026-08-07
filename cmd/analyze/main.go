// Command analyze computes the ecosystem-level network science on the
// graph produced by buildgraph: degree distributions (with a power-law
// exponent estimate for the scale-free question), weakly-connected
// components, PageRank, and the headline extremes. Results are written
// as TSVs plus a log-log degree-distribution SVG, and a summary is
// printed.
package main

import (
	"bufio"
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
	from, to := loadEdges(filepath.Join(*graphDir, "edges.tsv"))
	n, m := len(paths), len(from)
	log.Printf("loaded %d nodes, %d edges in %s", n, m, time.Since(start).Round(time.Millisecond))

	inDeg := make([]int32, n)
	outDeg := make([]int32, n)
	for i := range from {
		outDeg[from[i]]++
		inDeg[to[i]]++
	}

	// Degree histograms (for the log-log plot and the TSVs).
	inHist := histogram(inDeg)
	outHist := histogram(outDeg)
	writeHist(filepath.Join(*outDir, "in-degree-hist.tsv"), inHist)
	writeHist(filepath.Join(*outDir, "out-degree-hist.tsv"), outHist)

	// Power-law exponent for the in-degree tail (Clauset-style MLE with
	// a fixed kmin — an honest first estimate, not a full KS scan).
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
	giant, components := weakComponents(n, from, to)

	// PageRank on depends-on edges: importance flows from dependent to
	// dependency, so central infrastructure accumulates rank.
	pr := pagerank(n, from, to, outDeg, 0.85, 1e-12, 100)

	summary := &strings.Builder{}
	fmt.Fprintf(summary, "nodes: %d   direct edges: %d\n", n, m)
	fmt.Fprintf(summary, "isolated (no edges at all):        %d (%.1f%%)\n", isolated, pct(isolated, n))
	fmt.Fprintf(summary, "never imported (in-degree 0):      %d (%.1f%%)\n", noIn+isolated, pct(noIn+isolated, n))
	fmt.Fprintf(summary, "no dependencies (out-degree 0):    %d (%.1f%%)\n", noOut+isolated, pct(noOut+isolated, n))
	fmt.Fprintf(summary, "weakly connected components:       %d\n", components)
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

func loadEdges(path string) (from, to []int32) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
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
		from = append(from, int32(u))
		to = append(to, int32(v))
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	return from, to
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

// weakComponents runs union-find over the edges, ignoring direction.
func weakComponents(n int, from, to []int32) (giant, count int) {
	parent := make([]int32, n)
	for i := range parent {
		parent[i] = int32(i)
	}
	var find func(x int32) int32
	find = func(x int32) int32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]] // path halving
			x = parent[x]
		}
		return x
	}
	for i := range from {
		a, b := find(from[i]), find(to[i])
		if a != b {
			parent[a] = b
		}
	}
	size := make(map[int32]int, 1<<20)
	for i := range parent {
		size[find(int32(i))]++
	}
	for _, s := range size {
		giant = max(giant, s)
	}
	return giant, len(size)
}

// pagerank iterates the standard damped formulation with uniform
// redistribution of dangling mass until the L1 delta drops below eps.
func pagerank(n int, from, to []int32, outDeg []int32, d, eps float64, maxIter int) []float64 {
	rank := make([]float64, n)
	next := make([]float64, n)
	for i := range rank {
		rank[i] = 1 / float64(n)
	}
	for iter := range maxIter {
		var dangling float64
		for i := range next {
			next[i] = 0
		}
		for i, r := range rank {
			if outDeg[i] == 0 {
				dangling += r
			}
		}
		for i := range from {
			next[to[i]] += rank[from[i]] / float64(outDeg[from[i]])
		}
		base := (1-d)/float64(n) + d*dangling/float64(n)
		var delta float64
		for i := range next {
			next[i] = base + d*next[i]
			delta += math.Abs(next[i] - rank[i])
		}
		rank, next = next, rank
		if delta < eps {
			log.Printf("pagerank converged after %d iterations", iter+1)
			break
		}
	}
	return rank
}

func topK(score []float64, k int) []int {
	idx := make([]int, len(score))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return score[idx[a]] > score[idx[b]] })
	if len(idx) > k {
		idx = idx[:k]
	}
	return idx
}

func pct(a, b int) float64 { return 100 * float64(a) / float64(b) }

// writeDegreeSVG renders the in-degree distribution on log-log axes
// with the fitted power law overlaid — the scale-free signature plot.
func writeDegreeSVG(path string, hist map[int32]int64, alpha float64, kmin int32) {
	const (
		w, h                = 640.0, 460.0
		mL, mR, mT, mB      = 64.0, 32.0, 56.0, 56.0
		ink, inkSoft, faint = "#1e293b", "#475569", "#94a3b8"
		grid, surface, blue = "#e2e8f0", "#fcfcfb", "#6366f1"
		amber               = "#f59e0b"
	)
	var maxK, maxC float64 = 1, 1
	for k, c := range hist {
		if k == 0 {
			continue
		}
		maxK = math.Max(maxK, float64(k))
		maxC = math.Max(maxC, float64(c))
	}
	lx := func(k float64) float64 { return mL + math.Log10(k)/math.Log10(maxK)*(w-mL-mR) }
	ly := func(c float64) float64 { return h - mB - math.Log10(c)/math.Log10(maxC)*(h-mT-mB) }

	var s strings.Builder
	fmt.Fprintf(&s, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" font-family="-apple-system, 'Segoe UI', Roboto, sans-serif">`, w, h, w, h)
	fmt.Fprintf(&s, `<rect width="%.0f" height="%.0f" fill="%s"/>`, w, h, surface)
	fmt.Fprintf(&s, `<text x="%.0f" y="26" font-size="14" font-weight="600" fill="%s">The Go ecosystem is scale-free</text>`, mL, ink)
	fmt.Fprintf(&s, `<text x="%.0f" y="44" font-size="11" fill="%s">in-degree distribution of the module dependency graph, log&#8211;log &#8212; fitted exponent &#945; = %.2f</text>`, mL, inkSoft, alpha)

	// Decade gridlines and labels.
	for e := 0; math.Pow(10, float64(e)) <= maxK; e++ {
		x := lx(math.Pow(10, float64(e)))
		fmt.Fprintf(&s, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`, x, mT, x, h-mB, grid)
		fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="middle">10^%d</text>`, x, h-mB+16, faint, e)
	}
	for e := 0; math.Pow(10, float64(e)) <= maxC; e++ {
		y := ly(math.Pow(10, float64(e)))
		fmt.Fprintf(&s, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`, mL, y, w-mR, y, grid)
		fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="end">10^%d</text>`, mL-6, y+3.5, faint, e)
	}
	fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="11" fill="%s" text-anchor="middle">in-degree k (direct dependents)</text>`, (mL+w-mR)/2, h-12, inkSoft)
	fmt.Fprintf(&s, `<text x="16" y="%.1f" font-size="11" fill="%s" text-anchor="middle" transform="rotate(-90 16 %.1f)">number of modules</text>`, (mT+h-mB)/2, inkSoft, (mT+h-mB)/2)

	// Data points.
	for k, c := range hist {
		if k == 0 {
			continue
		}
		fmt.Fprintf(&s, `<circle cx="%.1f" cy="%.1f" r="1.6" fill="%s" opacity="0.55"/>`, lx(float64(k)), ly(float64(c)), blue)
	}
	// Fitted power law, anchored at (kmin, count(kmin)).
	if c0, ok := hist[kmin]; ok && c0 > 0 {
		x1, y1 := float64(kmin), float64(c0)
		x2 := maxK
		y2 := y1 * math.Pow(x2/x1, -alpha)
		fmt.Fprintf(&s, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="2" stroke-dasharray="6 4"/>`, lx(x1), ly(y1), lx(x2), ly(math.Max(y2, 0.5)), amber)
		fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="11" fill="%s">k^&#8722;%.2f</text>`, lx(x1)+10, ly(y1)-8, ink, alpha)
	}
	s.WriteString(`</svg>`)
	if err := os.WriteFile(path, []byte(s.String()), 0o644); err != nil {
		log.Fatal(err)
	}
}
