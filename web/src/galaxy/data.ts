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

export async function loadNbrIndex(): Promise<Set<number>> {
  const buf = await fetchBuffer(`${BASE}nbr-index.bin`)
  return new Set(new Uint32Array(buf))
}

export async function loadNeighbors(idx: number): Promise<Neighbors> {
  const buf = await fetchBuffer(`${BASE}nbr/${idx}.bin`)
  const header = new Uint32Array(buf, 0, 2)
  const nIn = header[0]
  const nOut = header[1]
  return {
    dependents: new Uint32Array(buf, 8, nIn),
    dependencies: new Uint32Array(buf, 8 + 4 * nIn, nOut),
  }
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
