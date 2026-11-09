package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func bias(mode uint16) ExposureSpec { return ExposureSpec{Seconds: 0, Dark: true, Mode: mode} }
func dark(sec float64) ExposureSpec { return ExposureSpec{Seconds: sec, Dark: true} }

func bad(r *FrameResult) bool {
	return r != nil && (r.Class == "SATURATED" || r.Class == "CORRUPT" || r.Class == "ERROR")
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

// withDCP runs fn with a driver control parameter temporarily changed, then restores it.
func (s *Session) withDCP(i int, v uint32, fn func()) {
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
	if err := s.SetDCPTemp(i, v); err != nil {
		s.pf("  could not set %s=%d: %v\n", dcpNames[i], v, err)
		s.RestoreAll()
		return
	}
	defer s.RestoreAll()
	fn()
}

func (s *Session) heading(name, why string) {
	s.pf("\n==== Experiment: %s ====\n", name)
	if why != "" {
		s.pl("  " + why)
	}
}

// QuickTest reproduces the reported sequence: a handful of bias frames.
func (s *Session) QuickTest(n int) {
	s.heading("baseline bias series", "Repeats the failing sequence (0 s dark frames, 1x1, full frame). "+
		"SBIG sets the black level at the factory to ~900-1000 ADU, so a healthy bias frame has a median near that.")
	for k := 0; k < n && !s.aborted(); k++ {
		s.TakeFrame("baseline-bias", bias(0))
		s.SampleOffsets(fmt.Sprintf("after baseline frame %d", k+1), 1)
	}
}

// FullTest is the default diagnostic plan. Every changed setting is restored
// right after its experiment, and again at the end.
func (s *Session) FullTest() {
	t0 := time.Now()
	s.QuickTest(4)
	if s.aborted() {
		return
	}
	s.heading("5 s dark", "Longer exposure: the driver lowers Vdd during exposures >= 3 s (DCP_VDD_OPTIMIZED).")
	s.series("dark-5s", dark(5), 1)

	s.heading("binning 2x2 and 3x3", "Different readout/clocking modes. If one still works there is a usable mode.")
	s.series("bin2x2", bias(1), 2)
	s.series("bin3x3", bias(2), 1)

	if m, ok := s.cam.Info.Mode(0); ok && m.Height > 400 {
		s.heading("subframes", "Top vs bottom 200 rows: is the damage tied to the CCD position or to the readout order/time?")
		top := bias(0)
		top.Height = 200
		s.series("sub-top200", top, 2)
		bot := bias(0)
		bot.Top = m.Height - 200
		bot.Height = 200
		s.series("sub-bottom200", bot, 2)
	}

	s.heading("DCP_VDD_OPTIMIZED = 0", "Vdd (output amplifier supply) is then lowered/raised for every exposure, not only >= 3 s.")
	s.withDCP(DCP_VDD_OPTIMIZED, 0, func() {
		s.series("vddopt0-bias", bias(0), 2)
		s.series("vddopt0-dark5", dark(5), 1)
	})

	s.heading("START_SKIP_VDD", "Keeps Vdd ON during the integration (no lowering at all) for a 5 s dark.")
	skip := dark(5)
	skip.CCDFlags = START_SKIP_VDD
	s.series("skipvdd-dark5", skip, 1)

	s.heading("DCP_USB_FIFO_ENABLE = 0", "Disables the camera FIFO/pipelining: pixels are digitized only when each line is requested.")
	s.withDCP(DCP_USB_FIFO_ENABLE, 0, func() { s.series("fifo0-bias", bias(0), 2) })

	s.heading("DCP_VERT_FLUSH_CONTROL_ENABLE = 0", "Disables background vertical flushing of the KAI CCD during integration (SBIG test setting).")
	s.withDCP(DCP_VERT_FLUSH_CONTROL_ENABLE, 0, func() { s.series("vflush0-bias", bias(0), 2) })

	s.heading("DCP_AUTO_AD_GAIN = 0", "Stops the driver from re-writing the A/D gain register before each readout "+
		"(the camera keeps its power-up A/D settings). Tests whether the driver's A/D writes are involved.")
	s.withDCP(DCP_AUTO_AD_GAIN, 0, func() { s.series("autoadgain0-bias", bias(0), 2) })

	if v, err := s.cam.GetDCP(DCP_PIXEL_PIPELINE_ENABLE); err == nil && v != 0 {
		s.heading("DCP_PIXEL_PIPELINE_ENABLE = 0", "Disables the driver's pixel pipelining (data path test).")
		s.withDCP(DCP_PIXEL_PIPELINE_ENABLE, 0, func() { s.series("pipeline0-bias", bias(0), 2) })
	}

	s.heading("DCP_OVERSCAN_ROWSCOLS = 1", "Adds overscan rows/columns (pixels that never held charge). If overscan is also saturated, "+
		"the fault is in the output amplifier / analog chain / A-D, not charge on the CCD.")
	s.withDCP(DCP_OVERSCAN_ROWSCOLS, 1, func() {
		old := s.cam.Info
		if info, err := s.cam.CCDInfo(0); err == nil && len(info.Modes) > 0 {
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
		s.heading(fmt.Sprintf("%s back to documented default %d", dcpNames[i], def), "This parameter was not at its documented default when we connected.")
		ii, dd := i, def
		s.withDCP(ii, uint32(dd), func() { s.series(fmt.Sprintf("default-%s", strings.TrimPrefix(dcpNames[ii], "DCP_")), bias(0), 2) })
	}

	if o, err := s.cam.GetCustomerOptions(); err == nil && o.VddNormallyOff == 0 {
		s.heading("camera option vddNormallyOff = 1", "Turns the CCD output-amplifier supply (Vdd) fully OFF between exposures, "+
			"which resets the output amplifier before every frame. Stored in the camera - restored right after.")
		n := o
		n.VddNormallyOff = 1
		if err := s.SetOptionsTemp(n); err != nil {
			s.pf("  could not change customer options: %v\n", err)
		} else {
			s.series("vddoff-bias", bias(0), 2)
			s.series("vddoff-dark5", dark(5), 1)
		}
		s.RestoreAll()
	}
	if o, err := s.cam.GetCustomerOptions(); err == nil && o.Overscan == 0 {
		s.heading("camera option overscanRegions = 1", "Camera-side overscan (pixels that never held charge). "+
			"Saturated overscan => output amplifier / analog chain / A-D fault. Restored right after.")
		n := o
		n.Overscan = 1
		if err := s.SetOptionsTemp(n); err != nil {
			s.pf("  could not change customer options: %v\n", err)
		} else {
			if info, err := s.cam.CCDInfo(0); err == nil && len(info.Modes) > 0 {
				s.pf("  frame size reported with overscan option: %dx%d\n", info.Modes[0].Width, info.Modes[0].Height)
			}
			s.series("custoverscan-bias", bias(0), 1)
		}
		s.RestoreAll()
		s.cam.CCDInfo(0)
	}
	if o, err := s.cam.GetCustomerOptions(); err == nil && (o.Preflash != 0 || o.VddNormallyOff != 0) {
		s.heading("camera customer options to factory defaults", "preflashCcd/vddNormallyOff are set in the camera; test with them off (restored afterwards).")
		n := o
		n.Preflash, n.VddNormallyOff = 0, 0
		if err := s.SetOptionsTemp(n); err != nil {
			s.pf("  could not change customer options: %v\n", err)
		} else {
			s.series("custopt-default-bias", bias(0), 2)
		}
		s.RestoreAll()
	}

	s.heading("shutter never moves", "One normal dark (closes the shutter), then 2 frames with SC_LEAVE_SHUTTER: the shutter motor "+
		"is not powered at all. Tests whether the shutter's current pulse (supply sag) or its timing matters.")
	s.series("shutter-close", bias(0), 1)
	leave := bias(0)
	leave.Leave = true
	s.series("shutter-leave", leave, 2)

	s.heading("slow readout", "2 ms pause after every line (readout ~5 s longer). Tests whether the failure depends on readout timing.")
	slow := bias(0)
	slow.LineDelay = 2 * time.Millisecond
	s.series("slow-readout", slow, 1)

	s.heading("re-link", "Close and reopen the camera connection (software re-initialisation, no power cycle).")
	if err := s.Relink(); err != nil {
		s.pf("  re-link failed: %v\n", err)
	} else {
		s.series("after-relink", bias(0), 2)
	}

	s.heading("re-link with DCP_AUTO_AD_GAIN = 0", "Re-initialise the connection while the driver does not touch the A/D gain.")
	s.withDCP(DCP_AUTO_AD_GAIN, 0, func() {
		if err := s.Relink(); err != nil {
			s.pf("  re-link failed: %v\n", err)
			return
		}
		s.series("relink-autoadgain0", bias(0), 2)
	})

	s.heading(fmt.Sprintf("rest %d s", s.restSec), "Leave the camera idle, then one frame. Does the fault decay with time (thermal/charge)?")
	s.SampleOffsets("before rest", 2)
	for k := 0; k < s.restSec && !s.aborted(); k++ {
		time.Sleep(time.Second)
	}
	s.SampleOffsets("after rest", 2)
	s.series("after-rest", bias(0), 1)

	s.heading("final baseline", "All settings restored; repeat the baseline.")
	s.RestoreAll()
	s.DumpDCPs("Driver control parameters (end, after restore)")
	s.series("final-bias", bias(0), 2)
	s.pf("\nFull test duration: %s\n", time.Since(t0).Round(time.Second))
}

func (s *Session) Relink() error {
	if err := s.cam.CloseDevice(); err != nil {
		s.pf("  close device: %v\n", err)
	}
	time.Sleep(2 * time.Second)
	if err := s.cam.OpenDevice(s.device, s.ip); err != nil {
		return err
	}
	_, err := s.cam.EstablishLink()
	if err == nil {
		s.cam.CCDInfo(0)
	}
	return err
}

// Interpret prints observations derived from the results (heuristics, not proof).
func (s *Session) Interpret() {
	s.pl("\n== Observations (automatic, heuristic) ==")
	by := map[string][]*FrameResult{}
	var order []string
	for _, r := range s.results {
		if _, ok := by[r.Experiment]; !ok {
			order = append(order, r.Experiment)
		}
		by[r.Experiment] = append(by[r.Experiment], r)
	}
	nbad := func(rs []*FrameResult) int {
		k := 0
		for _, r := range rs {
			if bad(r) {
				k++
			}
		}
		return k
	}
	base := by["baseline-bias"]
	if len(base) == 0 {
		s.pl("- no baseline frames were taken")
		return
	}
	nb := nbad(base)
	switch {
	case nb == 0:
		s.pl("- The baseline bias frames are all fine: the fault did NOT reproduce in this run.")
		s.pl("  sbigdiag calls the driver strictly one call at a time. NINA 3.2's native SBIG driver does NOT: it can run a")
		s.pl("  temperature query while a frame is being read out (fixed in NINA 3.3 nightly: \"Fixed missing synchronization in")
		s.pl("  native ... SBIG ... SDK calls\", commit f475c75d9f). If the camera now stays good here but fails in NINA 3.2,")
		s.pl("  update NINA to 3.3 (nightly) or use the SBIG ASCOM driver in NINA.")
	case !bad(base[0]) && nb > 0:
		s.pl("- First baseline frame fine, later ones bad: the fault develops after the first readout (as reported).")
	default:
		s.pf("- %d of %d baseline bias frames are bad (first frame: %s).\n", nb, len(base), base[0].Class)
		s.pl("  If the camera was NOT power-cycled since NINA last used it, it may still be in a state NINA left it in")
		s.pl("  (NINA 3.2 can call the SBIG driver from two threads at once during readout; fixed in NINA 3.3 nightly).")
		s.pl("  Decisive test: close NINA, power-cycle the camera, then run sbigdiag FIRST (option 3) before NINA connects.")
	}
	for _, r := range base {
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
	for _, r := range base {
		if r.PreExpS > 0.5 {
			pre = append(pre, fmt.Sprintf("frame %d %.1fs", r.N, r.PreExpS))
		}
	}
	if len(pre) > 0 {
		s.pl("- Long pre-exposure phase (RBI preflash or slow shutter): " + strings.Join(pre, ", "))
	}
	if med := base[0].Stats.Median; base[0].Class != "ERROR" && (med < 300 || med > 3000) {
		s.pf("- First baseline frame median %.0f ADU is far from the factory black level (~900-1000 ADU).\n", med)
	}
	if len(base) > 1 {
		var tr []string
		for _, r := range base {
			tr = append(tr, fmt.Sprintf("%.0f", r.Stats.Mean))
		}
		s.pl("  baseline mean ADU sequence: " + strings.Join(tr, " -> "))
	}
	// Offsets measured by the camera.
	for _, o := range s.offsets {
		if o.Err == "" && o.Offset >= 60000 {
			s.pl("- The camera's own offset measurement (CC_READ_OFFSET) is near full scale: the analog chain / A-D is saturated even")
			s.pl("  without an image readout. So it is not a USB transfer problem; it is either a hardware fault (output amplifier,")
			s.pl("  bias voltages, A-D) or A-D/clocking state left bad in the camera (only a power cycle resets that).")
			break
		}
	}
	cmp := func(exp, what string) {
		rs := by[exp]
		if len(rs) == 0 || nb == 0 {
			return
		}
		k := nbad(rs)
		switch {
		case k == 0:
			s.pf("- %s: ALL %d frame(s) GOOD while baseline was bad -> %s\n", exp, len(rs), what)
		case k < len(rs):
			s.pf("- %s: %d of %d frame(s) bad (partial improvement?)\n", exp, k, len(rs))
		default:
			s.pf("- %s: still bad\n", exp)
		}
	}
	cmp("bin2x2", "binned readout works: a usable workaround mode and a clue to the clocking/readout path")
	cmp("bin3x3", "3x3 binned readout works")
	cmp("sub-top200", "top rows are fine")
	cmp("sub-bottom200", "bottom rows are fine")
	cmp("vddopt0-bias", "DCP_VDD_OPTIMIZED=0 FIXES short exposures (driver Vdd switching is involved)")
	cmp("vddopt0-dark5", "DCP_VDD_OPTIMIZED=0 fixes 5 s darks")
	cmp("skipvdd-dark5", "keeping Vdd on during integration fixes 5 s darks")
	cmp("fifo0-bias", "disabling the USB FIFO fixes it (pipelining/data path issue)")
	cmp("vflush0-bias", "disabling vertical flush control fixes it")
	cmp("custopt-default-bias", "CAMERA CUSTOMER OPTIONS were the cause")
	cmp("vddoff-bias", "switching Vdd off between frames (camera option vddNormallyOff=1) FIXES bias frames - a usable workaround (set it in CCDOps)")
	cmp("vddoff-dark5", "vddNormallyOff=1 fixes 5 s darks")
	cmp("custoverscan-bias", "the overscan-option frame is fine")
	cmp("shutter-leave", "frames are good when the shutter does not move (shutter motor current / timing involved)")
	cmp("slow-readout", "slower readout fixes it (timing / power-supply droop during fast readout?)")
	cmp("autoadgain0-bias", "with DCP_AUTO_AD_GAIN=0 the frames are good: the driver's A/D gain writes are involved")
	cmp("pipeline0-bias", "disabling the pixel pipeline fixes it")
	cmp("relink-autoadgain0", "re-link with auto A/D gain off clears the fault")
	cmp("after-relink", "a software re-link clears the fault (camera state problem; reconnecting before each frame is a workaround)")
	cmp("after-rest", "resting idle clears the fault (thermal or slowly-recovering supply?)")
	for _, exp := range order {
		if strings.HasPrefix(exp, "default-") {
			cmp(exp, "resetting this parameter to its documented default fixes it")
		}
	}
	// Overscan.
	if rs := by["overscan-bias"]; len(rs) > 0 && rs[0].Class != "ERROR" {
		s.pl("- Overscan frame: check the right-most columns / bottom rows in its _rows.csv / FITS; saturated overscan = analog chain fault.")
	}
	// Top/bottom sub-frames.
	if t, b := by["sub-top200"], by["sub-bottom200"]; len(t) > 0 && len(b) > 0 && nb > 0 {
		if !bad(t[0]) && bad(b[0]) {
			s.pl("- Bottom rows bad but top rows good even in small subframes -> position on the CCD matters (not readout time).")
		}
		if bad(t[0]) && bad(b[0]) {
			s.pl("- Both small subframes bad -> not a cumulative 'runs out during long readout' effect.")
		}
	}
	// Stuck bits / dup rows / comm failures.
	for _, r := range s.results {
		for _, n := range r.Notes {
			if strings.Contains(n, "stuck A/D bits") || strings.Contains(n, "exact copies") {
				s.pf("- frame %d: %s\n", r.N, n)
			}
		}
		if r.ComFailed > 0 {
			s.pf("- frame %d: link comFailed increased by %d (USB communication errors)\n", r.N, r.ComFailed)
		}
		if r.PowerLow == "true" {
			s.pf("- frame %d: POWER-SUPPLY-LOW bit set after readout -> check the 12 V supply/cable\n", r.N)
		}
	}
	s.pl("Send the whole output folder (report.txt, summary.json, frames.csv, PNGs; FITS if possible) for analysis.")
}
