package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

// WritePreview writes a PNG with two panels: left = absolute log scale
// (100..65535 ADU, so frames are comparable and saturation is white), right =
// per-frame percentile stretch (shows faint structure such as banding).
func WritePreview(path string, pix []uint16, w, h int, st Stats) error {
	f := 1
	for w/f > 1000 || h/f > 1000 {
		f++
	}
	pw, ph := w/f, h/f
	if pw == 0 || ph == 0 {
		return nil
	}
	img := image.NewGray(image.Rect(0, 0, 2*pw+4, ph))
	lo, hi := st.P01, st.P99
	if hi <= lo {
		hi = lo + 1
	}
	l0, l1 := math.Log10(100), math.Log10(65535)
	for y := 0; y < ph; y++ {
		for x := 0; x < pw; x++ {
			sum := 0.0
			for dy := 0; dy < f; dy++ {
				for dx := 0; dx < f; dx++ {
					sum += float64(pix[(y*f+dy)*w+x*f+dx])
				}
			}
			v := sum / float64(f*f)
			a := (math.Log10(math.Max(v, 100)) - l0) / (l1 - l0)
			b := (v - lo) / (hi - lo)
			img.SetGray(x, y, color.Gray{uint8(255 * clamp01(a))})
			img.SetGray(pw+4+x, y, color.Gray{uint8(255 * clamp01(b))})
		}
		for x := pw; x < pw+4; x++ {
			img.SetGray(x, y, color.Gray{128})
		}
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	return png.Encode(out, img)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
