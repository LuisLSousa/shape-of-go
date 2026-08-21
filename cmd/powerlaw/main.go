// Command powerlaw fits a discrete power law to a degree histogram the
// defensible way (Clauset, Shalizi & Newman, 2009), instead of the
// quick fixed-kmin estimate cmd/analyze prints:
//
//   - exact discrete maximum-likelihood alpha (Hurwitz-zeta normalization,
//     not the continuous approximation),
//   - kmin chosen by scanning every candidate and keeping the one whose
//     fitted tail is closest to the data (minimal Kolmogorov-Smirnov
//     distance),
//   - a goodness-of-fit p-value from the semi-parametric bootstrap:
//     synthetic datasets drawn from the fitted model plus the empirical
//     body, each refitted from scratch, scored by how often synthetic
//     KS distance exceeds the observed one.
//
// Input is a two-column TSV of (degree, count) as written by analyze.
// Output (in -out, names prefixed by -label):
//
//	<label>-powerlaw.txt   fit summary, also printed to stdout
//	<label>-ccdf.tsv       empirical CCDF, P(K >= k) per distinct k
//	<label>-ccdf.svg       log-log CCDF with the fitted tail overlaid
package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	var (
		histPath = flag.String("hist", "data/graph/analysis/in-degree-hist.tsv", "degree histogram TSV (degree, count)")
		outDir   = flag.String("out", "data/graph/analysis", "output directory")
		label    = flag.String("label", "in-degree", "output file prefix and figure axis label")
		minTail  = flag.Int64("min-tail", 50, "smallest tail size a kmin candidate may leave")
		boot     = flag.Int("boot", 200, "bootstrap replicates for the goodness-of-fit p-value (0 = skip)")
		seed     = flag.Uint64("seed", 1, "bootstrap seed")
		workers  = flag.Int("workers", 0, "parallel workers (0 = all cores); NOTE: saturates every core while running")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)
	start := time.Now()

	h, err := loadHist(*histPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("loaded %s: %d observations, %d distinct degrees, max %d",
		*histPath, h.total, len(h.ks), h.ks[len(h.ks)-1])

	res := fit(h, *minTail, *workers)
	log.Printf("fit done in %s", time.Since(start).Round(time.Millisecond))

	pReport := "not run (-boot 0)"
	if *boot > 0 {
		bootStart := time.Now()
		p, exceed := bootstrapP(h, res, *boot, *seed, *minTail, *workers)
		pReport = fmt.Sprintf("%.4f (%d of %d synthetic KS >= observed; ±%.3f standard error)",
			p, exceed, *boot, pStdErr(p, *boot))
		log.Printf("bootstrap: %d replicates in %s", *boot, time.Since(bootStart).Round(time.Second))
	}

	var s strings.Builder
	fmt.Fprintf(&s, "power-law fit (Clauset-Shalizi-Newman), %s\n", *label)
	fmt.Fprintf(&s, "observations:        %d (%d distinct values)\n", h.total, len(h.ks))
	fmt.Fprintf(&s, "alpha:               %.4f (discrete MLE, Hurwitz-zeta normalized)\n", res.alpha)
	fmt.Fprintf(&s, "kmin:                %d (KS-optimal over %d candidates)\n", res.kmin, res.candidates)
	fmt.Fprintf(&s, "tail:                n = %d (%.4f%% of observations)\n", res.tailN, 100*float64(res.tailN)/float64(h.total))
	fmt.Fprintf(&s, "KS distance:         %.5f\n", res.ks)
	dep := departureK(h, res)
	fmt.Fprintf(&s, "fit tracks data to:  k ~ %d (%.1f decades above kmin; beyond, the tail steepens — finite-size cutoff)\n",
		dep, math.Log10(float64(dep)/float64(res.kmin)))
	fmt.Fprintf(&s, "goodness-of-fit p:   %s\n", pReport)
	fmt.Fprintf(&s, "\nreading: p < 0.1 rejects a pure power law (CSN's threshold). At n in the\n")
	fmt.Fprintf(&s, "millions the strict test rejects almost any real network; the honest claim\n")
	fmt.Fprintf(&s, "is the CCDF figure — how many decades the tail tracks the fitted slope.\n")
	summary := s.String()
	fmt.Print(summary)
	if err := os.WriteFile(filepath.Join(*outDir, *label+"-powerlaw.txt"), []byte(summary), 0o644); err != nil {
		log.Fatal(err)
	}

	if err := writeCCDF(filepath.Join(*outDir, *label+"-ccdf.tsv"), h); err != nil {
		log.Fatal(err)
	}
	if err := writeCCDFSVG(filepath.Join(*outDir, *label+"-ccdf.svg"), h, res, *label); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nwrote %s-powerlaw.txt, %s-ccdf.tsv, %s-ccdf.svg in %s\n",
		*label, *label, *label, time.Since(start).Round(time.Second))
}

func pStdErr(p float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return math.Sqrt(p * (1 - p) / float64(n))
}
