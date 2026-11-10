package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func bias(mode uint16) ExposureSpec { return ExposureSpec{Seconds: 0, Dark: true, Mode: mode} }
func dark(sec float64) ExposureSpec { return ExposureSpec{Seconds: sec, Dark: true} }

// nodata: the frame carries no usable image (failed, aborted or incomplete readout).
func nodata(r *FrameResult) bool {
	if r == nil {
		return true
	}
	switch r.Class {
	case "ERROR", "PARTIAL", "ABORTED", "EMPTY":
		return true
	}
	return false
}

// fault: a complete frame that shows the problem. Structure (STEP) only counts for
// closed-shutter frames, where any structure is abnormal.
func fault(r *FrameResult) bool {
	if nodata(r) {
		return false
	}
	switch r.Class {
	case "SATURATED", "CORRUPT", "ZERO", "ELEVATED", "GRADIENT":
		return true
	case "STEP":
		return r.Dark
	}
	return false
}

func withData(rs []*FrameResult) []*FrameResult {
	var out []*FrameResult
	for _, r := range rs {
		if !nodata(r) {
			out = append(out, r)
		}
	}
	return out
}

// series takes n frames of one spec and returns them.
func (s *Session) series(exp string, spec ExposureSpec, n int) []*FrameResult {
	var out []*FrameResult
	for k := 0; k < n && !s.aborted(); k++ {
		if r := s.TakeFrame(exp, spec); r != nil {
			out = append(out, r)
		}
	}
	return out
}

// withDCP runs fn with one driver control parameter temporarily changed, then
// restores exactly that parameter (and verifies it).
func (s *Session) withDCP(i int, v uint32, fn func()) {
	if s.aborted() {
		return
	}
	cur, err := s.cam.GetDCP(i)
	if err != nil {
		s.pf("  skipping: %s is not readable on this driver/camera (%s)\n", dcpNames[i], shortErr(err))
		return
	}
	if cur == v {
		s.pf("  note: %s is already %d - running the frames anyway for comparison\n", dcpNames[i], v)
		fn()
		return
	}
	defer func() {
		if !s.restoreDCP(i) {
			s.halt(dcpNames[i] + " could not be restored")
		}
	}()
	if err := s.SetDCPTemp(i, v); err != nil {
		s.pf("  could not set %s=%d: %v\n", dcpNames[i], v, err)
		return
	}
	fn()
}

// withOptions runs fn with the camera customer options (stored IN the camera)
// temporarily changed; they are restored and verified immediately afterwards.
func (s *Session) withOptions(o CustomerOptions, fn func()) {
	if s.aborted() {
		return
	}
	defer func() {
		if !s.restoreOptions() {
			s.halt("the camera customer options could not be restored")
		}
	}()
	if err := s.SetOptionsTemp(o); err != nil {
		s.pf("  could not change customer options: %v\n", err)
		return
	}
	fn()
}

// heading prints an experiment title; it returns false once the run is aborted,
// so that no further experiment (and no further setting change) is started.
func (s *Session) heading(name, why string) bool {
	if s.aborted() || s.halted.Load() {
		return false
	}
	s.pf("\n==== Experiment: %s ====\n", name)
	if why != "" {
		s.pl("  " + why)
	}
	return true
}

// QuickTest reproduces the reported sequence: a handful of bias frames.
func (s *Session) QuickTest(n int) {
	if !s.heading("baseline bias series", "Repeats the failing sequence (0 s dark frames, 1x1, full frame). "+
		"SBIG sets the black level at the factory to ~900-1000 ADU, so a healthy bias frame has a median near that.") {
		return
	}
	for k := 0; k < n && !s.aborted(); k++ {
		s.TakeFrame("baseline-bias", bias(0))
		s.SampleOffsets(fmt.Sprintf("after baseline frame %d", k+1), 1)
	}
}

