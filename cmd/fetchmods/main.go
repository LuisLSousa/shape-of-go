// Command fetchmods resolves the latest version of every module in the
// synced index and downloads its go.mod from proxy.golang.org,
// resumably.
//
// Pass 1 streams data/index.jsonl and keeps, per module path, the
// highest semantic version seen (golang.org/x/mod/semver ordering, so
// releases beat pre-releases and pseudo-versions sort correctly).
// Pass 2 fans the not-yet-fetched modules across concurrent workers;
// each worker appends {path, version, mod | error} records to its own
// shard file, so there is no write contention. Resume works by
// re-reading the shards on startup: any path already recorded (fetched
// OR permanently failed) is skipped. Transient failures retry with
// backoff a bounded number of times, then are recorded as errors;
// re-run with -retry-errors to give those another chance later.
//
// Like indexsync, the guarantee is at-least-once with downstream
// dedupe: a crash can at most re-fetch records that were buffered but
// not yet flushed.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const proxyURL = "https://proxy.golang.org"

type indexRecord struct {
	Path    string `json:"Path"`
	Version string `json:"Version"`
}

// shardRecord is one line in a shard file: a fetched go.mod, or a
// permanent error for this module.
type shardRecord struct {
	Path    string `json:"p"`
	Version string `json:"v"`
	Mod     string `json:"mod,omitempty"`
	Err     string `json:"err,omitempty"`
}

func main() {
	var (
		index       = flag.String("index", "data/index.jsonl", "synced module index (JSONL)")
		outDir      = flag.String("out", "data/mods", "shard output directory")
		workers     = flag.Int("workers", 16, "concurrent fetchers")
		rate        = flag.Int("rate", 120, "global request rate cap per second (politeness: ~740/s tripped proxy abuse protection)")
		maxMods     = flag.Int("max", 0, "fetch at most this many modules (0 = all; for testing)")
		retryErrors = flag.Bool("retry-errors", false, "re-attempt modules previously recorded as errors")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	unlock := lock(filepath.Join(*outDir, ".lock"))
	defer unlock()

	latest := scanIndex(*index)
	log.Printf("index: %d unique module paths", len(latest))

	done := scanShards(*outDir, *retryErrors)
	log.Printf("already recorded: %d", len(done))

	work := make([]string, 0, len(latest))
	for p := range latest {
		if !done[p] {
			work = append(work, p)
		}
	}
	sort.Strings(work)
	if *maxMods > 0 && len(work) > *maxMods {
		work = work[:*maxMods]
	}
	log.Printf("to fetch: %d modules on %d workers", len(work), *workers)
	if len(work) == 0 {
		log.Printf("done: nothing to do")
		return
	}

	startTokens(*rate)
	var fetched, failed atomic.Int64
	start := time.Now()
	jobs := make(chan string)
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 60 * time.Second}
	for i := range *workers {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			worker(shard, *outDir, client, jobs, &fetched, &failed)
		}(i)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		var lastTotal int64
		lastTick := start
		for now := range t.C {
			f, e := fetched.Load(), failed.Load()
			winRate := float64(f+e-lastTotal) / now.Sub(lastTick).Seconds()
			lastTotal, lastTick = f+e, now
			rem := time.Duration(float64(len(work)-int(f+e))/max(winRate, 1)) * time.Second
			log.Printf("%d fetched, %d errors, %.0f/s (30s window), ~%s remaining",
				f, e, winRate, rem.Round(time.Minute))
		}
	}()
	for _, p := range work {
		jobs <- p
	}
	close(jobs)
	wg.Wait()
	log.Printf("done: %d fetched, %d errors, in %s",
		fetched.Load(), failed.Load(), time.Since(start).Round(time.Second))
}

// latestByPath holds the version chosen for each module path; read-only
// after scanIndex, so workers share it without locks.
var latestByPath map[string]string

// scanIndex streams the index and keeps the highest semver per path.
func scanIndex(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("open index (run indexsync first): %v", err)
	}
	defer f.Close()

	latestByPath = make(map[string]string, 4<<20)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lines := 0
	for sc.Scan() {
		lines++
		var r indexRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if r.Path == "golang.org/toolchain" { // toolchain blobs, not a library
			continue
		}
		if cur, ok := latestByPath[r.Path]; !ok || semver.Compare(r.Version, cur) > 0 {
			latestByPath[r.Path] = r.Version
		}
		if lines%10_000_000 == 0 {
			log.Printf("index scan: %dM lines, %d paths", lines/1_000_000, len(latestByPath))
		}
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
	return latestByPath
}

