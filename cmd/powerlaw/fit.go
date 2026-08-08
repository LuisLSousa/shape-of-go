package main

import (
	"bufio"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// hist is a degree histogram: parallel slices sorted by degree, plus
// the total observation count. All fitting works on this sufficient
// statistic — the raw node list is never needed.
type hist struct {
	ks     []int64
	counts []int64
	total  int64
}

func loadHist(path string) (hist, error) {
	f, err := os.Open(path)
	if err != nil {
		return hist{}, err
	}
	defer f.Close()
	var h hist
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		ks, cs, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		k, err1 := strconv.ParseInt(ks, 10, 64)
		c, err2 := strconv.ParseInt(cs, 10, 64)
		if err1 != nil || err2 != nil || c <= 0 {
			continue
		}
		h.ks = append(h.ks, k)
		h.counts = append(h.counts, c)
		h.total += c
	}
	if err := sc.Err(); err != nil {
		return hist{}, err
	}
	if !sort.SliceIsSorted(h.ks, func(a, b int) bool { return h.ks[a] < h.ks[b] }) {
		sort.Sort(byDegree{&h})
	}
	if len(h.ks) < 3 {
		return hist{}, fmt.Errorf("%s: need at least 3 distinct degrees, got %d", path, len(h.ks))
	}
	return h, nil
}

type byDegree struct{ h *hist }

func (b byDegree) Len() int           { return len(b.h.ks) }
func (b byDegree) Less(i, j int) bool { return b.h.ks[i] < b.h.ks[j] }
func (b byDegree) Swap(i, j int) {
	b.h.ks[i], b.h.ks[j] = b.h.ks[j], b.h.ks[i]
	b.h.counts[i], b.h.counts[j] = b.h.counts[j], b.h.counts[i]
}

// zetaHurwitz computes the Hurwitz zeta function ζ(s, q) = Σ (q+i)^-s
// for s > 1, q >= 1, via Euler–Maclaurin: fifteen explicit terms, the
// integral remainder, and three correction terms. Absolute error is far
// below 1e-10 over the (s, q) ranges the fit visits.
func zetaHurwitz(s, q float64) float64 {
	const terms = 15
	var sum float64
	for i := range terms {
		sum += math.Pow(q+float64(i), -s)
	}
	a := q + terms
	sum += math.Pow(a, 1-s) / (s - 1)
	sum += 0.5 * math.Pow(a, -s)
	sum += s / 12 * math.Pow(a, -s-1)
	sum -= s * (s + 1) * (s + 2) / 720 * math.Pow(a, -s-3)
	sum += s * (s + 1) * (s + 2) * (s + 3) * (s + 4) / 30240 * math.Pow(a, -s-5)
	return sum
}

// tailOf returns the index of the first degree >= kmin, the tail count,
// and Σ c_k·ln k over the tail.
func (h hist) tailOf(kmin int64) (first int, n int64, sumLogK float64) {
	first = sort.Search(len(h.ks), func(i int) bool { return h.ks[i] >= kmin })
	for i := first; i < len(h.ks); i++ {
		n += h.counts[i]
		sumLogK += float64(h.counts[i]) * math.Log(float64(h.ks[i]))
	}
	return first, n, sumLogK
}

// mleAlpha maximizes the discrete power-law log-likelihood
//
//	L(α) = −n·ln ζ(α, kmin) − α·Σ c_k·ln k
//
// over the tail k >= kmin by golden-section search. The likelihood is
// strictly unimodal in α, so the bracket [1.01, 12] is safe for any
// degree data this pipeline produces.
func mleAlpha(kmin int64, n int64, sumLogK float64) float64 {
	logL := func(alpha float64) float64 {
		return -float64(n)*math.Log(zetaHurwitz(alpha, float64(kmin))) - alpha*sumLogK
	}
	const phi = 0.6180339887498949
	lo, hi := 1.01, 12.0
	x1 := hi - phi*(hi-lo)
	x2 := lo + phi*(hi-lo)
	f1, f2 := logL(x1), logL(x2)
	for hi-lo > 1e-6 {
		if f1 < f2 {
			lo, x1, f1 = x1, x2, f2
			x2 = lo + phi*(hi-lo)
			f2 = logL(x2)
		} else {
			hi, x2, f2 = x2, x1, f1
			x1 = hi - phi*(hi-lo)
			f1 = logL(x1)
		}
	}
	return (lo + hi) / 2
}

// ksDistance is the Kolmogorov–Smirnov statistic between the tail's
// empirical CDF and the fitted discrete power law, both conditioned on
// k >= kmin, evaluated at every distinct observed degree.
func ksDistance(h hist, first int, n int64, kmin int64, alpha float64) float64 {
	zKmin := zetaHurwitz(alpha, float64(kmin))
	var cum int64
	var worst float64
	for i := first; i < len(h.ks); i++ {
		cum += h.counts[i]
		emp := float64(cum) / float64(n)
		fitted := 1 - zetaHurwitz(alpha, float64(h.ks[i]+1))/zKmin
		worst = math.Max(worst, math.Abs(emp-fitted))
	}
	return worst
}

type fitResult struct {
	alpha      float64
	kmin       int64
	ks         float64
	tailN      int64
	candidates int
}

