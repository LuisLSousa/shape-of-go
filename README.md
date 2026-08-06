# The Shape of Go

A map of the Go module ecosystem: every public module, every dependency
edge, one graph — built from the official module index and proxy, and
analyzed with [gonx](https://github.com/LuisLSousa/gonx).

## Where the data comes from (no scraping)

The Go ecosystem publishes itself through two public, machine-readable
services — the same ones every `go get` already talks to:

1. **`index.golang.org/index`** — an append-only JSONL feed of every
   module version the proxy has ever seen (since April 2019), paginated
   by timestamp (`?since=...&limit=2000`).
2. **`proxy.golang.org/<module>/@v/<version>.mod`** — the raw `go.mod`
   for any module version. Its `require` directives are the dependency
   edges. `@latest` resolves the current version.

## Pipeline

```
cmd/indexsync   stream the full index → data/index.jsonl   (resumable, checkpointed)
cmd/fetchmods   latest version per module → fetch go.mod   (concurrent, disk-cached, resumable)
cmd/buildgraph  parse require directives → edge list + node table
cmd/analyze     degree distributions, load-bearing modules, components, communities
essay/          the write-up and figures
```

Every stage is idempotent and resumable: interrupt at any point and
re-run to continue. All randomness is seeded; the graph snapshot is
dated so the whole essay reproduces from one command per stage.

## Status

Pipeline scaffolding. See the essay when it ships.

## License

MIT — see [LICENSE](LICENSE).
