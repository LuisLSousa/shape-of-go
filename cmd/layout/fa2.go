package main

import (
	"math"
	"math/rand/v2"
	"sync"
)

// fa2Config carries the ForceAtlas2 tuning knobs (Jacomy et al. 2014).
type fa2Config struct {
	Theta           float64 // Barnes-Hut opening angle
	Scaling         float64 // repulsion strength kr
	Gravity         float64
	StrongGravity   bool
	LinLog          bool
	JitterTolerance float64
	Workers         int
}

// layout holds the mutable state of a ForceAtlas2 run over an
// undirected CSR graph. Mass is degree+1, per the paper.
type layout struct {
	n        int
	adjOff   []int32
	adj      []int32
	mass     []float64
	pos      []float64 // x,y interleaved
	force    []float64
	prev     []float64
	cfg      fa2Config
	qt       *quadtree
	speed    float64
	speedEff float64
	swinging float64
	traction float64
}

func newLayout(n int, adjOff, adj []int32, cfg fa2Config) *layout {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	l := &layout{
		n:      n,
		adjOff: adjOff,
		adj:    adj,
		mass:   make([]float64, n),
		pos:    make([]float64, 2*n),
		force:  make([]float64, 2*n),
		prev:   make([]float64, 2*n),
		cfg:    cfg,
		qt:     newQuadtree(n),
		speed:  1, speedEff: 1,
	}
	for i := range n {
		l.mass[i] = float64(adjOff[i+1]-adjOff[i]) + 1
	}
	return l
}

// initPositions scatters nodes uniformly in a disk whose radius grows
// with sqrt(n), keeping the initial density scale-independent.
func (l *layout) initPositions(seed uint64) {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	radius := math.Sqrt(float64(l.n))
	for i := 0; i < l.n; i++ {
		r := radius * math.Sqrt(rng.Float64())
		a := 2 * math.Pi * rng.Float64()
		l.pos[2*i] = r * math.Cos(a)
		l.pos[2*i+1] = r * math.Sin(a)
	}
}

// step runs one ForceAtlas2 iteration: Barnes-Hut repulsion + gravity +
// edge attraction into l.force (node-parallel, deterministic), then the
// adaptive-speed displacement update from the reference implementation.
func (l *layout) step() {
	l.qt.build(l.pos, l.mass)
	l.force, l.prev = l.prev, l.force
	l.parallelNodes(l.computeForces)

	// Global swinging vs traction decides how bold this step may be.
	var swg, tra float64
	for i := 0; i < l.n; i++ {
		dx, dy := l.force[2*i], l.force[2*i+1]
		ox, oy := l.prev[2*i], l.prev[2*i+1]
		swg += l.mass[i] * math.Hypot(ox-dx, oy-dy)
		tra += l.mass[i] * 0.5 * math.Hypot(ox+dx, oy+dy)
	}
	l.swinging, l.traction = swg, tra

	nf := float64(l.n)
	optJitter := 0.05 * math.Sqrt(nf)
	minJT, maxJT := math.Sqrt(optJitter), 10.0
	jt := l.cfg.JitterTolerance * math.Max(minJT, math.Min(maxJT, optJitter*tra/(nf*nf)))
	const minSpeedEff = 0.05
	if swg/tra > 2 {
		if l.speedEff > minSpeedEff {
			l.speedEff *= 0.5
		}
		jt = math.Max(jt, l.cfg.JitterTolerance)
	}
	targetSpeed := jt * l.speedEff * tra / swg
	if swg > jt*tra {
		if l.speedEff > minSpeedEff {
			l.speedEff *= 0.7
		}
	} else if l.speed < 1000 {
		l.speedEff *= 1.3
	}
	l.speed += math.Min(targetSpeed-l.speed, 0.5*l.speed)

	l.parallelNodes(func(lo, hi int) {
		for i := lo; i < hi; i++ {
			dx, dy := l.force[2*i], l.force[2*i+1]
			ox, oy := l.prev[2*i], l.prev[2*i+1]
			swinging := l.mass[i] * math.Hypot(ox-dx, oy-dy)
			factor := l.speed / (1 + math.Sqrt(l.speed*swinging))
			l.pos[2*i] += dx * factor
			l.pos[2*i+1] += dy * factor
		}
	})
}

// computeForces fills l.force for nodes [lo,hi): Barnes-Hut repulsion,
// gravity toward the origin, and (lin-log) attraction along edges. Each
// node writes only its own force slot, so ranges run in parallel and
// the result is independent of scheduling.
func (l *layout) computeForces(lo, hi int) {
	var stack [128]int32
	kr := l.cfg.Scaling
	kg := l.cfg.Scaling * l.cfg.Gravity
	for i := lo; i < hi; i++ {
		x, y, mi := l.pos[2*i], l.pos[2*i+1], l.mass[i]
		fx, fy := l.qt.repulsion(x, y, mi, kr, l.cfg.Theta, stack[:0])

		if d := math.Hypot(x, y); d > 1e-9 {
			g := kg * mi
			if !l.cfg.StrongGravity {
				g /= d
			}
			fx -= g * x
			fy -= g * y
		}

		for _, j := range l.adj[l.adjOff[i]:l.adjOff[i+1]] {
			dx, dy := l.pos[2*j]-x, l.pos[2*j+1]-y
			if l.cfg.LinLog {
				if d := math.Hypot(dx, dy); d > 1e-9 {
					f := math.Log1p(d) / d
					dx, dy = dx*f, dy*f
				}
			}
			fx += dx
			fy += dy
		}
		l.force[2*i] = fx
		l.force[2*i+1] = fy
	}
}

func (l *layout) parallelNodes(fn func(lo, hi int)) {
	w := l.cfg.Workers
	if w == 1 || l.n < 4096 {
		fn(0, l.n)
		return
	}
	var wg sync.WaitGroup
	chunk := (l.n + w - 1) / w
	for lo := 0; lo < l.n; lo += chunk {
		hi := min(lo+chunk, l.n)
		wg.Go(func() {
			fn(lo, hi)
		})
	}
	wg.Wait()
}

// radius reports the root-mean-square distance from the origin, a
// cheap convergence signal for the progress log.
func (l *layout) radius() float64 {
	var s float64
	for i := 0; i < l.n; i++ {
		s += l.pos[2*i]*l.pos[2*i] + l.pos[2*i+1]*l.pos[2*i+1]
	}
	return math.Sqrt(s / float64(l.n))
}