// FullTest is the default diagnostic plan. Every changed setting is restored
// right after its experiment, and again at the end. After Ctrl+C / window close
// no new experiment is started.
func (s *Session) FullTest() {
	t0 := time.Now()
	s.QuickTest(4)

	if !s.heading("5 s dark", "Longer exposure: the driver lowers Vdd during exposures >= 3 s (DCP_VDD_OPTIMIZED).") {
		return
	}
	s.series("dark-5s", dark(5), 1)

	if !s.heading("shutter OPEN (light) frames", "Same as the bias but with the shutter open (keep the telescope covered / dome dark). "+
		"In the known STX/STXL RBI-preflash bug, light frames were normal while bias/dark frames were saturated.") {
		return
	}
	lt := bias(0)
	lt.Dark = false
	s.series("light-min", lt, 2)

	if !s.heading("binning 2x2 and 3x3", "Different readout/clocking modes. If one still works there is a usable mode.") {
		return
	}
	s.series("bin2x2", bias(1), 2)
	s.series("bin3x3", bias(2), 1)

	if m, ok := s.cam.Info.Mode(0); ok && m.Height > 400 {
		if !s.heading("subframes", "Top vs bottom 200 rows: is the damage tied to the CCD position or to the readout order/time?") {
			return
		}
		top := bias(0)
		top.Height = 200
		s.series("sub-top200", top, 2)
		bot := bias(0)
		bot.Top = m.Height - 200
		bot.Height = 200
		s.series("sub-bottom200", bot, 2)
	}

	if !s.heading("DCP_VDD_OPTIMIZED = 0", "Vdd (output amplifier supply) is then lowered/raised for every exposure, not only >= 3 s.") {
		return
	}
	s.withDCP(DCP_VDD_OPTIMIZED, 0, func() {
		s.series("vddopt0-bias", bias(0), 3)
		s.series("vddopt0-dark5", dark(5), 2)
	})

	if !s.heading("START_SKIP_VDD", "Keeps Vdd ON during the integration (no lowering at all) for a 5 s dark.") {
		return
	}
	skip := dark(5)
	skip.CCDFlags = START_SKIP_VDD
	s.series("skipvdd-dark5", skip, 2)

	if !s.heading("DCP_USB_FIFO_ENABLE = 0", "Disables the camera FIFO/pipelining: pixels are digitized only when each line is requested.") {
		return
	}
	s.withDCP(DCP_USB_FIFO_ENABLE, 0, func() { s.series("fifo0-bias", bias(0), 3) })

	if !s.heading("DCP_VERT_FLUSH_CONTROL_ENABLE = 0", "Disables background vertical flushing of the KAI CCD during integration (SBIG test setting).") {
		return
	}
	s.withDCP(DCP_VERT_FLUSH_CONTROL_ENABLE, 0, func() { s.series("vflush0-bias", bias(0), 2) })

	if !s.heading("DCP_AUTO_AD_GAIN = 0", "Stops the driver from re-writing the A/D gain register before each readout "+
		"(the camera keeps its power-up A/D settings). Tests whether the driver's A/D writes are involved.") {
		return
	}
	s.withDCP(DCP_AUTO_AD_GAIN, 0, func() { s.series("autoadgain0-bias", bias(0), 3) })

	if v, err := s.cam.GetDCP(DCP_PIXEL_PIPELINE_ENABLE); err == nil && v != 0 {
		if !s.heading("DCP_PIXEL_PIPELINE_ENABLE = 0", "Disables the driver's pixel pipelining (data path test).") {
			return
		}
		s.withDCP(DCP_PIXEL_PIPELINE_ENABLE, 0, func() { s.series("pipeline0-bias", bias(0), 2) })
	}

	if !s.heading("DCP_OVERSCAN_ROWSCOLS = 1", "Adds overscan rows/columns (pixels that never held charge). If overscan is also saturated, "+
		"the fault is in the output amplifier / analog chain / A-D, not charge on the CCD.") {
		return
	}
	s.withDCP(DCP_OVERSCAN_ROWSCOLS, 1, func() {
		old := s.cam.Info
		if info, err := s.cam.CCDInfo(0); err == nil && len(info.Modes) > 0 && old != nil && len(old.Modes) > 0 {
			s.pf("  frame size with overscan: %dx%d (normal %dx%d)\n", info.Modes[0].Width, info.Modes[0].Height, old.Modes[0].Width, old.Modes[0].Height)
		}
		s.series("overscan-bias", bias(0), 1)
	})
	s.cam.CCDInfo(0) // refresh geometry after restore

	// Any other parameter that differs from its documented default.
	var keys []int
	for i := range dcpDocDefault {
		keys = append(keys, i)
	}
	sort.Ints(keys)
	for _, i := range keys {
		def := dcpDocDefault[i]
		// Not toggled: sizes/ratios, and the repair settings that the driver persists in the registry.
		if i == DCP_USB_FIFO_SIZE || i == 2 || i == DCP_WARM_PIXEL_REPAIR_COUNT || i == DCP_COLUMN_REPAIR_ENABLE ||
			i == DCP_WARM_PIXEL_REPAIR_ENABLE || i == DCP_CALL_JOURNAL_ENABLE || i == DCP_TDI_MODE_ENABLE {
			continue
		}
		start, ok := s.dcpStart[i]
		if !ok || int64(start) == def {
			continue
		}
		if !s.heading(fmt.Sprintf("%s back to documented default %d", dcpNames[i], def), "This parameter was not at its documented default when we connected.") {
			return
		}
		ii, dd := i, def
		s.withDCP(ii, uint32(dd), func() { s.series(fmt.Sprintf("default-%s", strings.TrimPrefix(dcpNames[ii], "DCP_")), bias(0), 2) })
	}

	if o, err := s.cam.GetCustomerOptions(); err == nil && o.VddNormallyOff == 0 {
		if !s.heading("camera option vddNormallyOff = 1", "Turns the CCD output-amplifier supply (Vdd) fully OFF between exposures, "+
			"which resets the output amplifier before every frame. Stored in the camera - restored right after.") {
			return
		}
		n := o
		n.VddNormallyOff = 1
		s.withOptions(n, func() {
			s.series("vddoff-bias", bias(0), 3)
			s.series("vddoff-dark5", dark(5), 2)
		})
	}
	if o, err := s.cam.GetCustomerOptions(); err == nil && o.Overscan == 0 {
		if !s.heading("camera option overscanRegions = 1", "Camera-side overscan (pixels that never held charge). "+
			"Saturated overscan => output amplifier / analog chain / A-D fault. Restored right after.") {
			return
		}
		n := o
		n.Overscan = 1
		s.withOptions(n, func() {
			if info, err := s.cam.CCDInfo(0); err == nil && len(info.Modes) > 0 {
				s.pf("  frame size reported with overscan option: %dx%d\n", info.Modes[0].Width, info.Modes[0].Height)
			}
			s.series("custoverscan-bias", bias(0), 1)
		})
		s.cam.CCDInfo(0)
	}
	if o, err := s.cam.GetCustomerOptions(); err == nil && (o.Preflash != 0 || o.VddNormallyOff != 0) {
		if !s.heading("camera customer options to factory defaults", "preflashCcd/vddNormallyOff are set in the camera; test with them off (restored afterwards).") {
			return
		}
		n := o
		n.Preflash, n.VddNormallyOff = 0, 0
		s.withOptions(n, func() { s.series("custopt-default-bias", bias(0), 2) })
	}

	if !s.heading("shutter never moves", "One normal dark (closes the shutter), then 2 frames with SC_LEAVE_SHUTTER: the shutter motor "+
		"is not powered at all. Tests whether the shutter's current pulse (supply sag) or its timing matters.") {
		return
	}
	s.series("shutter-close", bias(0), 1)
	leave := bias(0)
	leave.Leave = true
	s.series("shutter-leave", leave, 2)

	if !s.heading("slow readout", "2 ms pause after every line (readout ~5 s longer). Tests whether the failure depends on readout timing.") {
		return
	}
	slow := bias(0)
	slow.LineDelay = 2 * time.Millisecond
	s.series("slow-readout", slow, 2)

	if !s.heading("re-link", "Close and reopen the camera connection (software re-initialisation, no power cycle).") {
		return
	}
	if err := s.Relink(); err != nil {
		s.pf("  re-link failed: %v\n", err)
	} else {
		s.series("after-relink", bias(0), 3)
	}

	if !s.heading("re-link with DCP_AUTO_AD_GAIN = 0", "Re-initialise the connection while the driver does not touch the A/D gain.") {
		return
	}
	s.withDCP(DCP_AUTO_AD_GAIN, 0, func() {
		if err := s.Relink(); err != nil {
			s.pf("  re-link failed: %v\n", err)
			return
		}
		s.series("relink-autoadgain0", bias(0), 2)
	})

	if !s.heading(fmt.Sprintf("rest %d s", s.restSec), "Leave the camera idle, then one frame. Does the fault decay with time (thermal/charge)?") {
		return
	}
	s.SampleOffsets("before rest", 2)
	for k := 0; k < 10*s.restSec && !s.aborted(); k++ {
		time.Sleep(100 * time.Millisecond)
	}
	s.SampleOffsets("after rest", 2)
	s.series("after-rest", bias(0), 2)

	if !s.heading("final baseline", "All settings restored; repeat the baseline.") {
		return
	}
	s.RestoreAll()
	s.DumpDCPs("Driver control parameters (end, after restore)")
	s.series("final-bias", bias(0), 2)
	s.series("final-dark5", dark(5), 1)
	s.pf("\nFull test duration: %s\n", time.Since(t0).Round(time.Second))
}

