package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

const satLevel = 65000 // pixels at/above this are treated as saturated

type Stats struct {
	N             int
	Min, Max      uint16
	Mean, Std     float64
	Median        float64
	P01, P99      float64
	RobustSigma   float64 // 1.4826*MAD
	SatFrac       float64 // >= satLevel
	Full65535Frac float64
	ZeroFrac      float64
}

func histStats(h *[65536]int, n int) Stats {
	s := Stats{N: n}
	if n == 0 {
		return s
	}
	var sum, sum2 float64
	first := true
	for v := 0; v < 65536; v++ {
		c := h[v]
		if c == 0 {
			continue
		}
		if first {
			s.Min = uint16(v)
			first = false
		}
		s.Max = uint16(v)
		fv := float64(v)
		sum += fv * float64(c)
		sum2 += fv * fv * float64(c)
		if v >= satLevel {
			s.SatFrac += float64(c)
		}
	}
	s.Full65535Frac = float64(h[65535]) / float64(n)
	s.ZeroFrac = float64(h[0]) / float64(n)
	s.SatFrac /= float64(n)
	s.Mean = sum / float64(n)
	s.Std = math.Sqrt(math.Max(0, sum2/float64(n)-s.Mean*s.Mean))
	q := func(frac float64) float64 {
		target := int(frac * float64(n-1))
		acc := 0
		for v := 0; v < 65536; v++ {
			acc += h[v]
			if acc > target {
				return float64(v)
			}
		}
		return 65535
	}
	s.Median = q(0.5)
	s.P01 = q(0.01)
	s.P99 = q(0.99)
	// MAD via histogram of |v - median|.
	med := int(s.Median)
	var dh [65536]int
	for v := 0; v < 65536; v++ {
		if h[v] != 0 {
			d := v - med
			if d < 0 {
				d = -d
			}
			dh[d] += h[v]
		}
	}
	target := (n - 1) / 2
	acc := 0
	for d := 0; d < 65536; d++ {
		acc += dh[d]
		if acc > target {
			s.RobustSigma = 1.4826 * float64(d)
			break
		}
	}
	return s
}

func computeStats(pix []uint16) Stats {
	var h [65536]int
	for _, v := range pix {
		h[v]++
	}
	return histStats(&h, len(pix))
}

type Segment struct {
	Row0, Row1 int // inclusive
	Median     float64
	SatFrac    float64
}

type Boundary struct {
	Row    int     // first row of the new level
	Before float64 // median level above
	After  float64 // median level below
}

type Analysis struct {
	W, H          int
	Stats         Stats
	RowMedian     []float64
	RowMean       []float64
	RowSatFrac    []float64
	ColMedian     []float64
	Boundaries    []Boundary
	Segments      []Segment
	FirstSatRow   int // first row with >50% saturated pixels, -1 if none
	BitFrac       [16]float64
	BitN          int
	StuckBits     []string
	LeftMedian    float64
	RightMedian   float64
	EvenColMedian float64
	OddColMedian  float64
	TopMedian     float64
	BottomMedian  float64
	DupRows       int
	ColStepCol    int     // column with the biggest left/right step
	ColStep       float64 // size of that step
	Class         string
	Notes         []string
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	c := slices.Clone(v)
	slices.Sort(c)
	if len(c)%2 == 1 {
		return c[len(c)/2]
	}
	return (c[len(c)/2-1] + c[len(c)/2]) / 2
}

func medianU16(v []uint16) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	c := slices.Clone(v)
	slices.Sort(c)
	if len(c)%2 == 1 {
		return float64(c[len(c)/2])
	}
	return (float64(c[len(c)/2-1]) + float64(c[len(c)/2])) / 2
}

