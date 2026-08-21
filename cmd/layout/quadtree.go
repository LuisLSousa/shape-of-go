package main

import "math"

// quadtree is a Barnes-Hut tree over the current node positions,
// rebuilt every iteration. Cells are stored in flat slices that are
// reused across builds to keep the steady state allocation-free.
//
// Every cell tracks its center of mass; a leaf holds one point, except
// past maxDepth where coincident points merge into one aggregate.
type quadtree struct {
	// cell geometry: center and half-width of the square region
	cx, cy, half []float64
	// aggregate mass and center of mass
	mass, comX, comY []float64
	// children[4c..4c+4]; -1 = empty. leaf[c] marks single-point cells.
	children []int32
	leaf     []bool
	count    int
}

const maxDepth = 64

func newQuadtree(n int) *quadtree {
	cap := 2*n + 16
	return &quadtree{
		cx: make([]float64, 0, cap), cy: make([]float64, 0, cap),
		half: make([]float64, 0, cap), mass: make([]float64, 0, cap),
		comX: make([]float64, 0, cap), comY: make([]float64, 0, cap),
		children: make([]int32, 0, 4*cap), leaf: make([]bool, 0, cap),
	}
}

func (q *quadtree) newCell(cx, cy, half float64) int32 {
	q.cx = append(q.cx, cx)
	q.cy = append(q.cy, cy)
	q.half = append(q.half, half)
	q.mass = append(q.mass, 0)
	q.comX = append(q.comX, 0)
	q.comY = append(q.comY, 0)
	q.children = append(q.children, -1, -1, -1, -1)
	q.leaf = append(q.leaf, false)
	q.count++
	return int32(q.count - 1)
}

func (q *quadtree) reset() {
	q.cx, q.cy, q.half = q.cx[:0], q.cy[:0], q.half[:0]
	q.mass, q.comX, q.comY = q.mass[:0], q.comX[:0], q.comY[:0]
	q.children, q.leaf = q.children[:0], q.leaf[:0]
	q.count = 0
}

// build inserts every point, accumulating mass-weighted centers along
// the way so no separate summarize pass is needed.
func (q *quadtree) build(pos, mass []float64) {
	q.reset()
	n := len(mass)
	if n == 0 {
		return
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for i := range n {
		minX = math.Min(minX, pos[2*i])
		maxX = math.Max(maxX, pos[2*i])
		minY = math.Min(minY, pos[2*i+1])
		maxY = math.Max(maxY, pos[2*i+1])
	}
	half := math.Max(maxX-minX, maxY-minY)/2 + 1e-9
	root := q.newCell((minX+maxX)/2, (minY+maxY)/2, half)
	for i := range n {
		q.insert(root, pos[2*i], pos[2*i+1], mass[i], 0)
	}
}

func (q *quadtree) insert(c int32, x, y, m float64, depth int) {
	for {
		// Accumulate the aggregate on the way down.
		tm := q.mass[c] + m
		q.comX[c] = (q.comX[c]*q.mass[c] + x*m) / tm
		q.comY[c] = (q.comY[c]*q.mass[c] + y*m) / tm
		q.mass[c] = tm

		if q.mass[c] == m { // first point in this cell
			q.leaf[c] = true
			return
		}
		if q.leaf[c] {
			if depth >= maxDepth {
				return // merge coincident points into the aggregate
			}
			// Push the resident point one level down, then continue
			// inserting the new point from this cell.
			px, py := q.comX[c], q.comY[c]
			pm := q.mass[c]
			// Undo the aggregate: resident is the pre-insert com/mass.
			rm := pm - m
			rx := (px*pm - x*m) / rm
			ry := (py*pm - y*m) / rm
			q.leaf[c] = false
			child := q.childFor(c, rx, ry)
			q.insert(child, rx, ry, rm, depth+1)
		}
		c = q.childFor(c, x, y)
		depth++
	}
}

// childFor returns (creating if needed) the child quadrant of c that
// contains (x, y).
func (q *quadtree) childFor(c int32, x, y float64) int32 {
	quadrant := int32(0)
	if x > q.cx[c] {
		quadrant |= 1
	}
	if y > q.cy[c] {
		quadrant |= 2
	}
	slot := 4*c + quadrant
	if q.children[slot] >= 0 {
		return q.children[slot]
	}
	h := q.half[c] / 2
	ccx, ccy := q.cx[c]-h, q.cy[c]-h
	if quadrant&1 != 0 {
		ccx = q.cx[c] + h
	}
	if quadrant&2 != 0 {
		ccy = q.cy[c] + h
	}
	child := q.newCell(ccx, ccy, h)
	q.children[slot] = child
	return child
}

// repulsion accumulates the Barnes-Hut approximated repulsion force on
// a probe of mass m at (x, y): F = kr*m*m_cell/d per interaction. A
// cell is taken whole when its width over the distance to its center
// of mass is below theta. The caller provides a reusable stack.
func (q *quadtree) repulsion(x, y, m, kr, theta float64, stack []int32) (fx, fy float64) {
	if q.count == 0 {
		return 0, 0
	}
	theta2 := theta * theta
	stack = append(stack, 0)
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		dx, dy := x-q.comX[c], y-q.comY[c]
		d2 := dx*dx + dy*dy
		width2 := 4 * q.half[c] * q.half[c]
		if q.leaf[c] || width2 < theta2*d2 {
			// Skip self-interaction and coincident aggregates: the
			// probe's own leaf sits at d~0, where the direction is
			// undefined and the magnitude diverges. Distinct points
			// that are exactly coincident exert no force and travel
			// together, a zero-probability event under the
			// continuous random init.
			if d2 < 1e-12 {
				continue
			}
			f := kr * m * q.mass[c] / d2
			fx += dx * f
			fy += dy * f
			continue
		}
		for k := range int32(4) {
			if ch := q.children[4*c+k]; ch >= 0 {
				stack = append(stack, ch)
			}
		}
	}
	return fx, fy
}
