package main

import (
	"math"
	"math/rand/v2"
	"testing"
)

// brute-force repulsion for validating the Barnes-Hut approximation.
func bruteRepulsion(pos, mass []float64, i int, kr float64) (fx, fy float64) {
	x, y := pos[2*i], pos[2*i+1]
	for j := range mass {
		if j == i {
			continue
		}
		dx, dy := x-pos[2*j], y-pos[2*j+1]
		d2 := dx*dx + dy*dy
		if d2 < 1e-12 {
			continue
		}
		f := kr * mass[i] * mass[j] / d2
		fx += dx * f
		fy += dy * f
	}
	return fx, fy
}

func randomPoints(n int, seed uint64) (pos, mass []float64) {
	rng := rand.New(rand.NewPCG(seed, seed))
	pos = make([]float64, 2*n)
	mass = make([]float64, n)
	for i := range n {
		pos[2*i] = rng.Float64()*200 - 100
		pos[2*i+1] = rng.Float64()*200 - 100
		mass[i] = 1 + rng.Float64()*9
	}
	return pos, mass
}

func TestQuadtreeExactWhenThetaZero(t *testing.T) {
	pos, mass := randomPoints(400, 1)
	qt := newQuadtree(400)
	qt.build(pos, mass)
	for i := 0; i < 400; i += 7 {
		wantX, wantY := bruteRepulsion(pos, mass, i, 10)
		gotX, gotY := qt.repulsion(pos[2*i], pos[2*i+1], mass[i], 10, 0, nil)
		if relErr(gotX, wantX) > 1e-9 || relErr(gotY, wantY) > 1e-9 {
			t.Fatalf("node %d: got (%g,%g) want (%g,%g)", i, gotX, gotY, wantX, wantY)
		}
	}
}

func TestQuadtreeApproximation(t *testing.T) {
	pos, mass := randomPoints(2000, 2)
	qt := newQuadtree(2000)
	qt.build(pos, mass)
	var worst float64
	for i := 0; i < 2000; i += 13 {
		wantX, wantY := bruteRepulsion(pos, mass, i, 10)
		gotX, gotY := qt.repulsion(pos[2*i], pos[2*i+1], mass[i], 10, 1.2, nil)
		errMag := math.Hypot(gotX-wantX, gotY-wantY) / (math.Hypot(wantX, wantY) + 1e-12)
		worst = math.Max(worst, errMag)
	}
	// theta=1.2 is coarse by design; force direction and rough
	// magnitude must survive, not exact values.
	if worst > 0.25 {
		t.Fatalf("worst relative force error %.3f, want <= 0.25", worst)
	}
}

func TestQuadtreeCoincidentPoints(t *testing.T) {
	// Many points at the same spot must neither loop forever nor
	// produce non-finite forces.
	pos := make([]float64, 0, 40)
	mass := make([]float64, 0, 20)
	for range 20 {
		pos = append(pos, 5, 5)
		mass = append(mass, 1)
	}
	pos = append(pos, 6, 6)
	mass = append(mass, 1)
	qt := newQuadtree(len(mass))
	qt.build(pos, mass)
	fx, fy := qt.repulsion(6, 6, 1, 10, 1.2, nil)
	if math.IsNaN(fx) || math.IsInf(fx, 0) || math.IsNaN(fy) || math.IsInf(fy, 0) {
		t.Fatalf("non-finite force (%g, %g)", fx, fy)
	}
	if fx <= 0 || fy <= 0 {
		t.Fatalf("force should push away from the clump, got (%g, %g)", fx, fy)
	}
}

func TestLayoutDeterminism(t *testing.T) {
	adjOff, adj := ringCSR(500)
	run := func() []float64 {
		l := newLayout(500, adjOff, adj, fa2Config{
			Theta: 1.2, Scaling: 10, Gravity: 1, JitterTolerance: 1, Workers: 8,
		})
		l.initPositions(7)
		for range 30 {
			l.step()
		}
		return l.pos
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("position %d differs across identical runs: %g vs %g", i, a[i], b[i])
		}
	}
}

func TestPairEquilibrium(t *testing.T) {
	// Two connected nodes with no gravity settle where linear
	// attraction d balances repulsion kr*m^2/d: d = m*sqrt(kr).
	adjOff := []int32{0, 1, 2}
	adj := []int32{1, 0}
	kr := 10.0
	l := newLayout(2, adjOff, adj, fa2Config{
		Theta: 0, Scaling: kr, Gravity: 0, JitterTolerance: 1, Workers: 1,
	})
	l.initPositions(3)
	for range 2000 {
		l.step()
	}
	d := math.Hypot(l.pos[0]-l.pos[2], l.pos[1]-l.pos[3])
	want := 2 * math.Sqrt(kr) // mass = deg+1 = 2 for both
	if math.Abs(d-want)/want > 0.05 {
		t.Fatalf("equilibrium distance %.3f, want %.3f ± 5%%", d, want)
	}
}

func TestGiantComponentAndReindex(t *testing.T) {
	// 0-1-2 triangle (giant), 3-4 pair, 5 isolated.
	from := []int32{0, 1, 2, 3}
	to := []int32{1, 2, 0, 4}
	keep := giantComponent(6, from, to)
	want := []bool{true, true, true, false, false, false}
	for i := range want {
		if keep[i] != want[i] {
			t.Fatalf("keep[%d] = %v, want %v", i, keep[i], want[i])
		}
	}
	kept, keptIdx := reindex(keep)
	if len(kept) != 3 || keptIdx[3] != -1 || keptIdx[2] != 2 {
		t.Fatalf("reindex wrong: kept=%v keptIdx=%v", kept, keptIdx)
	}
	adjOff, adj := subgraphCSR(kept, keptIdx, from, to)
	if adjOff[3] != 6 || len(adj) != 6 {
		t.Fatalf("triangle should have 6 CSR entries, got %v %v", adjOff, adj)
	}
}

func ringCSR(n int) (off, adj []int32) {
	off = make([]int32, n+1)
	adj = make([]int32, 2*n)
	for i := range n {
		off[i+1] = int32(2 * (i + 1))
		adj[2*i] = int32((i + n - 1) % n)
		adj[2*i+1] = int32((i + 1) % n)
	}
	return off, adj
}

func relErr(got, want float64) float64 {
	return math.Abs(got-want) / (math.Abs(want) + 1e-12)
}