// Analyze computes everything we want to know about one frame.
func Analyze(pix []uint16, w, h int) *Analysis {
	a := &Analysis{W: w, H: h, FirstSatRow: -1}
	if w <= 0 || h <= 0 || len(pix) < w*h {
		a.Class = "EMPTY"
		return a
	}
	a.Stats = computeStats(pix[:w*h])
	a.RowMedian = make([]float64, h)
	a.RowMean = make([]float64, h)
	a.RowSatFrac = make([]float64, h)
	for y := 0; y < h; y++ {
		row := pix[y*w : (y+1)*w]
		a.RowMedian[y] = medianU16(row)
		sum, sat := 0.0, 0
		for _, v := range row {
			sum += float64(v)
			if v >= satLevel {
				sat++
			}
		}
		a.RowMean[y] = sum / float64(w)
		a.RowSatFrac[y] = float64(sat) / float64(w)
		if a.FirstSatRow < 0 && a.RowSatFrac[y] > 0.5 {
			a.FirstSatRow = y
		}
		if y > 0 && slices.Equal(row, pix[(y-1)*w:y*w]) {
			a.DupRows++
		}
	}
	col := make([]uint16, h)
	a.ColMedian = make([]float64, w)
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			col[y] = pix[y*w+x]
		}
		a.ColMedian[x] = medianU16(col)
	}
	// Halves and parity.
	var left, right, even, odd []float64
	for x := 0; x < w; x++ {
		if x < w/2 {
			left = append(left, a.ColMedian[x])
		} else {
			right = append(right, a.ColMedian[x])
		}
		if x%2 == 0 {
			even = append(even, a.ColMedian[x])
		} else {
			odd = append(odd, a.ColMedian[x])
		}
	}
	a.LeftMedian, a.RightMedian = median(left), median(right)
	a.EvenColMedian, a.OddColMedian = median(even), median(odd)
	a.TopMedian, a.BottomMedian = median(a.RowMedian[:h/2]), median(a.RowMedian[h/2:])
	if h == 1 {
		a.TopMedian = a.RowMedian[0]
	}
	a.ColStepCol, a.ColStep = biggestStep(a.ColMedian)
	a.findBoundaries()
	a.bitAnalysis(pix[:w*h])
	a.classify()
	return a
}

// biggestStep finds the largest change between window means on either side of an index.
func biggestStep(p []float64) (int, float64) {
	n := len(p)
	k := n / 50
	if k < 2 {
		k = 2
	}
	if n < 2*k+1 {
		return -1, 0
	}
	best, bestAt := 0.0, -1
	for i := k; i <= n-k; i++ {
		a := median(p[i-k : i])
		b := median(p[i : i+k])
		if d := math.Abs(b - a); d > best {
			best, bestAt = d, i
		}
	}
	return bestAt, best
}

func (a *Analysis) findBoundaries() {
	p := a.RowMedian
	n := len(p)
	k := n / 100
	if k < 2 {
		k = 2
	}
	if n < 2*k+2 {
		a.Segments = []Segment{{0, n - 1, median(p), a.Stats.SatFrac}}
		return
	}
	// Noise of the row profile (robust).
	diffs := make([]float64, 0, n)
	for i := 1; i < n; i++ {
		diffs = append(diffs, math.Abs(p[i]-p[i-1]))
	}
	noise := 1.4826 * median(diffs)
	thr := math.Max(500, 20*noise)
	step := make([]float64, n)
	for i := k; i <= n-k; i++ {
		step[i] = median(p[i:i+k]) - median(p[i-k:i])
	}
	used := make([]bool, n)
	for iter := 0; iter < 6; iter++ {
		bi, bv := -1, 0.0
		for i := k; i <= n-k; i++ {
			if !used[i] && math.Abs(step[i]) > bv {
				bi, bv = i, math.Abs(step[i])
			}
		}
		if bi < 0 || bv < thr {
			break
		}
		// refine: the exact row where the level changes most between adjacent rows near bi
		best, at := -1.0, bi
		for j := max(1, bi-k); j <= min(n-1, bi+k); j++ {
			if d := math.Abs(p[j] - p[j-1]); d > best {
				best, at = d, j
			}
		}
		a.Boundaries = append(a.Boundaries, Boundary{Row: at, Before: median(p[max(0, at-k):at]), After: median(p[at:min(n, at+k)])})
		for j := max(0, bi-2*k); j <= min(n-1, bi+2*k); j++ {
			used[j] = true
		}
	}
	sort.Slice(a.Boundaries, func(i, j int) bool { return a.Boundaries[i].Row < a.Boundaries[j].Row })
	start := 0
	cuts := []int{}
	for _, b := range a.Boundaries {
		cuts = append(cuts, b.Row)
	}
	cuts = append(cuts, n)
	for _, c := range cuts {
		if c <= start {
			continue
		}
		sat := 0.0
		for y := start; y < c; y++ {
			sat += a.RowSatFrac[y]
		}
		a.Segments = append(a.Segments, Segment{Row0: start, Row1: c - 1, Median: median(p[start:c]), SatFrac: sat / float64(c-start)})
		start = c
	}
}

