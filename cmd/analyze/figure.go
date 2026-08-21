package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
)

// writeDegreeSVG renders the in-degree distribution on log-log axes
// with the fitted power law overlaid: the scale-free signature plot.
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

	// Data points, in degree order so the file is byte-stable across
	// runs (map iteration order is not).
	keys := make([]int32, 0, len(hist))
	for k := range hist {
		if k != 0 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })
	for _, k := range keys {
		fmt.Fprintf(&s, `<circle cx="%.1f" cy="%.1f" r="1.6" fill="%s" opacity="0.55"/>`, lx(float64(k)), ly(float64(hist[k])), blue)
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
