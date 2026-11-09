package main

import (
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

func synth(w, h int, f func(x, y int, r *rand.Rand) float64) []uint16 {
	r := rand.New(rand.NewSource(7))
	p := make([]uint16, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p[y*w+x] = clamp16(f(x, y, r))
		}
	}
	return p
}

func TestHealthyBias(t *testing.T) {
	p := synth(400, 300, func(x, y int, r *rand.Rand) float64 { return 1000 + 12*r.NormFloat64() })
	a := Analyze(p, 400, 300)
	if a.Class != "OK" {
		t.Fatalf("class %s, notes %v, boundaries %v", a.Class, a.Notes, a.Boundaries)
	}
	if math.Abs(a.Stats.Median-1000) > 2 || math.Abs(a.Stats.RobustSigma-12) > 1.5 {
		t.Errorf("median %v sigma %v", a.Stats.Median, a.Stats.RobustSigma)
	}
	if len(a.StuckBits) != 0 {
		t.Errorf("false stuck bits: %v", a.StuckBits)
	}
}

func TestBoundaryDetection(t *testing.T) {
	p := synth(400, 300, func(x, y int, r *rand.Rand) float64 {
		if y >= 180 {
			return 60000 + 300*r.NormFloat64()
		}
		return 1000 + 12*r.NormFloat64()
	})
	a := Analyze(p, 400, 300)
	if a.Class != "CORRUPT" {
		t.Errorf("class %s", a.Class)
	}
	if len(a.Boundaries) == 0 || a.Boundaries[0].Row != 180 {
		t.Fatalf("boundaries %+v", a.Boundaries)
	}
	if len(a.Segments) != 2 || a.Segments[0].Row1 != 179 {
		t.Errorf("segments %+v", a.Segments)
	}
}

func TestSaturated(t *testing.T) {
	p := synth(200, 100, func(x, y int, r *rand.Rand) float64 { return 65535 })
	if a := Analyze(p, 200, 100); a.Class != "SATURATED" {
		t.Errorf("class %s", a.Class)
	}
}

func TestStuckBit(t *testing.T) {
	p := synth(300, 200, func(x, y int, r *rand.Rand) float64 { return float64(int(1000+40*r.NormFloat64()) &^ 2) })
	a := Analyze(p, 300, 200)
	if len(a.StuckBits) == 0 {
		t.Errorf("stuck bit 1 not detected: %v", a.BitFrac)
	}
}

func TestFITSRoundTrip(t *testing.T) {
	p := synth(37, 23, func(x, y int, r *rand.Rand) float64 { return float64((x*977 + y*131) % 65536) })
	p[0], p[1] = 0, 65535
	fn := filepath.Join(t.TempDir(), "t.fits")
	if err := WriteFITS(fn, p, 37, 23, []fitsCard{{"EXPTIME", 1.5, "s"}, {"IMAGETYP", "BIAS", ""}}); err != nil {
		t.Fatal(err)
	}
	q, w, h, hdr, err := ReadFITS(fn)
	if err != nil || w != 37 || h != 23 {
		t.Fatalf("read: %v %dx%d", err, w, h)
	}
	for i := range p {
		if p[i] != q[i] {
			t.Fatalf("pixel %d: %d != %d", i, p[i], q[i])
		}
	}
	if hdr["IMAGETYP"] != "BIAS" || hdr["EXPTIME"] != "1.5" {
		t.Errorf("header %v", hdr)
	}
}

func TestParseCustom(t *testing.T) {
	s, err := parseCustom("exp=5,bin=2,dark=0,top=10,height=20,skipvdd=1,linedelay=2ms")
	if err != nil || s.Seconds != 5 || s.Mode != 1 || s.Dark || s.Top != 10 || s.Height != 20 || s.CCDFlags != START_SKIP_VDD || s.LineDelay.Milliseconds() != 2 {
		t.Errorf("%+v %v", s, err)
	}
}
