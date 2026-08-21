package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"slices"
)

// writePreview renders the positions as an additive-brightness scatter
// on black, a cheap approximation of the WebGL galaxy view, meant for
// eyeballing a long layout run from the terminal.
func writePreview(path string, pos []float64) {
	const size = 1600
	n := len(pos) / 2
	if n == 0 {
		return
	}
	// Frame on a robust extent (99.5th percentile of |x|,|y|) so a few
	// runaway satellites don't shrink the galaxy to a dot.
	abs := make([]float64, 0, 2*n)
	for _, v := range pos {
		abs = append(abs, math.Abs(v))
	}
	slices.Sort(abs)
	extent := abs[int(float64(len(abs)-1)*0.995)] * 1.05
	if extent <= 0 {
		extent = 1
	}

	counts := make([]float32, size*size)
	scale := size / (2 * extent)
	for i := range n {
		x := int((pos[2*i] + extent) * scale)
		y := int((extent - pos[2*i+1]) * scale)
		if x < 0 || x >= size || y < 0 || y >= size {
			continue
		}
		counts[y*size+x]++
	}
	var maxC float32 = 1
	for _, c := range counts {
		maxC = max(maxC, c)
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	logMax := math.Log1p(float64(maxC))
	for i, c := range counts {
		if c == 0 {
			continue
		}
		v := math.Log1p(float64(c)) / logMax
		// Dim dust is indigo-ish, dense cores burn toward white.
		r := uint8(80 + 175*v)
		g := uint8(70 + 185*v)
		b := uint8(160 + 95*v)
		img.SetRGBA(i%size, i/size, color.RGBA{r, g, b, 255})
	}
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
}