func (a *Analysis) bitAnalysis(pix []uint16) {
	var cnt [16]int
	n := 0
	for _, v := range pix {
		if v == 0 || v >= satLevel {
			continue
		}
		n++
		for b := 0; b < 16; b++ {
			if v&(1<<b) != 0 {
				cnt[b]++
			}
		}
	}
	a.BitN = n
	if n == 0 {
		return
	}
	for b := 0; b < 16; b++ {
		a.BitFrac[b] = float64(cnt[b]) / float64(n)
	}
	// Low bits should be ~50% set whenever noise spans several ADU.
	if n >= 10000 && a.Stats.RobustSigma >= 3 {
		maxBit := int(math.Floor(math.Log2(a.Stats.RobustSigma))) - 1
		if maxBit > 8 {
			maxBit = 8
		}
		for b := 0; b <= maxBit; b++ {
			if a.BitFrac[b] < 0.02 {
				a.StuckBits = append(a.StuckBits, fmt.Sprintf("bit%d stuck at 0 (%.1f%% set)", b, 100*a.BitFrac[b]))
			} else if a.BitFrac[b] > 0.98 {
				a.StuckBits = append(a.StuckBits, fmt.Sprintf("bit%d stuck at 1 (%.1f%% set)", b, 100*a.BitFrac[b]))
			}
		}
	}
}

func (a *Analysis) classify() {
	s := a.Stats
	switch {
	case s.SatFrac >= 0.5 || s.Median >= 50000:
		a.Class = "SATURATED"
	case s.Median < 5 && s.Max < 50:
		a.Class = "ZERO"
	case s.SatFrac >= 0.01 || (len(a.Boundaries) > 0 && maxStep(a.Boundaries) > 5000):
		a.Class = "CORRUPT"
	case len(a.Boundaries) > 0:
		a.Class = "STEP"
	default:
		a.Class = "OK"
	}
	if len(a.StuckBits) > 0 {
		a.Notes = append(a.Notes, "stuck A/D bits: "+strings.Join(a.StuckBits, ", ")+" -> digital data path / A-D converter problem")
	}
	if a.DupRows > a.H/20 && a.H > 20 {
		a.Notes = append(a.Notes, fmt.Sprintf("%d rows are exact copies of the previous row -> data transfer/buffer problem", a.DupRows))
	}
	if d := math.Abs(a.LeftMedian - a.RightMedian); d > math.Max(200, 10*s.RobustSigma) && a.W > 100 {
		a.Notes = append(a.Notes, fmt.Sprintf("left/right halves differ by %.0f ADU (dual-output CCD channel mismatch?)", d))
	}
	if d := math.Abs(a.EvenColMedian - a.OddColMedian); d > math.Max(100, 5*s.RobustSigma) && a.W > 100 {
		a.Notes = append(a.Notes, fmt.Sprintf("even/odd columns differ by %.0f ADU", d))
	}
	if a.FirstSatRow >= 0 && a.FirstSatRow > 0 {
		a.Notes = append(a.Notes, fmt.Sprintf("first row with >50%% saturated pixels: %d of %d (%.0f%% down the readout)", a.FirstSatRow, a.H, 100*float64(a.FirstSatRow)/float64(a.H)))
	}
}

