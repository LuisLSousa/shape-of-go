// Static-asset loading for the galaxy: everything is exported by
// cmd/export as either JSON or little-endian binary indexed by "kept
// index" (the layout's node order).

const BASE = `${import.meta.env.BASE_URL}data/`

export interface Manifest {
  nodes: number
  edges: number
  maxInDeg: number
  labelChunk: number
  searchTop: number
  nbrTop: number
  yearBase: number
  generated: string
}

export type SearchRow = [path: string, idx: number, inDeg: number]

export interface Neighbors {
  dependents: Uint32Array
  dependencies: Uint32Array
}

async function fetchBuffer(url: string): Promise<ArrayBuffer> {
  const res = await fetch(url)
  if (!res.ok) throw new Error(`${url}: HTTP ${res.status}`)
  return res.arrayBuffer()
}

export async function loadManifest(): Promise<Manifest> {
  const res = await fetch(`${BASE}galaxy.json`)
  if (!res.ok) throw new Error(`galaxy.json: HTTP ${res.status}`)
  return res.json()
}

export async function loadPositions(n: number): Promise<Float32Array> {
  const buf = await fetchBuffer(`${BASE}positions.bin`)
  const arr = new Float32Array(buf)
  if (arr.length !== 2 * n) throw new Error(`positions.bin: ${arr.length} floats, want ${2 * n}`)
  return arr
}

export async function loadAttrs(n: number): Promise<{ inDeg: Uint32Array; years: Uint8Array }> {
  const buf = await fetchBuffer(`${BASE}attrs.bin`)
  if (buf.byteLength !== 5 * n) throw new Error(`attrs.bin: ${buf.byteLength} bytes, want ${5 * n}`)
  return {
    inDeg: new Uint32Array(buf, 0, n),
    years: new Uint8Array(buf, 4 * n, n),
  }
}

export async function loadSearch(): Promise<SearchRow[]> {
  const res = await fetch(`${BASE}search.json`)
  if (!res.ok) throw new Error(`search.json: HTTP ${res.status}`)
  return res.json()
}

// NbrIndex locates each hub's neighbor record inside a shard file. The
// table is sorted by kept index, so lookups binary-search it rather than
// materializing a 60k-entry Map.
export class NbrIndex {
  private keys: Uint32Array
  private offsets: Uint32Array
  private lengths: Uint32Array
  private shards: Uint16Array
  readonly shardCount: number

  constructor(buf: ArrayBuffer) {
    const header = new Uint32Array(buf, 0, 4)
    if (header[0] !== NBR_MAGIC) {
      throw new Error('nbr-index.bin: bad magic: stale or truncated asset tree')
    }
    if (header[1] !== 1) {
      throw new Error(`nbr-index.bin: unsupported format version ${header[1]}`)
    }
    const count = header[2]
    this.shardCount = header[3]
    let o = 16
    this.keys = new Uint32Array(buf, o, count)
    o += 4 * count
    this.offsets = new Uint32Array(buf, o, count)
    o += 4 * count
    this.lengths = new Uint32Array(buf, o, count)
    o += 4 * count
    this.shards = new Uint16Array(buf, o, count)
  }

  /** Record slot for a kept index, or -1 when the node has no record. */
  private slot(idx: number): number {
    let lo = 0
    let hi = this.keys.length - 1
    while (lo <= hi) {
      const mid = (lo + hi) >> 1
      const k = this.keys[mid]
      if (k === idx) return mid
      if (k < idx) lo = mid + 1
      else hi = mid - 1
    }
    return -1
  }

  has(idx: number): boolean {
    return this.slot(idx) >= 0
  }

  locate(idx: number): { shard: number; offset: number; length: number } | null {
    const s = this.slot(idx)
    if (s < 0) return null
    return { shard: this.shards[s], offset: this.offsets[s], length: this.lengths[s] }
  }
}

const NBR_MAGIC = 0x3152424e // "NBR1"

export async function loadNbrIndex(): Promise<NbrIndex> {
  return new NbrIndex(await fetchBuffer(`${BASE}nbr-index.bin`))
}

// Shards are cached by their promise: hubs are packed in rank order, so
// clicking several popular modules usually reuses one download.
const shardCache = new Map<number, Promise<ArrayBuffer>>()

function getShard(shard: number): Promise<ArrayBuffer> {
  let p = shardCache.get(shard)
  if (!p) {
    p = fetchBuffer(`${BASE}nbr/${shard}.bin`)
    shardCache.set(shard, p)
  }
  return p
}

/**
 * decodeNeighbors reads one record: uvarint dependent count, uvarint
 * dependency count, then both lists as ascending delta uvarints.
 */
function decodeNeighbors(bytes: Uint8Array): Neighbors {
  let p = 0
  const uvarint = (): number => {
    let x = 0
    let shift = 0
    for (;;) {
      const b = bytes[p++]
      x += (b & 0x7f) * 2 ** shift
      if ((b & 0x80) === 0) return x
      shift += 7
    }
  }
  const nIn = uvarint()
  const nOut = uvarint()
  const read = (n: number): Uint32Array => {
    const out = new Uint32Array(n)
    let prev = 0
    for (let i = 0; i < n; i++) {
      prev += uvarint()
      out[i] = prev
    }
    return out
  }
  return { dependents: read(nIn), dependencies: read(nOut) }
}

const nbrCache = new Map<number, Promise<Neighbors>>()

export function loadNeighbors(index: NbrIndex, idx: number): Promise<Neighbors> {
  let p = nbrCache.get(idx)
  if (!p) {
    const at = index.locate(idx)
    if (!at) return Promise.reject(new Error(`no neighbor record for kept index ${idx}`))
    p = getShard(at.shard).then((buf) => decodeNeighbors(new Uint8Array(buf, at.offset, at.length)))
    nbrCache.set(idx, p)
  }
  return p
}

// Labels arrive in fixed-size JSON chunks; hovering only ever needs a
// few of them, and each chunk is cached by its promise so concurrent
// hovers coalesce into one fetch.
const labelChunks = new Map<number, Promise<string[]>>()

export function getLabel(idx: number, chunkSize: number): Promise<string> {
  const chunk = Math.floor(idx / chunkSize)
  let p = labelChunks.get(chunk)
  if (!p) {
    p = fetch(`${BASE}labels/${chunk}.json`).then((res) => {
      if (!res.ok) throw new Error(`labels/${chunk}.json: HTTP ${res.status}`)
      return res.json()
    })
    labelChunks.set(chunk, p)
  }
  return p.then((labels) => labels[idx % chunkSize])
}