// scanShards rebuilds the set of already-recorded paths from the shard
// files. With retryErrors, permanently-failed records don't count as
// done and will be attempted again.
func scanShards(dir string, retryErrors bool) map[string]bool {
	done := make(map[string]bool)
	matches, _ := filepath.Glob(filepath.Join(dir, "shard-*.jsonl"))
	for _, m := range matches {
		f, err := os.Open(m)
		if err != nil {
			log.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var r shardRecord
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				continue // torn tail line from a crash: refetched, deduped downstream
			}
			if retryErrors && r.Err != "" {
				continue
			}
			done[r.Path] = true
		}
		if err := sc.Err(); err != nil {
			// A truncated shard tail (crash mid-write) is recoverable —
			// affected modules just refetch — but surface it.
			log.Printf("warning: reading %s: %v", m, err)
		}
		f.Close()
	}
	return done
}

func worker(shard int, dir string, client *http.Client, jobs <-chan string, fetched, failed *atomic.Int64) {
	name := filepath.Join(dir, fmt.Sprintf("shard-%03d.jsonl", shard))
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 256<<10)
	defer w.Flush()
	enc := json.NewEncoder(w)

	sinceFlush := 0
	for p := range jobs {
		rec := fetchOne(client, p, latestByPath[p])
		if rec.Err == "" {
			fetched.Add(1)
		} else {
			failed.Add(1)
		}
		if err := enc.Encode(rec); err != nil {
			log.Fatal(err)
		}
		if sinceFlush++; sinceFlush >= 50 {
			sinceFlush = 0
			if err := w.Flush(); err != nil {
				log.Fatal(err)
			}
		}
	}
}

// tokens is the global rate limiter: every request takes one token,
// refilled at the -rate cap. Staying well under the proxy's abuse
// threshold beats reacting to it after the fact.
var tokens chan struct{}

func startTokens(perSecond int) {
	tokens = make(chan struct{}, perSecond/4+1)
	go func() {
		t := time.NewTicker(time.Second / time.Duration(perSecond))
		defer t.Stop()
		for range t.C {
			select {
			case tokens <- struct{}{}:
			default: // bucket full: don't bank unbounded burst
			}
		}
	}()
}

// Storm breaker: scattered 403s are per-module legal blocks and only
// cost that module, but a long run of consecutive 403s with no success
// in between means the proxy is refusing *us* — then the whole fleet
// pauses. A single blocked module can never trip this, because every
// success resets the streak.
var (
	consec403   atomic.Int64
	pausedUntil atomic.Int64 // unix nanoseconds
)

func note403() {
	if consec403.Add(1) >= 30 {
		consec403.Store(0)
		target := time.Now().Add(2 * time.Minute).UnixNano()
		for {
			cur := pausedUntil.Load()
			if cur >= target || pausedUntil.CompareAndSwap(cur, target) {
				break
			}
		}
		log.Printf("403 storm detected: pausing all workers 2m")
	}
}

func waitIfPaused() {
	for {
		d := time.Until(time.Unix(0, pausedUntil.Load()))
		if d <= 0 {
			return
		}
		time.Sleep(min(d, time.Second))
	}
}

// fetchOne downloads one go.mod. Gone/NotFound are permanent; 403 is
// recorded after 3 patient tries (some modules are legally blocked and
// will never succeed); other failures retry with backoff up to 8
// attempts before being recorded as errors (re-run with -retry-errors
// to try again).
func fetchOne(client *http.Client, path, version string) shardRecord {
	rec := shardRecord{Path: path, Version: version}
	ep, err1 := module.EscapePath(path)
	ev, err2 := module.EscapeVersion(version)
	if err1 != nil || err2 != nil {
		rec.Err = "unescapable path or version"
		return rec
	}
	u := fmt.Sprintf("%s/%s/@v/%s.mod", proxyURL, ep, ev)

	backoff := time.Second
	forbidden := 0
	for attempt := 1; ; attempt++ {
		waitIfPaused()
		<-tokens
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("User-Agent", "shape-of-go/0.1 (+https://github.com/LuisLSousa/shape-of-go)")
		resp, err := client.Do(req)
		if err == nil {
			switch resp.StatusCode {
			case http.StatusOK:
				consec403.Store(0)
				body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
				resp.Body.Close()
				if readErr == nil {
					rec.Mod = string(body)
					return rec
				}
				err = readErr
			case http.StatusNotFound, http.StatusGone:
				consec403.Store(0)
				resp.Body.Close()
				rec.Err = resp.Status // permanent: removed or never valid
				return rec
			case http.StatusForbidden:
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				note403()
				if forbidden++; forbidden >= 3 {
					rec.Err = "403 Forbidden"
					return rec
				}
				time.Sleep(5 * time.Second)
				continue
			default:
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				err = fmt.Errorf("status %s", resp.Status)
			}
		}
		if attempt >= 8 {
			rec.Err = fmt.Sprintf("gave up after %d attempts: %v", attempt, err)
			return rec
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, time.Minute)
	}
}

// lock takes an exclusive flock, refusing a second concurrent run.
func lock(path string) (unlock func()) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatalf("another fetchmods is already running against %s", filepath.Dir(path))
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
