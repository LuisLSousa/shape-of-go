// WebGL2 renderer: the whole galaxy is one additive-blended POINTS draw
// over a static position buffer; selection adds two LINES draws (edges
// to dependents / dependencies) built on demand. Color comes from a
// 256×2 ramp texture (row 0 degree, row 1 cohort year) so switching
// modes is a uniform, not a buffer rewrite.

import { rampTextureData, SELECT_COLORS, SURFACE } from './palette'
import type { Camera } from './camera'

export type ColorMode = 'degree' | 'year'

// Node selection states, mirrored in the vertex shader.
export const STATE_NORMAL = 0
export const STATE_DIM = 1
export const STATE_SELECTED = 2
export const STATE_DEPENDENT = 3
export const STATE_DEPENDENCY = 4

const POINT_VS = `#version 300 es
precision highp float;
layout(location=0) in vec2 aPos;
layout(location=1) in vec2 aAttr;   // x: log-degree 0..1, y: year 0..1
layout(location=2) in float aState;
uniform vec2 uCenter;
uniform float uZoom;
uniform vec2 uViewport;
uniform float uDpr;
uniform int uMode;
uniform float uHasSelection;
uniform float uSelAlpha;
uniform sampler2D uRamp;
out vec4 vColor;
out float vCore;

vec3 stateColor(int s, vec3 base) {
  if (s == ${STATE_SELECTED}) return vec3(1.0);
  if (s == ${STATE_DEPENDENT}) return vec3(${hex3(SELECT_COLORS.dependents)});
  if (s == ${STATE_DEPENDENCY}) return vec3(${hex3(SELECT_COLORS.dependencies)});
  return base;
}

void main() {
  vec2 px = (aPos - uCenter) * uZoom;
  gl_Position = vec4(2.0 * px / uViewport, 0.0, 1.0);

  float deg = aAttr.x;
  float rampX = uMode == 0 ? deg : aAttr.y;
  vec3 base = texture(uRamp, vec2(rampX, uMode == 0 ? 0.25 : 0.75)).rgb;

  int s = int(aState + 0.5);
  vec3 color = stateColor(s, base);

  // Size: dust stays ~1px, hubs grow with log degree; zooming in
  // grows everything gently (sub-linear) so the core never floods.
  float zoomGrow = pow(clamp(uZoom, 0.001, 100.0), 0.18);
  float size = (1.3 + 26.0 * pow(deg, 1.35)) * zoomGrow * uDpr;
  if (s == ${STATE_SELECTED}) size = max(size * 1.8, 14.0 * uDpr);
  gl_PointSize = clamp(size, 1.0, 96.0 * uDpr);

  // Brightness: dim everything that isn't part of an active selection.
  // uSelAlpha shrinks with the highlight count so a 300k-node blast
  // stays readable while a 500-node one keeps full drama.
  float alpha = 0.28 + 0.6 * deg;
  if (uHasSelection > 0.5) {
    alpha = s == ${STATE_DIM} ? alpha * 0.05 : min(1.0, (alpha * 1.6 + 0.25) * uSelAlpha);
  }
  vColor = vec4(color, alpha);
  vCore = clamp(deg * 1.4 + (s >= ${STATE_SELECTED} ? 0.6 : 0.0), 0.0, 1.0);
}
`

const POINT_FS = `#version 300 es
precision highp float;
in vec4 vColor;
in float vCore;
out vec4 outColor;

void main() {
  vec2 p = gl_PointCoord * 2.0 - 1.0;
  float r2 = dot(p, p);
  if (r2 > 1.0) discard;
  // Soft gaussian glow plus a hot core for important nodes.
  float glow = exp(-r2 * 3.0) - exp(-3.0);
  float core = smoothstep(0.28, 0.0, r2) * vCore;
  float b = vColor.a * (glow + core);
  // Additive blending: premultiplied color, alpha channel unused.
  outColor = vec4(vColor.rgb * b + vec3(1.0) * core * b * 0.35, 1.0);
}
`

const LINE_VS = `#version 300 es
precision highp float;
layout(location=0) in vec2 aPos;
uniform vec2 uCenter;
uniform float uZoom;
uniform vec2 uViewport;
void main() {
  vec2 px = (aPos - uCenter) * uZoom;
  gl_Position = vec4(2.0 * px / uViewport, 0.0, 1.0);
}
`

