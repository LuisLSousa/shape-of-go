package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
)

func countNodes(path string) int {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		n++
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	return n
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

// giantComponent marks the members of the largest weakly-connected
// component via union-find with path halving.
func giantComponent(n int, from, to []int32) []bool {
	parent := make([]int32, n)
	for i := range parent {
		parent[i] = int32(i)
	}
	find := func(x int32) int32 {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
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
	var giantRoot int32
	giant := 0
	for root, s := range size {
		if s > giant || (s == giant && root < giantRoot) {
			giant, giantRoot = s, root
		}
	}
	keep := make([]bool, n)
	for i := range keep {
		keep[i] = find(int32(i)) == giantRoot
	}
	return keep
}

// reindex maps kept original ids to dense indices. kept[k] is the
// original id of dense index k; keptIdx[orig] is the dense index or -1.
func reindex(keep []bool) (kept []int32, keptIdx []int32) {
	keptIdx = make([]int32, len(keep))
	for i, k := range keep {
		if k {
			keptIdx[i] = int32(len(kept))
			kept = append(kept, int32(i))
		} else {
			keptIdx[i] = -1
		}
	}
	return kept, keptIdx
}

// subgraphCSR builds the undirected adjacency of the kept subgraph in
// compressed sparse row form; each surviving edge appears in both
// endpoints' neighbor lists.
func subgraphCSR(kept, keptIdx, from, to []int32) (off []int32, adj []int32) {
	n := len(kept)
	deg := make([]int32, n)
	for i := range from {
		u, v := keptIdx[from[i]], keptIdx[to[i]]
		if u < 0 || v < 0 {
			continue
		}
		deg[u]++
		deg[v]++
	}
	off = make([]int32, n+1)
	for i := range n {
		off[i+1] = off[i] + deg[i]
	}
	adj = make([]int32, off[n])
	cursor := make([]int32, n)
	copy(cursor, off[:n])
	for i := range from {
		u, v := keptIdx[from[i]], keptIdx[to[i]]
		if u < 0 || v < 0 {
			continue
		}
		adj[cursor[u]] = v
		cursor[u]++
		adj[cursor[v]] = u
		cursor[v]++
	}
	return off, adj
}

func writeKept(path string, kept []int32) {
	var b strings.Builder
	b.Grow(len(kept) * 8)
	for _, orig := range kept {
		fmt.Fprintf(&b, "%d\n", orig)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
}

// writePositions dumps positions as consecutive x,y float32 pairs,
// little-endian, via a temp file and atomic rename so a reader (or a
// crash) never sees a half-written checkpoint.
func (l *layout) writePositions(path string) error {
	buf := make([]byte, len(l.pos)*4)
	for i, p := range l.pos {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(float32(p)))
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (l *layout) loadPositions(path string) error {
	buf, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(buf) != len(l.pos)*4 {
		return fmt.Errorf("positions.bin has %d values, want %d (node filter changed?)", len(buf)/4, len(l.pos))
	}
	for i := range l.pos {
		l.pos[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:])))
	}
	return nil
}