func (s *Session) Relink() error {
	if s.aborted() {
		return errAborted
	}
	if err := s.cam.CloseDevice(); err != nil {
		s.pf("  close device: %v\n", err)
	}
	for k := 0; k < 20; k++ { // 2 s, but react to an abort within 100 ms
		if s.aborted() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := s.cam.OpenDevice(s.device, s.ip); err != nil {
		return err
	}
	_, err := s.cam.EstablishLink()
	if err == nil {
		s.cam.CCDInfo(0)
	}
	return err
}

// controlExps are the experiments taken under default conditions (0 s bias, 1x1,
// full frame, all settings at their original values); they show the camera's
// state over time. 5 s dark experiments are compared with the default 5 s dark.
var biasControls = []string{"baseline-bias", "shutter-close", "final-bias"}
var darkControls = []string{"dark-5s", "final-dark5"}

// Experiments that change the camera state for good (not just while a setting is active).
var resettingExps = []string{"after-relink", "relink-autoadgain0", "after-rest"}

func isResetting(exp string) bool {
	for _, e := range resettingExps {
		if e == exp {
			return true
		}
	}
	return false
}

// faultStrict also applies the closed-shutter rules (level, gradient, steps) - used for
// shutter-open frames of a covered telescope before claiming that they are "fine".
func faultStrict(r *FrameResult) bool {
	if fault(r) {
		return true
	}
	if nodata(r) {
		return false
	}
	switch r.StrictClass {
	case "SATURATED", "CORRUPT", "ZERO", "ELEVATED", "GRADIENT", "STEP":
		return true
	}
	return false
}

// Interpret prints observations derived from the results (heuristics, not proof).
func (s *Session) Interpret() {
	s.pl("\n== Observations (automatic, heuristic) ==")
	if s.halted.Load() {
		s.pf("- THE RUN WAS STOPPED EARLY: %s. Settings may not all be at their original values - see 'Restoring settings' below.\n", s.haltReason)
	}
	by := map[string][]*FrameResult{}
	var order []string
	for _, r := range s.results {
		if _, ok := by[r.Experiment]; !ok {
			order = append(order, r.Experiment)
		}
		by[r.Experiment] = append(by[r.Experiment], r)
	}
	countFault := func(rs []*FrameResult) int {
		k := 0
		for _, r := range rs {
			if fault(r) {
				k++
			}
		}
		return k
	}
	controlFrames := func(names []string) []*FrameResult {
		var cs []*FrameResult
		for _, n := range names {
			cs = append(cs, withData(by[n])...)
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].N < cs[j].N })
		return cs
	}
	bctl := controlFrames(biasControls)
	dctl := controlFrames(darkControls)
	prevNext := func(cs []*FrameResult, first, last int) (*FrameResult, *FrameResult) {
		var prev, next *FrameResult
		for _, c := range cs {
			if c.N < first {
				prev = c
			}
			if c.N > last && next == nil {
				next = c
			}
		}
		return prev, next
	}

	// Frames that failed outright.
	for _, r := range s.results {
		if r.Class == "ERROR" || r.Class == "PARTIAL" {
			e := r.FatalErr
			if e == "" && len(r.Errors) > 0 {
				e = r.Errors[len(r.Errors)-1]
			}
			s.pf("- frame %d (%s): no usable image (%s) %s\n", r.N, r.Experiment, r.Class, e)
		}
	}
	all := withData(s.results)
	if len(all) == 0 {
		s.pl("- no usable frames were taken")
		return
	}
	var firstFault *FrameResult
	for _, r := range all {
		if fault(r) {
			firstFault = r
			break
		}
	}
	base := withData(by["baseline-bias"])
	nb := countFault(base)
	switch {
	case len(base) == 0:
		s.pl("- No usable baseline frame.")
	case firstFault == nil:
		s.pl("- No frame shows the fault: it did NOT reproduce in this run.")
		s.pl("  sbigdiag calls the driver strictly one call at a time. NINA 3.2's native SBIG driver does NOT: it can run a")
		s.pl("  temperature query while a frame is being read out (fixed in NINA 3.3 nightly: \"Fixed missing synchronization in")
		s.pl("  native ... SBIG ... SDK calls\", commit f475c75d9f). If the camera stays good here but fails in NINA 3.2,")
		s.pl("  update NINA to 3.3 (nightly) or use the SBIG ASCOM driver in NINA.")
	case nb == 0:
		s.pf("- The baseline frames are fine, but the fault appeared later: first at frame %d (%s, %s).\n", firstFault.N, firstFault.Experiment, firstFault.Class)
	case !fault(base[0]):
		s.pl("- First baseline frame fine, later ones faulty: the fault develops after the first readout (as reported).")
	default:
		s.pf("- %d of %d baseline bias frames are faulty (first: %s).\n", nb, len(base), base[0].Class)
		s.pl("  If the camera was NOT power-cycled since NINA last used it, it may still be in a state NINA left it in")
		s.pl("  (NINA 3.2 can call the SBIG driver from two threads at once during readout; fixed in NINA 3.3 nightly).")
		s.pl("  Decisive test: close NINA, power-cycle the camera, then run sbigdiag FIRST (option 2/3) before NINA connects.")
	}
	if len(base) > 1 {
		var tr []string
		for _, r := range base {
			tr = append(tr, fmt.Sprintf("%.0f", r.Stats.Mean))
		}
		s.pl("  baseline mean ADU sequence: " + strings.Join(tr, " -> "))
	}
	// Did the camera change state during the run (control frames)?
	wentBadAt := -1 // frame number from which later experiments cannot be attributed
	for i := 1; i < len(bctl); i++ {
		if fault(bctl[i-1]) && !fault(bctl[i]) {
			cause := ""
			for _, exp := range resettingExps {
				if rs := withData(by[exp]); len(rs) > 0 && rs[0].N > bctl[i-1].N && rs[0].N < bctl[i].N && countFault(rs) == 0 {
					cause = " - right after the '" + exp + "' step"
					break
				}
			}
			s.pf("- The camera RECOVERED during the run: control frame %d (%s) is fine after faulty frame %d%s.\n", bctl[i].N, bctl[i].Experiment, bctl[i-1].N, cause)
			break
		}
		if !fault(bctl[i-1]) && fault(bctl[i]) {
			first := bctl[i]
			for _, r := range all {
				if r.N > bctl[i-1].N && fault(r) {
					first = r
					break
				}
			}
			wentBadAt = first.N
			s.pf("- The camera went BAD during the run between control frames %d and %d; first faulty frame: %d (%s).\n", bctl[i-1].N, bctl[i].N, first.N, first.Experiment)
			s.pl("  Experiments after that frame cannot be attributed: the camera stayed faulty on its own.")
			break
		}
	}
	if d := withData(by["dark-5s"]); len(d) > 0 && !fault(d[0]) {
		prev, next := prevNext(bctl, d[0].N, d[0].N)
		if prev != nil && fault(prev) && (next == nil || fault(next)) {
			s.pl("- The 5 s dark is fine while the 0 s bias frames around it are faulty: exposure-length dependent (the driver")
			s.pl("  lowers Vdd only for exposures >= 3 s - see the DCP_VDD_OPTIMIZED results).")
		}
	}
	for _, r := range all {
		if r.Exact65535 {
			s.pl("- Frames are EXACTLY 65535 everywhere: a DL forum case with this signature was a USB hub problem (connect the")
			s.pl("  camera directly to the PC / try another cable and port).")
			break
		}
	}
	if co, ok := s.info["customer_options"].(string); ok && strings.Contains(co, "preflashCcd=1") {
		s.pl("- IR RBI PREFLASH IS ON in the camera. Diffraction Limited's first advice for 65535 bias/dark frames on STX/STXL is")
		s.pl("  to turn the IR preflash OFF (CCDOps: Camera Setup -> Customer Options), and to keep driver and camera firmware")
		s.pl("  versions matched (Driver Checker 64) - RBI preflash 'gets borked' when they are not updated together.")
	}
	var pre []string
	for _, r := range all {
		if r.PreExpS > 0.5 && len(pre) < 8 {
			pre = append(pre, fmt.Sprintf("frame %d %.1fs", r.N, r.PreExpS))
		}
	}
	if len(pre) > 0 {
		s.pl("- Long pre-exposure phase (RBI preflash or slow shutter): " + strings.Join(pre, ", "))
	}
	for _, o := range s.offsets {
		if o.Err == "" && o.Offset >= 60000 {
			s.pl("- The camera's own offset measurement (CC_READ_OFFSET) is near full scale: the analog chain / A-D is saturated even")
			s.pl("  without an image readout. So it is not a USB transfer problem; it is either a hardware fault (output amplifier,")
			s.pl("  bias voltages, A-D) or A-D/clocking state left bad in the camera (only a power cycle resets that).")
			break
		}
	}
	creditedReset := ""
	cmp := func(exp, what string, controls []*FrameResult) {
		rsAll := by[exp]
		if len(rsAll) == 0 {
			return
		}
		rs := withData(rsAll)
		if len(rs) == 0 {
			s.pf("- %s: no usable frame (%d failed/aborted) - no conclusion\n", exp, len(rsAll))
			return
		}
		isFault := fault
		if exp == "light-min" {
			isFault = faultStrict // shutter-open frames of a covered telescope must look like bias frames
		}
		k := 0
		for _, r := range rs {
			if isFault(r) {
				k++
			}
		}
		prev, next := prevNext(controls, rs[0].N, rs[len(rs)-1].N)
		if next == nil {
			// e.g. a 5 s dark experiment with no later 5 s dark control: use the bias controls
			_, next = prevNext(bctl, rs[0].N, rs[len(rs)-1].N)
		}
		prevBad := prev != nil && fault(prev)
		nextBad := next != nil && fault(next)
		switch {
		case k == 0 && prevBad && isResetting(exp):
			// re-link / rest change the camera state for good: judge against the frame before only
			if creditedReset == "" {
				creditedReset = exp
				hedge := ""
				if len(rs) == 1 {
					hedge = " (only 1 usable frame - suggestive)"
				}
				s.pf("- %s: ALL %d frame(s) GOOD right after this step, while the camera was faulty before (frame %d)%s -> %s\n", exp, len(rs), prev.N, hedge, what)
			} else {
				s.pf("- %s: good (the camera was already recovered by '%s')\n", exp, creditedReset)
			}
		case k == 0 && prevBad && len(rs) == 1 && (next == nil || nextBad):
			s.pf("- %s: 1 good frame while the camera was faulty before (frame %d) - suggestive only, not confirmed (%s?)\n", exp, prev.N, what)
		case k == 0 && prevBad && (next == nil || nextBad):
			after := ""
			if next != nil {
				after = fmt.Sprintf(" and after (frame %d)", next.N)
			}
			s.pf("- %s: ALL %d frame(s) GOOD while the camera was faulty before (frame %d)%s -> %s\n", exp, len(rs), prev.N, after, what)
		case k == 0 && prevBad:
			s.pf("- %s: good, but the next control frame %d is good too: the camera may have recovered (inconclusive)\n", exp, next.N)
		case k == 0:
			// camera was fine before: nothing learned
		case k < len(rs):
			var seq []string
			for _, r := range rs {
				c := r.Class
				if exp == "light-min" && r.StrictClass != "" {
					c = r.StrictClass // the class that was judged
				}
				seq = append(seq, c)
			}
			extra := ""
			if !isFault(rs[0]) && isFault(rs[len(rs)-1]) {
				if prevBad {
					extra = " - good right after the change, then faulty again (transient, like after power-on)"
				} else {
					extra = " - the camera went bad during this experiment"
				}
			}
			s.pf("- %s: %d of %d frame(s) faulty: %s%s\n", exp, k, len(rs), strings.Join(seq, " -> "), extra)
		case prev != nil && !prevBad && next != nil && !nextBad:
			s.pf("- %s: FAULTY while the camera is fine before (frame %d) and after (frame %d) -> this mode/setting TRIGGERS the fault\n", exp, prev.N, next.N)
		case wentBadAt >= 0 && rs[0].N > wentBadAt:
			// after the camera went bad on its own: nothing to attribute (already reported once)
		default:
			s.pf("- %s: still faulty\n", exp)
		}
	}
	cmp("light-min", "shutter-open frames are fine while closed-shutter frames are not: the signature of the STX/STXL RBI-preflash / "+
		"driver-firmware mismatch bug (only meaningful if the telescope/dome was dark)", bctl)
	cmp("bin2x2", "binned readout works: a usable workaround mode and a clue to the clocking/readout path", bctl)
	cmp("bin3x3", "3x3 binned readout works", bctl)
	cmp("sub-top200", "top rows are fine", bctl)
	cmp("sub-bottom200", "bottom rows are fine", bctl)
	cmp("vddopt0-bias", "DCP_VDD_OPTIMIZED=0 FIXES short exposures (driver Vdd switching is involved)", bctl)
	cmp("vddopt0-dark5", "DCP_VDD_OPTIMIZED=0 fixes 5 s darks", dctl)
	cmp("skipvdd-dark5", "keeping Vdd on during integration fixes 5 s darks", dctl)
	cmp("fifo0-bias", "disabling the USB FIFO fixes it (pipelining/data path issue)", bctl)
	cmp("vflush0-bias", "disabling vertical flush control fixes it", bctl)
	cmp("autoadgain0-bias", "with DCP_AUTO_AD_GAIN=0 the frames are good: the driver's A/D gain writes are involved", bctl)
	cmp("pipeline0-bias", "disabling the pixel pipeline fixes it", bctl)
	cmp("custopt-default-bias", "CAMERA CUSTOMER OPTIONS were the cause", bctl)
	cmp("vddoff-bias", "switching Vdd off between frames (camera option vddNormallyOff=1) FIXES bias frames - a usable workaround (set it in CCDOps)", bctl)
	cmp("vddoff-dark5", "vddNormallyOff=1 fixes 5 s darks", dctl)
	cmp("custoverscan-bias", "the overscan-option frame is fine", bctl)
	cmp("shutter-leave", "frames are good when the shutter does not move (shutter motor current / timing involved)", bctl)
	cmp("slow-readout", "slower readout fixes it (timing / power-supply droop during fast readout?)", bctl)
	cmp("after-relink", "a software re-link clears the fault (camera state problem; reconnecting before each frame is a workaround)", bctl)
	cmp("relink-autoadgain0", "re-link with auto A/D gain off clears the fault", bctl)
	cmp("after-rest", "resting idle clears the fault (thermal or slowly-recovering supply?)", bctl)
	for _, exp := range order {
		if strings.HasPrefix(exp, "default-") {
			cmp(exp, "resetting this parameter to its documented default fixes it", bctl)
		}
	}
	if rs := withData(by["overscan-bias"]); len(rs) > 0 {
		s.pl("- Overscan frame: check the right-most columns / bottom rows in its _rows.csv / FITS; saturated overscan = analog chain fault.")
	}
	if t, b := withData(by["sub-top200"]), withData(by["sub-bottom200"]); len(t) > 0 && len(b) > 0 {
		if !fault(t[0]) && fault(b[0]) {
			s.pl("- Bottom rows faulty but top rows fine even in small subframes -> position on the CCD matters (not readout time).")
		}
		if fault(t[0]) && fault(b[0]) {
			s.pl("- Both small subframes faulty -> not a cumulative 'runs out during long readout' effect.")
		}
	}
	for _, r := range s.results {
		for _, n := range r.Notes {
			if strings.Contains(n, "stuck A/D bits") || strings.Contains(n, "exact copies") {
				s.pf("- frame %d: %s\n", r.N, n)
			}
		}
		if r.ComValid && r.ComFailed > 0 {
			s.pf("- frame %d: link comFailed increased by %d (USB communication errors)\n", r.N, r.ComFailed)
		}
		if r.PowerLow == "true" {
			s.pf("- frame %d: POWER-SUPPLY-LOW bit set after readout -> check the 12 V supply/cable\n", r.N)
		}
	}
	seen := map[string]int{}
	for _, r := range all {
		if r.SHA256 == "" || r.Stats.Min == r.Stats.Max {
			continue // constant frames (e.g. all 65535) are trivially identical
		}
		if first, ok := seen[r.SHA256]; ok {
			s.pf("- frame %d is BIT-IDENTICAL to frame %d: the driver/camera delivered the same image twice (stale buffer / transfer path)\n", r.N, first)
			continue
		}
		seen[r.SHA256] = r.N
	}
	s.pl("Send the whole output folder (report.txt, summary.json, frames.csv, PNGs; FITS if possible) for analysis.")
}

