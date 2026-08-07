import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  getLabel,
  loadAttrs,
  loadManifest,
  loadNbrIndex,
  loadNeighbors,
  loadPositions,
  loadSearch,
  type Manifest,
  type SearchRow,
} from './galaxy/data'
import { Camera } from './galaxy/camera'
import { SpatialGrid } from './galaxy/grid'
import {
  GalaxyRenderer,
  STATE_DEPENDENCY,
  STATE_DEPENDENT,
  STATE_DIM,
  STATE_SELECTED,
  type ColorMode,
} from './galaxy/renderer'
import { DEGREE_STOPS, SELECT_COLORS, YEAR_STEPS } from './galaxy/palette'

interface Dataset {
  manifest: Manifest
  positions: Float32Array
  inDeg: Uint32Array
  years: Uint8Array
  degNorm: Float32Array
  grid: SpatialGrid
  search: SearchRow[]
  nbrSet: Set<number>
}

interface Selection {
  idx: number
  path: string
  inDeg: number
  year: number | null
  dependents: number | null // kept-graph counts, null while loading / absent
  dependencies: number | null
  edgesSampled: boolean
}

interface Hover {
  idx: number
  x: number
  y: number
  path: string | null
}

const fmt = new Intl.NumberFormat('en-US')

export default function App() {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const [data, setData] = useState<Dataset | null>(null)
  const [progress, setProgress] = useState(0)
  const [error, setError] = useState<string | null>(null)
  const [mode, setMode] = useState<ColorMode>(() =>
    new URLSearchParams(window.location.search).get('mode') === 'year' ? 'year' : 'degree',
  )
  const [selection, setSelection] = useState<Selection | null>(null)
  const [hover, setHover] = useState<Hover | null>(null)
  const [query, setQuery] = useState('')
  const [activeResult, setActiveResult] = useState(0)

  const rendererRef = useRef<GalaxyRenderer | null>(null)
  const cameraRef = useRef(new Camera())
  const modeRef = useRef(mode)
  const hasSelectionRef = useRef(false)
  modeRef.current = mode

  // ---- data load ----
  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const manifest = await loadManifest()
        setProgress(0.1)
        const [positions, attrs, search, nbrSet] = await Promise.all([
          loadPositions(manifest.nodes).then((p) => {
            if (!cancelled) setProgress(0.55)
            return p
          }),
          loadAttrs(manifest.nodes).then((a) => {
            if (!cancelled) setProgress(0.75)
            return a
          }),
          loadSearch(),
          loadNbrIndex(),
        ])
        if (cancelled) return
        const n = manifest.nodes
        const degNorm = new Float32Array(n)
        const logMax = Math.log1p(manifest.maxInDeg)
        for (let i = 0; i < n; i++) degNorm[i] = Math.log1p(attrs.inDeg[i]) / logMax
        const grid = new SpatialGrid(positions, degNorm)
        setProgress(1)
        setData({ manifest, positions, inDeg: attrs.inDeg, years: attrs.years, degNorm, grid, search, nbrSet })
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  // ---- renderer + render loop ----
  useEffect(() => {
    if (!data || !canvasRef.current) return
    const canvas = canvasRef.current
    const camera = cameraRef.current

    const attrs = new Float32Array(data.manifest.nodes * 2)
    for (let i = 0; i < data.manifest.nodes; i++) {
      attrs[2 * i] = data.degNorm[i]
      attrs[2 * i + 1] = Math.min(data.years[i], 7) / 7
    }
    let renderer: GalaxyRenderer
    try {
      renderer = new GalaxyRenderer(canvas, data.positions, attrs)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      return
    }
    rendererRef.current = renderer

    // Frame the galaxy on its dense core: mean center, then a high
    // percentile of sampled distances so stray outer dust can't
    // shrink the view.
    const n = data.manifest.nodes
    let mx = 0
    let my = 0
    for (let i = 0; i < n; i++) {
      mx += data.positions[2 * i]
      my += data.positions[2 * i + 1]
    }
    mx /= n
    my /= n
    const dists: number[] = []
    for (let i = 0; i < n; i += 97) {
      dists.push(Math.hypot(data.positions[2 * i] - mx, data.positions[2 * i + 1] - my))
    }
    dists.sort((a, b) => a - b)
    const extent = dists[Math.floor(dists.length * 0.96)]

    let raf = 0
    const resize = () => {
      const dpr = Math.min(window.devicePixelRatio || 1, 2)
      const w = canvas.clientWidth
      const h = canvas.clientHeight
      if (canvas.width !== w * dpr || canvas.height !== h * dpr) {
        canvas.width = w * dpr
        canvas.height = h * dpr
      }
      return { w, h, dpr }
    }
    const { w, h } = resize()
    camera.fit(mx, my, extent, w, h)

    const loop = (now: number) => {
      camera.tick(now)
      const { w, h, dpr } = resize()
      renderer.render(camera, w, h, dpr, modeRef.current, hasSelectionRef.current)
      raf = requestAnimationFrame(loop)
    }
    raf = requestAnimationFrame(loop)
    return () => {
      cancelAnimationFrame(raf)
      rendererRef.current = null
    }
  }, [data])

  // ---- selection plumbing ----
  const clearSelection = useCallback(() => {
    setSelection(null)
    hasSelectionRef.current = false
    rendererRef.current?.setStates(null)
    rendererRef.current?.setEdges([])
  }, [])

  const select = useCallback(
    async (idx: number) => {
      if (!data) return
      const path = await getLabel(idx, data.manifest.labelChunk)
      const year = data.years[idx] === 255 ? null : data.manifest.yearBase + data.years[idx]
      const base: Selection = {
        idx,
        path,
        inDeg: data.inDeg[idx],
        year,
        dependents: null,
        dependencies: null,
        edgesSampled: false,
      }
      setSelection(base)
      hasSelectionRef.current = true

      if (data.nbrSet.has(idx)) {
        const nbr = await loadNeighbors(idx)
        // A newer click may have replaced this selection while the
        // neighbor list was in flight.
        setSelection((cur) => {
          if (!cur || cur.idx !== idx) return cur
          return { ...cur, dependents: nbr.dependents.length, dependencies: nbr.dependencies.length }
        })
        const renderer = rendererRef.current
        if (!renderer) return
        renderer.setStates((states) => {
          states.fill(STATE_DIM)
          for (const j of nbr.dependents) states[j] = STATE_DEPENDENT
          for (const j of nbr.dependencies) states[j] = STATE_DEPENDENCY
          states[idx] = STATE_SELECTED
        })
        // Cap the drawn edges: additive lines all converge on the hub,
        // so past ~20k segments the epicenter just saturates to white.
        // Node highlighting stays complete — only lines are sampled.
        const MAX_EDGES = 20000
        let sampled = false
        const segs = (list: Uint32Array) => {
          const stride = Math.max(1, Math.ceil(list.length / MAX_EDGES))
          sampled = sampled || stride > 1
          const count = Math.ceil(list.length / stride)
          const out = new Float32Array(count * 4)
          const sx = data.positions[2 * idx]
          const sy = data.positions[2 * idx + 1]
          for (let k = 0, o = 0; k < list.length; k += stride, o++) {
            out[4 * o] = sx
            out[4 * o + 1] = sy
            out[4 * o + 2] = data.positions[2 * list[k]]
            out[4 * o + 3] = data.positions[2 * list[k] + 1]
          }
          return out
        }
        renderer.setEdges([
          { segments: segs(nbr.dependents), color: SELECT_COLORS.dependents },
          { segments: segs(nbr.dependencies), color: SELECT_COLORS.dependencies },
        ])
        setSelection((cur) => (cur && cur.idx === idx ? { ...cur, edgesSampled: sampled } : cur))
      } else {
        rendererRef.current?.setStates((states) => {
          states.fill(STATE_DIM)
          states[idx] = STATE_SELECTED
        })
        rendererRef.current?.setEdges([])
      }
    },
    [data],
  )

  const flyToNode = useCallback(
    (idx: number) => {
      if (!data || !canvasRef.current) return
      const camera = cameraRef.current
      const w = canvasRef.current.clientWidth
      const h = canvasRef.current.clientHeight
      // Zoom so the neighborhood fills the view but context survives.
      const targetZoom = Math.max(camera.zoom, Math.min(w, h) / 900)
      camera.flyTo(data.positions[2 * idx], data.positions[2 * idx + 1], targetZoom)
      void select(idx)
    },
    [data, select],
  )

  // ?m=<module path> deep-links straight to a module (also handy for
  // headless screenshot tests of the selection state).
  useEffect(() => {
    if (!data) return
    const target = new URLSearchParams(window.location.search).get('m')
    if (!target) return
    const row = data.search.find((r) => r[0] === target)
    if (!row) return
    const t = window.setTimeout(() => flyToNode(row[1]), 400)
    return () => window.clearTimeout(t)
  }, [data, flyToNode])

  // ---- pointer input ----
  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas || !data) return
    const camera = cameraRef.current
    let dragging = false
    let moved = false
    let lastX = 0
    let lastY = 0
    let hoverTimer = 0

    const pickAt = (px: number, py: number): number => {
      const [wx, wy] = camera.screenToWorld(px, py, canvas.clientWidth, canvas.clientHeight)
      return data.grid.pick(wx, wy, 14 / camera.zoom)
    }

    const onPointerDown = (e: PointerEvent) => {
      dragging = true
      moved = false
      lastX = e.clientX
      lastY = e.clientY
      canvas.classList.add('dragging')
      canvas.setPointerCapture(e.pointerId)
    }
    const onPointerMove = (e: PointerEvent) => {
      if (dragging) {
        const dx = e.clientX - lastX
        const dy = e.clientY - lastY
        if (Math.abs(dx) + Math.abs(dy) > 2) moved = true
        camera.pan(dx, dy)
        lastX = e.clientX
        lastY = e.clientY
        setHover(null)
        return
      }
      window.clearTimeout(hoverTimer)
      const { clientX, clientY } = e
      hoverTimer = window.setTimeout(() => {
        const rect = canvas.getBoundingClientRect()
        const idx = pickAt(clientX - rect.left, clientY - rect.top)
        if (idx < 0) {
          setHover(null)
          return
        }
        setHover({ idx, x: clientX, y: clientY, path: null })
        void getLabel(idx, data.manifest.labelChunk).then((path) => {
          setHover((cur) => (cur && cur.idx === idx ? { ...cur, path } : cur))
        })
      }, 60)
    }
    const onPointerUp = (e: PointerEvent) => {
      canvas.classList.remove('dragging')
      if (!dragging) return
      dragging = false
      if (moved) return
      const rect = canvas.getBoundingClientRect()
      const idx = pickAt(e.clientX - rect.left, e.clientY - rect.top)
      if (idx >= 0) void select(idx)
      else clearSelection()
    }
    const onWheel = (e: WheelEvent) => {
      e.preventDefault()
      const rect = canvas.getBoundingClientRect()
      const factor = Math.exp(-e.deltaY * (e.ctrlKey ? 0.01 : 0.002))
      camera.zoomAt(factor, e.clientX - rect.left, e.clientY - rect.top, canvas.clientWidth, canvas.clientHeight)
      setHover(null)
    }
    const onLeave = () => {
      window.clearTimeout(hoverTimer)
      setHover(null)
    }

    canvas.addEventListener('pointerdown', onPointerDown)
    canvas.addEventListener('pointermove', onPointerMove)
    canvas.addEventListener('pointerup', onPointerUp)
    canvas.addEventListener('pointerleave', onLeave)
    canvas.addEventListener('wheel', onWheel, { passive: false })
    return () => {
      canvas.removeEventListener('pointerdown', onPointerDown)
      canvas.removeEventListener('pointermove', onPointerMove)
      canvas.removeEventListener('pointerup', onPointerUp)
      canvas.removeEventListener('pointerleave', onLeave)
      canvas.removeEventListener('wheel', onWheel)
      window.clearTimeout(hoverTimer)
    }
  }, [data, select, clearSelection])

  // Esc clears the selection or the search box.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        if (query) setQuery('')
        else clearSelection()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [query, clearSelection])

  // ---- search ----
  const results = useMemo(() => {
    if (!data || query.trim().length < 2) return []
    const q = query.trim().toLowerCase()
    const out: SearchRow[] = []
    for (const row of data.search) {
      if (row[0].toLowerCase().includes(q)) {
        out.push(row)
        if (out.length >= 10) break
      }
    }
    return out
  }, [data, query])

  useEffect(() => setActiveResult(0), [query])

  const runResult = (row: SearchRow) => {
    setQuery('')
    flyToNode(row[1])
  }

  if (error) {
    return <div className="error">The galaxy failed to load: {error}</div>
  }

  return (
    <div className="app">
      <canvas ref={canvasRef} className="galaxy" />

      {!data && (
        <div className="loading">
          <div>loading the Go module galaxy…</div>
          <div className="bar">
            <div style={{ width: `${Math.round(progress * 100)}%` }} />
          </div>
        </div>
      )}

      <div className="overlay header">
        <h1>The Shape of Go</h1>
        <div className="subtitle">
          Every Go module connected to the ecosystem, placed by its dependencies alone.
        </div>
        {data && (
          <div className="stats">
            {fmt.format(data.manifest.nodes)} modules · {fmt.format(data.manifest.edges)} dependency edges
          </div>
        )}
      </div>

      {data && (
        <div className="overlay search">
          <input
            type="text"
            placeholder="find a module — try testify, cobra, gin…"
            value={query}
            spellCheck={false}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'ArrowDown') {
                e.preventDefault()
                setActiveResult((a) => Math.min(a + 1, results.length - 1))
              } else if (e.key === 'ArrowUp') {
                e.preventDefault()
                setActiveResult((a) => Math.max(a - 1, 0))
              } else if (e.key === 'Enter' && results[activeResult]) {
                runResult(results[activeResult])
              }
            }}
          />
          {results.length > 0 && (
            <div className="results">
              {results.map((row, i) => (
                <button
                  key={row[1]}
                  className={i === activeResult ? 'active' : ''}
                  onMouseEnter={() => setActiveResult(i)}
                  onClick={() => runResult(row)}
                >
                  <span className="path">{row[0]}</span>
                  <span className="count">{fmt.format(row[2])}</span>
                </button>
              ))}
            </div>
          )}
        </div>
      )}

      {data && (
        <div className="overlay legend">
          <div className="modes">
            <button className={mode === 'degree' ? 'active' : ''} onClick={() => setMode('degree')}>
              Dependents
            </button>
            <button className={mode === 'year' ? 'active' : ''} onClick={() => setMode('year')}>
              First seen
            </button>
          </div>
          {mode === 'degree' ? (
            <>
              <div
                className="ramp"
                style={{ background: `linear-gradient(to right, ${DEGREE_STOPS.join(', ')})` }}
              />
              <div className="ends">
                <span>0</span>
                <span>{fmt.format(data.manifest.maxInDeg)} dependents (log)</span>
              </div>
              <div className="note">
                Brightness follows how many modules import each node. The dark dust is the {' '}
                93% nobody imports.
              </div>
            </>
          ) : (
            <>
              <div className="ramp stepped">
                {YEAR_STEPS.map((c) => (
                  <span key={c} style={{ background: c }} />
                ))}
              </div>
              <div className="ends">
                <span>2019</span>
                <span>2026</span>
              </div>
              <div className="note">Color is the year a module first appeared in the proxy index.</div>
            </>
          )}
        </div>
      )}

      {selection && (
        <div className="overlay info">
          <button className="close" onClick={clearSelection} aria-label="Clear selection">
            ×
          </button>
          <div className="path">
            <a href={`https://pkg.go.dev/${selection.path}`} target="_blank" rel="noreferrer">
              {selection.path}
            </a>
          </div>
          <div className="rows">
            <div className="row">
              <span>direct dependents</span>
              <b>{fmt.format(selection.inDeg)}</b>
            </div>
            {selection.year !== null && (
              <div className="row">
                <span>first seen</span>
                <b>{selection.year}</b>
              </div>
            )}
            {selection.dependents !== null && (
              <div className="row">
                <span>
                  <i className="dot" style={{ background: SELECT_COLORS.dependents }} />
                  dependents shown
                </span>
                <b>{fmt.format(selection.dependents)}</b>
              </div>
            )}
            {selection.dependencies !== null && (
              <div className="row">
                <span>
                  <i className="dot" style={{ background: SELECT_COLORS.dependencies }} />
                  dependencies shown
                </span>
                <b>{fmt.format(selection.dependencies)}</b>
              </div>
            )}
          </div>
          <div className="hint">
            {selection.dependents === null && selection.dependencies === null
              ? 'Neighbor highlighting is precomputed for the top 20,000 hubs; this module is dust — the honest kind.'
              : `Amber = modules that import it · aqua = what it imports.${
                  selection.edgesSampled ? ' Lines are a sample; every node is lit.' : ''
                } Esc to clear.`}
          </div>
        </div>
      )}

      {hover && hover.path && (!selection || hover.idx !== selection.idx) && (
        <div className="tooltip" style={{ left: hover.x, top: hover.y }}>
          {hover.path}
          {data && <span className="count">{fmt.format(data.inDeg[hover.idx])}</span>}
        </div>
      )}
    </div>
  )
}
