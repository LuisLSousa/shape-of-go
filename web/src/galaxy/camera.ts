// A 2-D pan/zoom camera. World coordinates come from the layout;
// `zoom` is screen pixels per world unit. All input math keeps the
// point under the cursor fixed, which is what makes zooming feel
// physical instead of slippery.

export interface View {
  cx: number
  cy: number
  zoom: number
}

const EASE = (t: number) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2)

export class Camera {
  cx = 0
  cy = 0
  zoom = 1
  fitZoom = 1
  minZoom = 1e-5
  maxZoom = 100

  private anim: {
    from: View
    to: View
    start: number
    duration: number
  } | null = null

  /** Fit a world extent around a center point into a viewport. */
  fit(cx: number, cy: number, extent: number, widthPx: number, heightPx: number) {
    this.cx = cx
    this.cy = cy
    this.zoom = Math.min(widthPx, heightPx) / (2 * extent * 1.05)
    this.fitZoom = this.zoom
    this.minZoom = this.zoom * 0.25
    this.maxZoom = this.zoom * 20000
  }

  /** Screen position of a world point (y down, CSS pixels). */
  worldToScreen(wx: number, wy: number, w: number, h: number): [number, number] {
    return [(wx - this.cx) * this.zoom + w / 2, (this.cy - wy) * this.zoom + h / 2]
  }

  screenToWorld(px: number, py: number, w: number, h: number): [number, number] {
    return [this.cx + (px - w / 2) / this.zoom, this.cy - (py - h / 2) / this.zoom]
  }

  pan(dxPx: number, dyPx: number) {
    this.anim = null
    this.cx -= dxPx / this.zoom
    this.cy += dyPx / this.zoom
  }

  /** Zoom by a factor, anchored at a screen point. */
  zoomAt(factor: number, px: number, py: number, w: number, h: number) {
    this.anim = null
    const [wx, wy] = this.screenToWorld(px, py, w, h)
    this.zoom = Math.min(this.maxZoom, Math.max(this.minZoom, this.zoom * factor))
    // Re-anchor so (wx, wy) is back under the cursor.
    this.cx = wx - (px - w / 2) / this.zoom
    this.cy = wy + (py - h / 2) / this.zoom
  }

  /** Instant reposition: deep links land, they don't travel. */
  jumpTo(wx: number, wy: number, zoom: number) {
    this.anim = null
    this.cx = wx
    this.cy = wy
    this.zoom = Math.min(this.maxZoom, Math.max(this.minZoom, zoom))
  }

  flyTo(wx: number, wy: number, zoom: number, duration = 900) {
    this.anim = {
      from: { cx: this.cx, cy: this.cy, zoom: this.zoom },
      to: { cx: wx, cy: wy, zoom: Math.min(this.maxZoom, zoom) },
      start: performance.now(),
      duration,
    }
  }

  /** Advance any running fly-to; returns true while animating. */
  tick(now: number): boolean {
    if (!this.anim) return false
    const { from, to, start, duration } = this.anim
    const t = Math.min(1, (now - start) / duration)
    const e = EASE(t)
    // Interpolate zoom geometrically, which is perceptually uniform.
    this.zoom = from.zoom * Math.pow(to.zoom / from.zoom, e)
    this.cx = from.cx + (to.cx - from.cx) * e
    this.cy = from.cy + (to.cy - from.cy) * e
    if (t >= 1) this.anim = null
    return this.anim !== null
  }
}