func maxStep(bs []Boundary) float64 {
	m := 0.0
	for _, b := range bs {
		m = math.Max(m, math.Abs(b.After-b.Before))
	}
	return m
}

// --- console rendering ---

var levelChars = []struct {
	lim float64
	ch  byte
}{{2000, '.'}, {5000, ':'}, {10000, '-'}, {20000, '='}, {35000, '+'}, {50000, '*'}, {60000, '#'}, {65000, '%'}, {1e9, '@'}}

func levelChar(v float64) byte {
	for _, l := range levelChars {
		if v < l.lim {
			return l.ch
		}
	}
	return '@'
}

const levelLegend = "scale (ADU):  . <2k   : <5k   - <10k   = <20k   + <35k   * <50k   # <60k   % <65k   @ saturated"

// Thumbnail renders a coarse absolute-scale map of the frame (block means, so a
// thin bad stripe still shows up as an intermediate level).
func Thumbnail(pix []uint16, w, h, cols, rows int) []string {
	if w < cols {
		cols = w
	}
	if h < rows {
		rows = h
	}
	var out []string
	buf := make([]uint16, 0, 4096)
	for r := 0; r < rows; r++ {
		y0, y1 := r*h/rows, (r+1)*h/rows
		var sb strings.Builder
		for c := 0; c < cols; c++ {
			x0, x1 := c*w/cols, (c+1)*w/cols
			buf = buf[:0]
			// subsample large blocks for speed
			sy := max(1, (y1-y0)/24)
			sx := max(1, (x1-x0)/24)
			for y := y0; y < y1; y += sy {
				for x := x0; x < x1; x += sx {
					buf = append(buf, pix[y*w+x])
				}
			}
			sum := 0.0
			for _, v := range buf {
				sum += float64(v)
			}
			sb.WriteByte(levelChar(sum / float64(max(1, len(buf)))))
		}
		out = append(out, fmt.Sprintf("  row %4d |%s|", y0, sb.String()))
	}
	return out
}

func (a *Analysis) Summary() []string {
	s := a.Stats
	var out []string
	out = append(out, fmt.Sprintf("%dx%d  min %d  max %d  mean %.1f  median %.0f  std %.1f  robust-sigma %.1f  p1 %.0f  p99 %.0f",
		a.W, a.H, s.Min, s.Max, s.Mean, s.Median, s.Std, s.RobustSigma, s.P01, s.P99))
	out = append(out, fmt.Sprintf("saturated(>=%d) %.2f%%   ==65535 %.2f%%   ==0 %.2f%%   halves: top %.0f bottom %.0f left %.0f right %.0f",
		satLevel, 100*s.SatFrac, 100*s.Full65535Frac, 100*s.ZeroFrac, a.TopMedian, a.BottomMedian, a.LeftMedian, a.RightMedian))
	if len(a.Segments) > 1 {
		for _, sg := range a.Segments {
			out = append(out, fmt.Sprintf("  rows %4d-%4d: median %7.0f ADU, %5.1f%% saturated", sg.Row0, sg.Row1, sg.Median, 100*sg.SatFrac))
		}
	}
	for _, b := range a.Boundaries {
		out = append(out, fmt.Sprintf("  boundary at row %d: %.0f -> %.0f ADU", b.Row, b.Before, b.After))
	}
	for _, n := range a.Notes {
		out = append(out, "  note: "+n)
	}
	return out
}

func (a *Analysis) BitLine() string {
	if a.BitN == 0 {
		return "bit analysis: no unsaturated non-zero pixels"
	}
	var sb strings.Builder
	sb.WriteString("bits set % (bit0..15, unsaturated pixels):")
	for b := 0; b < 16; b++ {
		fmt.Fprintf(&sb, " %.0f", 100*a.BitFrac[b])
	}
	return sb.String()
}