// fullTestFrames is the approximate number of full-size 1x1 frames FullTest takes.
const fullTestFrames = 50

// preflight prints the plan, checks the free disk space (FITS are dropped if it is
// short) and, when the operator chose the run in the menu, asks for confirmation:
// the full test opens the shutter and briefly changes camera options.
func (s *Session) preflight(o options, interactive bool) bool {
	frames, mins := fullTestFrames, 12
	what := "the FULL test"
	switch {
	case o.custom != "":
		frames, mins, what = o.frames, 1+o.frames/4, "a custom series"
	case o.quick:
		frames, mins, what = o.frames, 1+o.frames/4, "the quick test"
	}
	perFrame := uint64(2*4008*2672 + 700_000) // FITS + PNG + CSV of a full 1x1 frame
	if s.cam != nil && s.cam.Info != nil {
		if m, ok := s.cam.Info.Mode(0); ok && m.Width > 0 {
			perFrame = uint64(2*m.Width*m.Height + 700_000)
		}
	}
	need := uint64(frames) * perFrame
	s.pf("\n== Plan: %s, about %d frames, about %d min, about %.1f GB of files ==\n", what, frames, mins, float64(need)/1e9)
	if what == "the FULL test" {
		s.pl("  * Keep the telescope covered / the dome dark: the shutter opens for 2 short frames.")
		opts := "vddNormallyOff, overscan"
		if o, err := s.cam.GetCustomerOptions(); err == nil && o.Preflash != 0 {
			opts = "vddNormallyOff, overscan and preflashCcd (now ON - tested OFF)"
		}
		if s.journalBlock != "" {
			opts = "none (" + s.journalBlock + ")"
		}
		s.pl("  * Driver settings and camera options (" + opts + ") are changed briefly, one at a time,")
		s.pl("    and put back right after each experiment. Cooling, fan and filter wheel are not touched.")
	}
	if s.dir != "" && s.saveFITS {
		if free, ok := freeBytes(s.dir); ok && free < need+1_000_000_000 {
			s.saveFITS = false
			s.pf("  ! Only %.1f GB free on the output drive: FITS files are NOT saved (PNG/CSV/summary are).\n", float64(free)/1e9)
		}
	}
	if !interactive {
		return true
	}
	if !s.confirm("Start now?") {
		s.pl("Cancelled before the first exposure.")
		return false
	}
	return true
}
