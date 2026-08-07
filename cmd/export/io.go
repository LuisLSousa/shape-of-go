package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

func loadKept(path string) []int32 {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	var kept []int32
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		v, err := strconv.Atoi(sc.Text())
		if err != nil {
			log.Fatalf("kept.tsv: %v", err)
		}
		kept = append(kept, int32(v))
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	return kept
}

// loadKeptPaths streams nodes.tsv (line number = original id) and keeps
// the paths of kept nodes, in kept order. Returns the total node count.
func loadKeptPaths(path string, kept []int32) (nOrig int, paths []string) {
	wantKept := make(map[int32]int32, len(kept))
	for k, orig := range kept {
		wantKept[orig] = int32(k)
	}
	paths = make([]string, len(kept))
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var line int32
	found := 0
	for sc.Scan() {
		if k, ok := wantKept[line]; ok {
			parts := strings.SplitN(sc.Text(), "\t", 3)
			if len(parts) < 2 {
				log.Fatalf("nodes.tsv line %d: malformed", line)
			}
			paths[k] = parts[1]
			found++
		}
		line++
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	if found != len(kept) {
		log.Fatalf("kept.tsv references %d nodes but nodes.tsv matched %d", len(kept), found)
	}
	return int(line), paths
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

// firstSeenYears returns each kept node's first-seen year as an offset
// from 2019 (255 = unknown). The index feed is timestamp-ordered, so a
// path's first record is its first appearance; the scan result is
// cached as a TSV so only the first export pays for it.
func firstSeenYears(indexPath, cachePath string, paths []string) []uint8 {
	years := make([]uint8, len(paths))
	for i := range years {
		years[i] = 255
	}
	byPath := make(map[string]int32, len(paths))
	for i, p := range paths {
		byPath[p] = int32(i)
	}

	if f, err := os.Open(cachePath); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		hits := 0
		for sc.Scan() {
			p, ys, ok := strings.Cut(sc.Text(), "\t")
			if !ok {
				continue
			}
			if i, present := byPath[p]; present {
				y, err := strconv.Atoi(ys)
				if err != nil {
					continue
				}
				years[i] = yearOffset(y)
				hits++
			}
		}
		if err := sc.Err(); err != nil {
			log.Fatal(err)
		}
		log.Printf("first-seen cache: %d of %d kept nodes", hits, len(paths))
		return years
	}

	log.Printf("scanning %s for first-seen years (cache miss, ~1 min)", indexPath)
	start := time.Now()
	f, err := os.Open(indexPath)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	firstSeen := make(map[string]int, 1<<21)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lines := 0
	for sc.Scan() {
		lines++
		line := sc.Text()
		p, rest, ok := extractPathAndRest(line)
		if !ok {
			continue
		}
		if _, seen := firstSeen[p]; seen {
			continue
		}
		y, ok := extractYear(rest)
		if !ok {
			continue
		}
		firstSeen[p] = y
		if len(firstSeen)%500_000 == 0 {
			log.Printf("%dM index lines, %dk first-seen paths", lines/1_000_000, len(firstSeen)/1000)
		}
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}

	var b strings.Builder
	for p, y := range firstSeen {
		fmt.Fprintf(&b, "%s\t%d\n", p, y)
	}
	if err := os.WriteFile(cachePath, []byte(b.String()), 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("first-seen scan: %d lines, %d paths in %s (cached to %s)",
		lines, len(firstSeen), time.Since(start).Round(time.Second), cachePath)

	hits := 0
	for p, y := range firstSeen {
		if i, present := byPath[p]; present {
			years[i] = yearOffset(y)
			hits++
		}
	}
	log.Printf("first-seen years matched %d of %d kept nodes", hits, len(paths))
	return years
}

// extractPathAndRest pulls the Path value out of an index feed line
// like {"Path":"...","Version":"...","Timestamp":"2019-04-10T..."}
// without a full JSON decode; module paths cannot contain quotes.
func extractPathAndRest(line string) (path, rest string, ok bool) {
	const key = `"Path":"`
	i := strings.Index(line, key)
	if i < 0 {
		return "", "", false
	}
	rest = line[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return "", "", false
	}
	return rest[:j], rest[j:], true
}

func extractYear(rest string) (int, bool) {
	const key = `"Timestamp":"`
	i := strings.Index(rest, key)
	if i < 0 || i+len(key)+4 > len(rest) {
		return 0, false
	}
	y, err := strconv.Atoi(rest[i+len(key) : i+len(key)+4])
	if err != nil {
		return 0, false
	}
	return y, true
}

func yearOffset(y int) uint8 {
	if y < 2019 || y > 2019+250 {
		return 255
	}
	return uint8(y - 2019)
}