const LINE_FS = `#version 300 es
precision highp float;
uniform vec3 uColor;
uniform float uAlpha;
out vec4 outColor;
void main() { outColor = vec4(uColor * uAlpha, 1.0); }
`

function hex3(hex: string): string {
  const v = parseInt(hex.slice(1), 16)
  const f = (x: number) => (x / 255).toFixed(4)
  return `${f((v >> 16) & 255)}, ${f((v >> 8) & 255)}, ${f(v & 255)}`
}

function hexToVec(hex: string): [number, number, number] {
  const v = parseInt(hex.slice(1), 16)
  return [((v >> 16) & 255) / 255, ((v >> 8) & 255) / 255, (v & 255) / 255]
}

function compile(gl: WebGL2RenderingContext, type: number, src: string): WebGLShader {
  const sh = gl.createShader(type)!
  gl.shaderSource(sh, src)
  gl.compileShader(sh)
  if (!gl.getShaderParameter(sh, gl.COMPILE_STATUS)) {
    throw new Error(`shader: ${gl.getShaderInfoLog(sh)}`)
  }
  return sh
}

function link(gl: WebGL2RenderingContext, vs: string, fs: string): WebGLProgram {
  const p = gl.createProgram()!
  gl.attachShader(p, compile(gl, gl.VERTEX_SHADER, vs))
  gl.attachShader(p, compile(gl, gl.FRAGMENT_SHADER, fs))
  gl.linkProgram(p)
  if (!gl.getProgramParameter(p, gl.LINK_STATUS)) {
    throw new Error(`program: ${gl.getProgramInfoLog(p)}`)
  }
  return p
}

export class GalaxyRenderer {
  private gl: WebGL2RenderingContext
  private n: number
  private pointProgram: WebGLProgram
  private lineProgram: WebGLProgram
  private pointVao: WebGLVertexArrayObject
  private stateBuf: WebGLBuffer
  private states: Uint8Array
  private edges: { vao: WebGLVertexArrayObject; count: number; color: [number, number, number] }[] = []
  private uniforms: Record<string, WebGLUniformLocation | null> = {}
  private lineUniforms: Record<string, WebGLUniformLocation | null> = {}
  private clearColor: [number, number, number]
  private selAlpha = 1

  constructor(canvas: HTMLCanvasElement, positions: Float32Array, attrs: Float32Array) {
    const gl = canvas.getContext('webgl2', { antialias: false, alpha: false })
    if (!gl) throw new Error('WebGL2 is not available')
    this.gl = gl
    this.n = positions.length / 2
    this.states = new Uint8Array(this.n)
    this.clearColor = hexToVec(SURFACE)

    this.pointProgram = link(gl, POINT_VS, POINT_FS)
    this.lineProgram = link(gl, LINE_VS, LINE_FS)
    for (const name of ['uCenter', 'uZoom', 'uViewport', 'uDpr', 'uMode', 'uHasSelection', 'uSelAlpha', 'uRamp']) {
      this.uniforms[name] = gl.getUniformLocation(this.pointProgram, name)
    }
    for (const name of ['uCenter', 'uZoom', 'uViewport', 'uColor', 'uAlpha']) {
      this.lineUniforms[name] = gl.getUniformLocation(this.lineProgram, name)
    }

    this.pointVao = gl.createVertexArray()!
    gl.bindVertexArray(this.pointVao)
    const posBuf = gl.createBuffer()!
    gl.bindBuffer(gl.ARRAY_BUFFER, posBuf)
    gl.bufferData(gl.ARRAY_BUFFER, positions, gl.STATIC_DRAW)
    gl.enableVertexAttribArray(0)
    gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0)
    const attrBuf = gl.createBuffer()!
    gl.bindBuffer(gl.ARRAY_BUFFER, attrBuf)
    gl.bufferData(gl.ARRAY_BUFFER, attrs, gl.STATIC_DRAW)
    gl.enableVertexAttribArray(1)
    gl.vertexAttribPointer(1, 2, gl.FLOAT, false, 0, 0)
    this.stateBuf = gl.createBuffer()!
    gl.bindBuffer(gl.ARRAY_BUFFER, this.stateBuf)
    gl.bufferData(gl.ARRAY_BUFFER, this.states, gl.DYNAMIC_DRAW)
    gl.enableVertexAttribArray(2)
    gl.vertexAttribPointer(2, 1, gl.UNSIGNED_BYTE, false, 0, 0)
    gl.bindVertexArray(null)

