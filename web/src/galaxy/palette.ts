// Galaxy color system. Two encodings, both validated with the dataviz
// palette validator against the space surface (#06060e):
//
//  - degree: SEQUENTIAL indigo, dim → white-hot, driven by log in-degree.
//    The near-zero end deliberately recedes into the surface — 93% of
//    the ecosystem is never-imported dust and should read as background.
//  - year: ORDINAL amber, 8 monotone-lightness steps for the 2019–2026
//    first-seen cohorts (all ordinal gates pass: ΔL ≥ 0.06 per step,
//    dark end 2.95:1 vs surface, single hue).

export const SURFACE = '#06060e'

export const DEGREE_STOPS = ['#312e81', '#4f46e5', '#818cf8', '#c7d2fe', '#f1f5ff']

export const YEAR_STEPS = [
  '#8a4a12', // 2019
  '#a25c12',
  '#b96f12',
  '#cf8412',
  '#e09a20',
  '#efb241',
  '#face6c',
  '#ffedad', // 2026
]

// Selection classes (interaction state, not series): the selected node,
// the modules that import it, and the modules it imports. Each is also
// named with a colored dot + text in the info panel, so identity never
// rides on color alone.
export const SELECT_COLORS = {
  selected: '#ffffff',
  dependents: '#fbbf24',
  dependencies: '#34d399',
}

function hexToRgb(hex: string): [number, number, number] {
  const v = parseInt(hex.slice(1), 16)
  return [((v >> 16) & 255) / 255, ((v >> 8) & 255) / 255, (v & 255) / 255]
}

/** Bake a 256-entry RGB ramp by piecewise-linear interpolation. */
function bakeRamp(stops: string[], discrete: boolean): Uint8Array {
  const rgb = stops.map(hexToRgb)
  const out = new Uint8Array(256 * 4)
  for (let i = 0; i < 256; i++) {
    const t = i / 255
    let r: number, g: number, b: number
    if (discrete) {
      const k = Math.min(stops.length - 1, Math.floor(t * stops.length))
      ;[r, g, b] = rgb[k]
    } else {
      const x = t * (stops.length - 1)
      const k = Math.min(stops.length - 2, Math.floor(x))
      const f = x - k
      r = rgb[k][0] + (rgb[k + 1][0] - rgb[k][0]) * f
      g = rgb[k][1] + (rgb[k + 1][1] - rgb[k][1]) * f
      b = rgb[k][2] + (rgb[k + 1][2] - rgb[k][2]) * f
    }
    out[i * 4] = r * 255
    out[i * 4 + 1] = g * 255
    out[i * 4 + 2] = b * 255
    out[i * 4 + 3] = 255
  }
  return out
}

/** Two-row ramp texture: row 0 = degree (smooth), row 1 = year (stepped). */
export function rampTextureData(): Uint8Array {
  const deg = bakeRamp(DEGREE_STOPS, false)
  const yr = bakeRamp(YEAR_STEPS, true)
  const out = new Uint8Array(256 * 2 * 4)
  out.set(deg, 0)
  out.set(yr, 256 * 4)
  return out
}
