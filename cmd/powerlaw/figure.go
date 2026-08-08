package main

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
)

// ccdf returns, for every distinct degree k >= 1, P(K >= k) over all
// observations. The CCDF needs no binning, which is exactly why it is
// the honest presentation for heavy tails: every point is a plain
// fraction of the data.
func ccdf(h hist) (ks []int64, p []float64) {
	// Cumulate from the top so p[i] = P(K >= ks[i]).
	var above int64
	for i, v := range slices.Backward(h.ks) {
		if v < 1 {
			break
		}
		above += h.counts[i]
		ks = append(ks, v)
		p = append(p, float64(above)/float64(h.total))
	}
	// Reverse into ascending-degree order.
	for i, j := 0, len(ks)-1; i < j; i, j = i+1, j-1 {
		ks[i], ks[j] = ks[j], ks[i]
		p[i], p[j] = p[j], p[i]
	}
	return ks, p
}

func writeCCDF(path string, h hist) error {
	ks, p := ccdf(h)
	var b strings.Builder
	for i := range ks {
		fmt.Fprintf(&b, "%d\t%.10g\n", ks[i], p[i])
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// writeCCDFSVG renders the log-log CCDF with the fitted power-law tail
// overlaid from kmin onward, anchored at the empirical tail mass (the
// CSN presentation). Styling matches analyze's degree figure.
func writeCCDFSVG(path string, h hist, res fitResult, label string) error {
	const (
		w, hpx              = 640.0, 460.0
		mL, mR, mT, mB      = 64.0, 32.0, 56.0, 56.0
		ink, inkSoft, faint = "#1e293b", "#475569", "#94a3b8"
		grid, surface, blue = "#e2e8f0", "#fcfcfb", "#6366f1"
		amber               = "#f59e0b"
	)
	ks, p := ccdf(h)
	maxK := float64(ks[len(ks)-1])
	minP := p[len(p)-1]
	lx := func(k float64) float64 { return mL + math.Log10(k)/math.Log10(maxK)*(w-mL-mR) }
	ly := func(v float64) float64 {
		return mT + (hpx-mT-mB)*math.Log10(v)/math.Log10(minP)
	}

	var s strings.Builder
	fmt.Fprintf(&s, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" font-family="-apple-system, 'Segoe UI', Roboto, sans-serif">`, w, hpx, w, hpx)
	fmt.Fprintf(&s, `<rect width="%.0f" height="%.0f" fill="%s"/>`, w, hpx, surface)
	dep := departureK(h, res)
	title := fmt.Sprintf("The %s tail follows a power law for %.1f decades, then steepens", label, math.Log10(float64(dep)/float64(res.kmin)))
	if dep == ks[len(ks)-1] {
		title = fmt.Sprintf("The %s tail follows a power law for %.1f decades", label, math.Log10(maxK/float64(res.kmin)))
	}
	fmt.Fprintf(&s, `<text x="%.0f" y="26" font-size="14" font-weight="600" fill="%s">%s</text>`, mL, ink, title)
	fmt.Fprintf(&s, `<text x="%.0f" y="44" font-size="11" fill="%s">complementary CDF, log&#8211;log &#8212; discrete MLE &#945; = %.2f, KS-optimal k_min = %d (Clauset&#8211;Shalizi&#8211;Newman)</text>`,
		mL, inkSoft, res.alpha, res.kmin)

	// Decade gridlines.
	for e := 0; math.Pow(10, float64(e)) <= maxK; e++ {
		x := lx(math.Pow(10, float64(e)))
		fmt.Fprintf(&s, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`, x, mT, x, hpx-mB, grid)
		fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="middle">10^%d</text>`, x, hpx-mB+16, faint, e)
	}
	for e := 0; math.Pow(10, float64(-e)) >= minP; e++ {
		y := ly(math.Pow(10, float64(-e)))
		fmt.Fprintf(&s, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s"/>`, mL, y, w-mR, y, grid)
		fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="10" fill="%s" text-anchor="end">10^&#8722;%d</text>`, mL-6, y+3.5, faint, e)
	}
	fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="11" fill="%s" text-anchor="middle">%s k</text>`, (mL+w-mR)/2, hpx-12, inkSoft, label)
	fmt.Fprintf(&s, `<text x="16" y="%.1f" font-size="11" fill="%s" text-anchor="middle" transform="rotate(-90 16 %.1f)">P(K &#8805; k)</text>`, (mT+hpx-mB)/2, inkSoft, (mT+hpx-mB)/2)

	// Empirical CCDF.
	for i := range ks {
		fmt.Fprintf(&s, `<circle cx="%.1f" cy="%.1f" r="1.6" fill="%s" opacity="0.55"/>`, lx(float64(ks[i])), ly(p[i]), blue)
	}

	// Fitted tail: P(K >= k) = P_emp(K >= kmin) · ζ(α,k)/ζ(α,kmin),
	// drawn only over the fitted range, per CSN.
	_, tailN, _ := h.tailOf(res.kmin)
	anchor := float64(tailN) / float64(h.total)
	zKmin := zetaHurwitz(res.alpha, float64(res.kmin))
	var d strings.Builder
	steps := 120
	for i := 0; i <= steps; i++ {
		k := float64(res.kmin) * math.Pow(maxK/float64(res.kmin), float64(i)/float64(steps))
		v := anchor * zetaHurwitz(res.alpha, k) / zKmin
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&d, "%s%.1f %.1f ", cmd, lx(k), ly(v))
	}
	fmt.Fprintf(&s, `<path d="%s" fill="none" stroke="%s" stroke-width="2" stroke-dasharray="6 4"/>`, strings.TrimSpace(d.String()), amber)
	fmt.Fprintf(&s, `<text x="%.1f" y="%.1f" font-size="11" fill="%s">&#8764; k^&#8722;(&#945;&#8722;1), &#945; = %.2f</text>`,
		lx(float64(res.kmin))+12, ly(anchor)-10, ink, res.alpha)

	s.WriteString(`</svg>`)
	return os.WriteFile(path, []byte(s.String()), 0o644)
}
