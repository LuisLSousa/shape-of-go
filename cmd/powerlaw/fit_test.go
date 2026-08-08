package main

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestZetaKnownValues(t *testing.T) {
	cases := []struct {
		s, q, want float64
	}{
		{2, 1, math.Pi * math.Pi / 6},
		{4, 1, math.Pow(math.Pi, 4) / 90},
		{2, 2, math.Pi*math.Pi/6 - 1},
	}
	for _, c := range cases {
		if got := zetaHurwitz(c.s, c.q); math.Abs(got-c.want) > 1e-10 {
			t.Errorf("zeta(%g, %g) = %.12f, want %.12f", c.s, c.q, got, c.want)
		}
	}
	// Against brute-force summation for a non-integer case.
	var brute float64
	for i := range 5_000_000 {
		brute += math.Pow(7+float64(i), -2.35)
	}
	if got := zetaHurwitz(2.35, 7); math.Abs(got-brute) > 1e-8 {
		t.Errorf("zeta(2.35, 7) = %.12f, brute force %.12f", got, brute)
	}
}

// synthetic builds a histogram of n draws from a discrete power law
// with the given alpha and kmin, using the production sampler.
func synthetic(n int, alpha float64, kmin int64, seed uint64) hist {
	s := newTailSampler(kmin, alpha)
	r := rand.New(rand.NewPCG(seed, seed))
	counts := map[int64]int64{}
	for range n {
		counts[s.draw(r)]++
	}
	var h hist
	for k := int64(0); len(counts) > 0; k++ {
		if c, ok := counts[k]; ok {
			h.ks = append(h.ks, k)
			h.counts = append(h.counts, c)
			h.total += c
			delete(counts, k)
		}
		if k > 1<<40 {
			break
		}
	}
	return h
}

func TestMLERecoversAlpha(t *testing.T) {
	const alpha, kmin = 2.5, 4
	h := synthetic(200_000, alpha, kmin, 11)
	_, n, sumLogK := h.tailOf(kmin)
	got := mleAlpha(kmin, n, sumLogK)
	if math.Abs(got-alpha) > 0.02 {
		t.Fatalf("MLE alpha = %.4f, want %.4f ± 0.02", got, alpha)
	}
}

func TestFitStaysOutOfBody(t *testing.T) {
	// Power-law tail from kmin=8 under a FLAT body — a shape no power
	// law can imitate. Any kmin >= 8 is a correct fit (a power-law tail
	// from 8 is also one from any larger cutoff, with the same α), and
	// the CSN kmin estimator is known to spread upward on finite
	// samples. The failures worth catching are kmin dipping INTO the
	// body, a runaway kmin, or a wrong α.
	const alpha, kmin = 2.2, 8
	for _, seed := range []uint64{3, 7, 11} {
		h := synthetic(150_000, alpha, kmin, seed)
		bh := hist{}
		for k := int64(1); k <= 7; k++ {
			bh.ks = append(bh.ks, k)
			bh.counts = append(bh.counts, 20_000)
			bh.total += 20_000
		}
		bh.ks = append(bh.ks, h.ks...)
		bh.counts = append(bh.counts, h.counts...)
		bh.total += h.total

		res := fit(bh, 50, 1)
		if res.kmin < 8 {
			t.Fatalf("seed %d: kmin = %d reached into the non-power-law body", seed, res.kmin)
		}
		if res.kmin > 40 {
			t.Fatalf("seed %d: kmin = %d ran away from the true cutoff 8", seed, res.kmin)
		}
		if math.Abs(res.alpha-alpha) > 0.12 {
			t.Fatalf("seed %d: alpha = %.4f at kmin %d, want %.4f ± 0.12", seed, res.alpha, res.kmin, alpha)
		}
	}
}

func TestTailSamplerMatchesCDF(t *testing.T) {
	// Empirical frequencies from the sampler must match the analytic
	// pmf at the low end, where nearly all the mass lives.
	const alpha, kmin = 1.7, 10
	s := newTailSampler(kmin, alpha)
	r := rand.New(rand.NewPCG(3, 3))
	const n = 500_000
	var atKmin, beyondTable int
	for range n {
		k := s.draw(r)
		if k == kmin {
			atKmin++
		}
		if k >= kmin+int64(len(s.cdf)) {
			beyondTable++
		}
	}
	want := math.Pow(kmin, -alpha) / zetaHurwitz(alpha, kmin)
	got := float64(atKmin) / n
	if math.Abs(got-want) > 0.005 {
		t.Fatalf("P(K = kmin): sampled %.4f, analytic %.4f", got, want)
	}
	// alpha = 1.7 is heavy: the deep-tail bisection path must actually
	// be exercised, or the sampler silently truncates the tail.
	if beyondTable == 0 {
		t.Fatal("no draws beyond the CDF table; deep-tail path untested")
	}
}

func TestCCDFBasics(t *testing.T) {
	h := hist{ks: []int64{0, 1, 3, 10}, counts: []int64{5, 3, 1, 1}, total: 10}
	ks, p := ccdf(h)
	if len(ks) != 3 || ks[0] != 1 || p[0] != 0.5 {
		t.Fatalf("ccdf head wrong: ks=%v p=%v", ks, p)
	}
	if p[2] != 0.1 || ks[2] != 10 {
		t.Fatalf("ccdf tail wrong: ks=%v p=%v", ks, p)
	}
}
