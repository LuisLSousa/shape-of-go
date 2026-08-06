// Command indexsync streams the complete Go module index
// (index.golang.org) into a local JSONL file, resumably.
//
// The index is an append-only feed paginated by timestamp. After every
// page the checkpoint file is atomically rewritten with the boundary
// timestamp (and the record keys seen at that exact timestamp, to
// de-duplicate the inclusive `since` boundary). Interrupt the process
// at any point — Ctrl-C, network drop, laptop sleep — and re-running
// resumes where it left off. Transient network failures never kill the
// process: requests retry forever with capped exponential backoff, so a
// machine that sleeps mid-page simply stalls and recovers on wake.
//
// Guarantees are at-least-once: a crash between appending a page and
// writing the checkpoint can duplicate a page on resume. Downstream
// stages de-duplicate by module@version, so this is harmless.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const indexURL = "https://index.golang.org/index"

type record struct {
	Path      string `json:"Path"`
	Version   string `json:"Version"`
	Timestamp string `json:"Timestamp"`
}

func (r record) key() string { return r.Path + "@" + r.Version }

// checkpoint marks how far the sync has progressed: the timestamp to
// resume from and the keys already written at exactly that timestamp
// (the API's `since` is inclusive, so these would otherwise duplicate).
type checkpoint struct {
	Since    string   `json:"since"`
	Boundary []string `json:"boundary"`
	Lines    int64    `json:"lines"`
}

func main() {
	var (
		out    = flag.String("out", "data/index.jsonl", "output JSONL path (appended)")
		cpPath = flag.String("checkpoint", "data/index.checkpoint", "checkpoint file")
		limit  = flag.Int("limit", 2000, "records per page (API max 2000)")
	)
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	unlock := lock(*out + ".lock")
	defer unlock()
	cp := loadCheckpoint(*cpPath)
	if cp.Since != "" {
		log.Printf("resuming from %s (%d lines so far)", cp.Since, cp.Lines)
	}

	f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)

	client := &http.Client{Timeout: 60 * time.Second}
	boundary := make(map[string]bool, len(cp.Boundary))
	for _, k := range cp.Boundary {
		boundary[k] = true
	}

	start := time.Now()
	pages := 0
	for {
		raw := fetchPage(client, cp.Since, *limit)
		recs := parse(raw)
		if len(recs) == 0 {
			break
		}

		kept := 0
		for _, r := range recs {
			if r.Timestamp == cp.Since && boundary[r.key()] {
				continue // inclusive-boundary duplicate
			}
			fmt.Fprintln(w, r.raw)
			kept++
		}
		cp.Lines += int64(kept)

		// Advance the boundary. If the timestamp moved, start a fresh
		// key set; if not (rare: a full page sharing one timestamp),
		// grow it so progress is still made.
		last := recs[len(recs)-1].Timestamp
		if last != cp.Since {
			boundary = make(map[string]bool)
		}
		for i := len(recs) - 1; i >= 0 && recs[i].Timestamp == last; i-- {
			boundary[recs[i].key()] = true
		}
		cp.Since = last
		cp.Boundary = keys(boundary)

		if err := w.Flush(); err != nil {
			log.Fatal(err)
		}
		saveCheckpoint(*cpPath, cp)

		pages++
		if pages%200 == 0 {
			log.Printf("%d lines, index time %s, %.0f lines/s",
				cp.Lines, cp.Since[:10], float64(cp.Lines)/time.Since(start).Seconds())
		}
		if len(recs) < *limit {
			break // caught up with the head of the index
		}
	}
	log.Printf("done: %d lines, caught up to %s in %s", cp.Lines, cp.Since, time.Since(start).Round(time.Second))
}

type rawRecord struct {
	record
	raw string
}

func parse(lines []string) []rawRecord {
	recs := make([]rawRecord, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var r record
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			log.Printf("skipping unparsable line: %.80q", ln)
			continue
		}
		recs = append(recs, rawRecord{record: r, raw: ln})
	}
	return recs
}

// fetchPage GETs one index page, retrying forever with capped backoff:
// transient failures (including hours asleep) stall progress, never
// abort it.
func fetchPage(client *http.Client, since string, limit int) []string {
	u := fmt.Sprintf("%s?limit=%d", indexURL, limit)
	if since != "" {
		u += "&since=" + url.QueryEscape(since)
	}
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			log.Fatal(err) // malformed URL: programming error
		}
		req.Header.Set("User-Agent", "shape-of-go/0.1 (+https://github.com/LuisLSousa/shape-of-go)")
		resp, err := client.Do(req)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				body, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if readErr == nil {
					return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
				}
				err = readErr
			} else {
				resp.Body.Close()
				err = fmt.Errorf("status %s", resp.Status)
			}
		}
		if attempt%5 == 1 {
			log.Printf("fetch failed (attempt %d, retrying in %s): %v", attempt, backoff, err)
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, time.Minute)
	}
}

func loadCheckpoint(path string) checkpoint {
	var cp checkpoint
	b, err := os.ReadFile(path)
	if err != nil {
		return cp // first run
	}
	if err := json.Unmarshal(b, &cp); err != nil {
		log.Fatalf("corrupt checkpoint %s: %v (delete it to restart from scratch)", path, err)
	}
	return cp
}

// saveCheckpoint writes atomically (temp file + rename) so a crash
// mid-write can never corrupt the checkpoint.
func saveCheckpoint(path string, cp checkpoint) {
	b, err := json.Marshal(cp)
	if err != nil {
		log.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Fatal(err)
	}
}

// lock takes an exclusive flock on path, refusing to start if another
// indexsync already holds it. Two concurrent writers would interleave
// appends and race on the checkpoint; the kernel releases the lock on
// any process death, so there is no stale-lock cleanup to get wrong.
func lock(path string) (unlock func()) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatalf("another indexsync is already running against this output (lock %s held)", path)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

func keys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