    const ramp = gl.createTexture()!
    gl.activeTexture(gl.TEXTURE0)
    gl.bindTexture(gl.TEXTURE_2D, ramp)
    gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, 256, 2, 0, gl.RGBA, gl.UNSIGNED_BYTE, rampTextureData())
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR)
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR)
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE)
    gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE)

    gl.enable(gl.BLEND)
    gl.blendFunc(gl.ONE, gl.ONE) // additive: light accumulates
    gl.disable(gl.DEPTH_TEST)
  }

  /** Replace all node states (pass null to clear the selection). */
  setStates(update: ((states: Uint8Array) => void) | null) {
    this.states.fill(STATE_NORMAL)
    if (update) update(this.states)
    let highlighted = 0
    for (let i = 0; i < this.n; i++) {
      if (this.states[i] >= STATE_SELECTED) highlighted++
    }
    this.selAlpha = Math.min(1, Math.max(0.12, 60 / Math.sqrt(highlighted + 1)))
    const gl = this.gl
    gl.bindBuffer(gl.ARRAY_BUFFER, this.stateBuf)
    gl.bufferSubData(gl.ARRAY_BUFFER, 0, this.states)
  }

  /** Replace selection edge geometry: interleaved endpoint pairs. */
  setEdges(groups: { segments: Float32Array; color: string }[]) {
    const gl = this.gl
    for (const e of this.edges) gl.deleteVertexArray(e.vao)
    this.edges = []
    for (const { segments, color } of groups) {
      if (segments.length === 0) continue
      const vao = gl.createVertexArray()!
      gl.bindVertexArray(vao)
      const buf = gl.createBuffer()!
      gl.bindBuffer(gl.ARRAY_BUFFER, buf)
      gl.bufferData(gl.ARRAY_BUFFER, segments, gl.STATIC_DRAW)
      gl.enableVertexAttribArray(0)
      gl.vertexAttribPointer(0, 2, gl.FLOAT, false, 0, 0)
      gl.bindVertexArray(null)
      this.edges.push({ vao, count: segments.length / 2, color: hexToVec(color) })
    }
  }

  render(camera: Camera, widthPx: number, heightPx: number, dpr: number, mode: ColorMode, hasSelection: boolean) {
    const gl = this.gl
    gl.viewport(0, 0, widthPx * dpr, heightPx * dpr)
    const [r, g, b] = this.clearColor
    gl.clearColor(r, g, b, 1)
    gl.clear(gl.COLOR_BUFFER_BIT)

    if (this.edges.length > 0) {
      gl.useProgram(this.lineProgram)
      gl.uniform2f(this.lineUniforms.uCenter, camera.cx, camera.cy)
      gl.uniform1f(this.lineUniforms.uZoom, camera.zoom)
      gl.uniform2f(this.lineUniforms.uViewport, widthPx, heightPx)
      for (const e of this.edges) {
        gl.bindVertexArray(e.vao)
        gl.uniform3f(this.lineUniforms.uColor, e.color[0], e.color[1], e.color[2])
        gl.uniform1f(this.lineUniforms.uAlpha, Math.min(0.3, 220 / e.count))
        gl.drawArrays(gl.LINES, 0, e.count)
      }
    }

    gl.useProgram(this.pointProgram)
    gl.uniform2f(this.uniforms.uCenter, camera.cx, camera.cy)
    gl.uniform1f(this.uniforms.uZoom, camera.zoom)
    gl.uniform2f(this.uniforms.uViewport, widthPx, heightPx)
    gl.uniform1f(this.uniforms.uDpr, dpr)
    gl.uniform1i(this.uniforms.uMode, mode === 'degree' ? 0 : 1)
    gl.uniform1f(this.uniforms.uHasSelection, hasSelection ? 1 : 0)
    gl.uniform1f(this.uniforms.uSelAlpha, this.selAlpha)
    gl.uniform1i(this.uniforms.uRamp, 0)
    gl.bindVertexArray(this.pointVao)
    gl.drawArrays(gl.POINTS, 0, this.n)
    gl.bindVertexArray(null)
  }
}