// departureK finds where the fitted tail stops tracking the data: the
// smallest degree at which the fitted CCDF exceeds the empirical one by
// 50%. Heavy-tailed real networks almost always steepen eventually
// (finite-size cutoff); reporting where keeps the headline claim inside
// what the figure shows. Returns the largest degree if the fit tracks
// to the end.
func departureK(h hist, res fitResult) int64 {
	first, tailN, _ := h.tailOf(res.kmin)
	anchor := float64(tailN) / float64(h.total)
	zKmin := zetaHurwitz(res.alpha, float64(res.kmin))
	var below int64 // observations with degree < current k, within tail
	for i := first; i < len(h.ks); i++ {
		k := h.ks[i]
		emp := anchor * float64(tailN-below) / float64(tailN) // P(K >= k)
		fitted := anchor * zetaHurwitz(res.alpha, float64(k)) / zKmin
		if fitted > 1.5*emp {
			return k
		}
		below += h.counts[i]
	}
	return h.ks[len(h.ks)-1]
}

// fit scans every distinct degree >= 1 that leaves at least minTail
// tail observations, fits α by MLE at each, and keeps the kmin whose
// fitted tail has the smallest KS distance — the CSN recipe.
func fit(h hist, minTail int64, workers int) fitResult {
	var candidates []int64
	for _, k := range h.ks {
		if k < 1 {
			continue
		}
		if _, n, _ := h.tailOf(k); n < minTail {
			break
		}
		candidates = append(candidates, k)
	}
	if len(candidates) == 0 {
		panic("powerlaw: no kmin candidate leaves enough tail")
	}

	results := make([]fitResult, len(candidates))
	parallelFor(len(candidates), workers, func(j int) {
		kmin := candidates[j]
		first, n, sumLogK := h.tailOf(kmin)
		alpha := mleAlpha(kmin, n, sumLogK)
		results[j] = fitResult{
			alpha: alpha,
			kmin:  kmin,
			ks:    ksDistance(h, first, n, kmin, alpha),
			tailN: n,
		}
	})
	best := results[0]
	for _, r := range results[1:] {
		if r.ks < best.ks {
			best = r
		}
	}
	best.candidates = len(candidates)
	return best
}

// ---- bootstrap goodness of fit ----

// tailSampler draws degrees from the fitted discrete power law by
// inverse transform: a CDF table covers the bulk, and the (heavy) tail
// beyond the table is inverted with a zeta bisection — for α < 2 no
// table can hold enough mass for table-only sampling.
type tailSampler struct {
	kmin  int64
	alpha float64
	zKmin float64
	cdf   []float64 // cdf[i] = P(K <= kmin+i | tail)
}

func newTailSampler(kmin int64, alpha float64) *tailSampler {
	s := &tailSampler{kmin: kmin, alpha: alpha, zKmin: zetaHurwitz(alpha, float64(kmin))}
	const tableLen = 1 << 20
	s.cdf = make([]float64, tableLen)
	// Built by accumulating the pmf P(K = k) = k^-α / ζ(α, kmin); one
	// pow per entry instead of a zeta evaluation each.
	cum := 0.0
	for i := range s.cdf {
		cum += math.Pow(float64(kmin+int64(i)), -alpha) / s.zKmin
		s.cdf[i] = cum
	}
	return s
}

func (s *tailSampler) draw(r *rand.Rand) int64 {
	u := r.Float64()
	if u < s.cdf[len(s.cdf)-1] {
		i := sort.SearchFloat64s(s.cdf, u)
		return s.kmin + int64(i)
	}
	// Rare deep-tail draw: bisect k on P(K > k) = ζ(α, k+1)/ζ(α, kmin).
	lo := s.kmin + int64(len(s.cdf))
	hi := lo
	for zetaHurwitz(s.alpha, float64(hi+1))/s.zKmin > 1-u {
		hi *= 2
		if hi > 1<<50 {
			break
		}
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		if 1-zetaHurwitz(s.alpha, float64(mid+1))/s.zKmin >= u {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// bootstrapP runs the CSN semi-parametric bootstrap: each replicate
// draws h.total observations — from the fitted power law with the
// observed tail probability, otherwise from the empirical body below
// kmin — then refits kmin and α from scratch and records whether its KS
// distance reaches the observed one. The returned p is the fraction
// that did; small p means the data deviate from a power law by more
// than the model's own fluctuations.
func bootstrapP(h hist, res fitResult, reps int, seed uint64, minTail int64, workers int) (p float64, exceed int) {
	first, _, _ := h.tailOf(res.kmin)
	bodyKs := h.ks[:first]
	bodyCum := make([]int64, first)
	var bodyN int64
	for i := range first {
		bodyN += h.counts[i]
		bodyCum[i] = bodyN
	}
	pTail := float64(res.tailN) / float64(h.total)
	sampler := newTailSampler(res.kmin, res.alpha)

	var over atomic.Int64
	parallelFor(reps, workers, func(rep int) {
		r := rand.New(rand.NewPCG(seed, uint64(rep)))
		counts := make(map[int64]int64, len(h.ks))
		for range h.total {
			if r.Float64() < pTail {
				counts[sampler.draw(r)]++
			} else {
				// Empirical body draw, weighted by counts.
				t := r.Int64N(bodyN)
				i := sort.Search(first, func(i int) bool { return bodyCum[i] > t })
				counts[bodyKs[i]]++
			}
		}
		syn := hist{ks: make([]int64, 0, len(counts)), counts: make([]int64, 0, len(counts))}
		for k := range counts {
			syn.ks = append(syn.ks, k)
		}
		slices.Sort(syn.ks)
		for _, k := range syn.ks {
			syn.counts = append(syn.counts, counts[k])
			syn.total += counts[k]
		}
		if fit(syn, minTail, 1).ks >= res.ks {
			over.Add(1)
		}
	})
	exceed = int(over.Load())
	return float64(exceed) / float64(reps), exceed
}

func parallelFor(n, workers int, fn func(i int)) {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers == 1 || n <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(workers, n) {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				fn(i)
			}
		})
	}
	wg.Wait()
}
