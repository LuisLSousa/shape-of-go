// Uniform spatial grid over the (static) node positions, for hover and
// click picking. Built once with a counting sort; queries scan the
// cells covering the search radius and prefer the most-imported node
// so hubs are easy to grab inside their own dust cloud.

export class SpatialGrid {
  private cellsPerSide: number
  private minX = 0
  private minY = 0
  private cellSize = 1
  private starts: Int32Array
  private items: Int32Array

  constructor(
    private positions: Float32Array,
    private weight: Float32Array, // 0..1 pick priority (normalized log in-degree)
    cellsPerSide = 512,
  ) {
    const n = positions.length / 2
    this.cellsPerSide = cellsPerSide
    let minX = Infinity,
      minY = Infinity,
      maxX = -Infinity,
      maxY = -Infinity
    for (let i = 0; i < n; i++) {
      minX = Math.min(minX, positions[2 * i])
      maxX = Math.max(maxX, positions[2 * i])
      minY = Math.min(minY, positions[2 * i + 1])
      maxY = Math.max(maxY, positions[2 * i + 1])
    }
    this.minX = minX
    this.minY = minY
    this.cellSize = Math.max(maxX - minX, maxY - minY) / cellsPerSide + 1e-9

    const nCells = cellsPerSide * cellsPerSide
    const counts = new Int32Array(nCells + 1)
    const cellOf = new Int32Array(n)
    for (let i = 0; i < n; i++) {
      const c = this.cellIndex(positions[2 * i], positions[2 * i + 1])
      cellOf[i] = c
      counts[c + 1]++
    }
    for (let c = 0; c < nCells; c++) counts[c + 1] += counts[c]
    this.starts = counts
    this.items = new Int32Array(n)
    const cursor = counts.slice(0, nCells)
    for (let i = 0; i < n; i++) this.items[cursor[cellOf[i]]++] = i
  }

  private cellIndex(x: number, y: number): number {
    const cx = Math.min(this.cellsPerSide - 1, Math.max(0, Math.floor((x - this.minX) / this.cellSize)))
    const cy = Math.min(this.cellsPerSide - 1, Math.max(0, Math.floor((y - this.minY) / this.cellSize)))
    return cy * this.cellsPerSide + cx
  }

  /**
   * Best node within `radius` of (x, y): importance-weighted so a hub
   * beats a leaf that happens to sit a little closer to the cursor.
   */
  pick(x: number, y: number, radius: number): number {
    const c0x = Math.max(0, Math.floor((x - radius - this.minX) / this.cellSize))
    const c1x = Math.min(this.cellsPerSide - 1, Math.floor((x + radius - this.minX) / this.cellSize))
    const c0y = Math.max(0, Math.floor((y - radius - this.minY) / this.cellSize))
    const c1y = Math.min(this.cellsPerSide - 1, Math.floor((y + radius - this.minY) / this.cellSize))
    let best = -1
    let bestScore = -Infinity
    const r2 = radius * radius
    for (let cy = c0y; cy <= c1y; cy++) {
      for (let cx = c0x; cx <= c1x; cx++) {
        const c = cy * this.cellsPerSide + cx
        for (let k = this.starts[c]; k < this.starts[c + 1]; k++) {
          const i = this.items[k]
          const dx = this.positions[2 * i] - x
          const dy = this.positions[2 * i + 1] - y
          const d2 = dx * dx + dy * dy
          if (d2 > r2) continue
          // Nearness matters, but importance breaks the dust tie.
          const score = this.weight[i] * 2 - d2 / r2
          if (score > bestScore) {
            bestScore = score
            best = i
          }
        }
      }
    }
    return best
  }
}
