package main

import (
	"bytes"
	"strings"
	"testing"
)

// plan is the FullTest order of experiments with their frame counts (shutter
// state per experiment), used to build synthetic result sequences.
var plan = []struct {
	exp  string
	n    int
	dark bool
}{
	{"baseline-bias", 4, true}, {"dark-5s", 1, true}, {"light-min", 2, false}, {"bin2x2", 2, true}, {"bin3x3", 1, true},
	{"sub-top200", 2, true}, {"sub-bottom200", 2, true}, {"vddopt0-bias", 2, true}, {"vddopt0-dark5", 1, true},
	{"skipvdd-dark5", 1, true}, {"fifo0-bias", 2, true}, {"vflush0-bias", 2, true}, {"autoadgain0-bias", 2, true},
	{"vddoff-bias", 2, true}, {"vddoff-dark5", 1, true}, {"shutter-close", 1, true}, {"shutter-leave", 2, true},
	{"slow-readout", 1, true}, {"after-relink", 2, true}, {"relink-autoadgain0", 2, true}, {"after-rest", 1, true},
	{"final-bias", 2, true}, {"final-dark5", 1, true},
}

// run builds a session whose frame classes come from classOf(frame number, experiment).
func runPlan(t *testing.T, classOf func(n int, exp string) string) string {
	t.Helper()
	var buf bytes.Buffer
	s := &Session{out: &buf, info: map[string]interface{}{}}
	n := 0
	for _, p := range plan {
		for k := 0; k < p.n; k++ {
			n++
			c := classOf(n, p.exp)
			r := &FrameResult{N: n, Experiment: p.exp, Spec: "x", Class: c, StrictClass: c, Dark: p.dark}
			r.Stats.Median, r.Stats.Mean = 1000, 1000
			if c == "SATURATED" {
				r.Stats.Median, r.Stats.Mean = 65535, 60000
			}
			s.results = append(s.results, r)
		}
	}
	s.Interpret()
	return buf.String()
}

func TestInterpretWentBadOnItsOwn(t *testing.T) {
	out := runPlan(t, func(n int, exp string) string {
		if n >= 6 {
			return "SATURATED"
		}
		return "OK"
	})
	if strings.Contains(out, "TRIGGERS") {
		t.Errorf("must not blame later experiments:\n%s", out)
	}
	if !strings.Contains(out, "went BAD during the run") || !strings.Contains(out, "first faulty frame: 6") {
		t.Errorf("missing went-bad report:\n%s", out)
	}
}

func TestInterpretRelinkFixes(t *testing.T) {
	relinkAt := 0
	out := runPlan(t, func(n int, exp string) string {
		if exp == "after-relink" && relinkAt == 0 {
			relinkAt = n
		}
		if relinkAt > 0 && n >= relinkAt {
			return "OK"
		}
		return "SATURATED"
	})
	if !strings.Contains(out, "after-relink: ALL 2 frame(s) GOOD right after this step") {
		t.Errorf("re-link fix not credited:\n%s", out)
	}
	if strings.Contains(out, "after-relink: good, but") || strings.Contains(out, "after-rest: ALL") {
		t.Errorf("re-link reported as inconclusive or rest credited:\n%s", out)
	}
	if !strings.Contains(out, "right after the 'after-relink' step") {
		t.Errorf("recovery not attributed to the re-link:\n%s", out)
	}
}

func TestInterpretSpontaneousRecoveryNoDarkFixClaims(t *testing.T) {
	out := runPlan(t, func(n int, exp string) string {
		if n >= 15 {
			return "OK"
		}
		return "SATURATED"
	})
	for _, bad := range []string{"vddopt0-dark5: ALL", "skipvdd-dark5: ALL", "vddoff-dark5: ALL"} {
		if strings.Contains(out, bad) {
			t.Errorf("false fix claim %q after a spontaneous recovery:\n%s", bad, out)
		}
	}
}

func TestInterpretLightFramesNeedBiasRules(t *testing.T) {
	var buf bytes.Buffer
	s := &Session{out: &buf, info: map[string]interface{}{}}
	add := func(n int, exp, class, strict string, dark bool) {
		r := &FrameResult{N: n, Experiment: exp, Spec: "x", Class: class, StrictClass: strict, Dark: dark}
		r.Stats.Median = 1000
		s.results = append(s.results, r)
	}
	add(1, "baseline-bias", "SATURATED", "SATURATED", true)
	add(2, "light-min", "OK", "ELEVATED", false) // looks OK for a light frame, but not like a bias frame
	add(3, "shutter-close", "SATURATED", "SATURATED", true)
	s.Interpret()
	if strings.Contains(buf.String(), "RBI-preflash") {
		t.Errorf("RBI signature claimed for elevated shutter-open frames:\n%s", buf.String())
	}
}

func TestInterpretExposureLengthUsesNeighbours(t *testing.T) {
	out := runPlan(t, func(n int, exp string) string {
		if n == 1 {
			return "SATURATED" // only the very first frame is bad
		}
		return "OK"
	})
	if strings.Contains(out, "exposure-length dependent") {
		t.Errorf("exposure-length verdict although the neighbouring bias frames are fine:\n%s", out)
	}
}
