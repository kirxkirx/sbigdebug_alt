package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Session owns the camera connection, the output directory and the list of
// temporary setting changes that must be undone.
type Session struct {
	cam      *Camera
	out      io.Writer
	dir      string
	device   string
	ip       string
	abort    atomic.Bool
	saveFITS bool
	results  []*FrameResult
	origDCP  map[int]uint32 // original values of every DCP we changed
	origOpts *CustomerOptions
	dcpStart map[int]uint32 // all DCP values read at start
	info     map[string]interface{}
	offsets  []OffsetSample
	nframe   int
	restSec  int
	// nvPending is true while camera customer options (stored in the camera) differ
	// from their original values; the signal handler then refuses to os.Exit.
	nvPending    atomic.Bool
	origOptsText atomic.Value
	halted       atomic.Bool // a restore failed: no further experiment may run
	haltReason   string
}

// halt stops the experiment sequence after a failed restore (continuing would run
// every later experiment with a setting stuck at its test value).
func (s *Session) halt(reason string) {
	if s.halted.CompareAndSwap(false, true) {
		s.haltReason = reason
		s.info["status"] = "halted: " + reason
		s.pf("\n!!! STOPPED: %s - the remaining experiments are skipped.\n", reason)
	}
}

type FrameResult struct {
	N           int               `json:"n"`
	Experiment  string            `json:"experiment"`
	Spec        string            `json:"spec"`
	Time        string            `json:"time"`
	Class       string            `json:"class"`
	W           int               `json:"w"`
	H           int               `json:"h"`
	Stats       Stats             `json:"stats"`
	Boundaries  []Boundary        `json:"boundaries"`
	Segments    []Segment         `json:"segments"`
	Notes       []string          `json:"notes"`
	BitFrac     [16]float64       `json:"bit_frac"`
	FirstSatRow int               `json:"first_saturated_row"`
	TopMed      float64           `json:"top_median"`
	BottomMed   float64           `json:"bottom_median"`
	LeftMed     float64           `json:"left_median"`
	RightMed    float64           `json:"right_median"`
	IntegrS     float64           `json:"integration_s"`
	ReadoutS    float64           `json:"readout_s"`
	SlowLines   string            `json:"slow_lines"`
	CCDTempC    *float64          `json:"ccd_temp_c"` // null when the temperature query failed
	HeatsinkC   *float64          `json:"heatsink_c"`
	ComFailed   int64             `json:"com_failed_delta"`
	ComValid    bool              `json:"com_counters_valid"`
	SHA256      string            `json:"pixels_sha256"`
	PowerLow    string            `json:"power_low_bit"`
	DCPs        map[string]uint32 `json:"dcps"`
	RowMedian   []float64         `json:"row_median_binned"`
	Errors      []string          `json:"errors"`
	File        string            `json:"file"`
	PreExpS     float64           `json:"pre_exposure_s"`
	Exact65535  bool              `json:"all_exactly_65535"`
	Dark        bool              `json:"shutter_closed"`
	LinesRead   int               `json:"lines_read"`
	StrictClass string            `json:"class_with_bias_rules"` // class under the closed-shutter rules
	FatalErr    string            `json:"fatal_error,omitempty"` // the error that ended the exposure/readout
}

type OffsetSample struct {
	When   string  `json:"when"`
	Offset int     `json:"offset"`
	RMS    float64 `json:"rms"`
	Err    string  `json:"err,omitempty"`
}

func (s *Session) pf(format string, a ...interface{}) { fmt.Fprintf(s.out, format, a...) }
func (s *Session) pl(a ...interface{})                { fmt.Fprintln(s.out, a...) }

func (s *Session) aborted() bool { return s.abort.Load() }

// --- connection ---

func (s *Session) Connect() error {
	c := s.cam
	s.pl("\n== Driver ==")
	if err := c.OpenDriver(); err != nil {
		return fmt.Errorf("CC_OPEN_DRIVER: %v", err)
	}
	for _, req := range []uint16{0, 1, 2} {
		names := []string{"standard", "extended", "USB loader"}
		if di, err := c.DriverInfo(req); err == nil {
			s.pf("Driver info (%s): version %s, name %q, maxRequest %d\n", names[req], di.VersionString(), di.Name, di.MaxRequest)
			s.info[fmt.Sprintf("driver_info_%d", req)] = fmt.Sprintf("%s %q", di.VersionString(), di.Name)
		} else {
			s.pf("Driver info (%s): %v\n", names[req], err)
		}
	}
	if cams, err := c.QueryUSB(); err == nil {
		s.pf("USB cameras found by the driver: %d\n", len(cams))
		for i, u := range cams {
			s.pf("  [%d] %s  type=%s  serial=%q\n", i+1, u.Name, cameraTypeName(u.Type), u.Serial)
		}
	} else {
		s.pf("CC_QUERY_USB: %v\n", err)
	}
	s.pf("Opening device %s ...\n", s.device)
	if err := c.OpenDevice(s.device, s.ip); err != nil {
		hint := ""
		if isCode(err, CE_DEVICE_NOT_CLOSED, CE_SHARE_ERROR, CE_DRIVER_NOT_CLOSED) {
			hint = "\n  -> the camera seems to be in use: disconnect it in NINA/CCDOps/MaxIm (or close them) and try again"
		}
		if isCode(err, CE_DEVICE_NOT_FOUND, CE_CAMERA_NOT_FOUND) {
			hint = "\n  -> camera not found: check USB cable/power; if another program is connected, close it"
		}
		return fmt.Errorf("CC_OPEN_DEVICE: %v%s", err, hint)
	}
	t0 := time.Now()
	typ, err := c.EstablishLink()
	if err != nil {
		return fmt.Errorf("CC_ESTABLISH_LINK: %v", err)
	}
	s.pf("Link established in %.1f s: camera type %s (%d)\n", time.Since(t0).Seconds(), cameraTypeName(typ), typ)
	e1, e2 := c.CleanState()
	s.pf("Clean-state preamble (abort any exposure/readout left by another program): end-exposure %s, end-readout %s\n", e1, e2)
	return nil
}

// --- information dump ---

func (s *Session) Info() {
	c := s.cam
	s.pl("\n== Camera ==")
	if info, err := c.CCDInfo(0); err == nil {
		s.pf("Name: %s\nFirmware: %s (BCD 0x%04x; NINA displays this as %d.%d because it prints the low byte in decimal)   camera type: %s\n",
			info.Name, info.FirmwareString(), info.Firmware, info.Firmware>>8, info.Firmware&0xFF, cameraTypeName(info.CameraType))
		s.info["camera"] = info.Name
		s.info["firmware"] = info.FirmwareString()
		for _, m := range info.Modes {
			s.pf("  readout mode %-12s %4d x %-4d  gain %.2f e/ADU  pixel %.2f x %.2f um\n",
				readoutModeName(m.Mode), m.Width, m.Height, m.GainEPerADU, m.PixelW, m.PixelH)
		}
	} else {
		s.pf("CCD info (imaging): %v\n", err)
	}
	if info, err := c.CCDInfo(1); err == nil && len(info.Modes) > 0 {
		s.pf("Tracking CCD: %s, %dx%d\n", info.Name, info.Modes[0].Width, info.Modes[0].Height)
	} else if err != nil {
		s.pf("Tracking CCD info: %v\n", err)
	}
	if x, err := c.CCDInfoExtended(); err == nil {
		s.pf("Serial number: %q   imaging ABG: %d   bad columns (factory list): %v\n", x.Serial, x.ABG, x.BadColumns)
		s.info["serial"] = x.Serial
	} else {
		s.pf("Extended CCD info: %v\n", err)
	}
	if caps, dumpExtra, err := c.CCDCapabilities(); err == nil {
		var cs []string
		for _, b := range capabilityBits {
			if caps&b.mask != 0 {
				cs = append(cs, b.set)
			} else {
				cs = append(cs, b.clr)
			}
		}
		s.pf("Capabilities 0x%04x: %s; dumpExtra=%d\n", caps, strings.Join(cs, ", "), dumpExtra)
	} else {
		s.pf("Capabilities: %v\n", err)
	}
	if a, b, cc, err := c.CCDInfo6(); err == nil {
		s.pf("CCD_INFO_EXTENDED3: cameraBits=0x%08x ccdBits=0x%08x extraBits=0x%08x\n", a, b, cc)
	} else {
		s.pf("CCD_INFO_EXTENDED3: %v\n", err)
	}
	if o, err := c.GetCustomerOptions(); err == nil {
		s.pf("Customer options (stored IN THE CAMERA, survive power cycles): %s\n", o)
		s.info["customer_options"] = o.String()
		if o.Preflash != 0 {
			s.pl("  !!! preflashCcd=1: the camera floods the CCD with light (RBI preflash) before exposures.")
			s.pl("      If the flush after the preflash fails, frames come out bright/saturated. This is a prime suspect.")
		}
		if o.VddNormallyOff != 0 {
			s.pl("  !!! vddNormallyOff=1: the CCD output amplifier supply is kept off between exposures (non-default).")
		}
		if o.Overscan != 0 {
			s.pl("  note: overscanRegions=1 - frames include overscan regions (non-default).")
		}
	} else {
		s.pf("Customer options: %v\n", err)
	}
	// GET only fills the results struct (params NULL) - it cannot write anything.
	if dl, fc, err := c.RBIParams(); err == nil {
		s.pf("RBI preflash parameters: darkFrameLength=%d flushCount=%d\n", dl, fc)
		s.info["rbi"] = fmt.Sprintf("darkFrameLength=%d flushCount=%d", dl, fc)
	} else {
		s.pf("RBI preflash parameters: %v\n", err)
	}
	if cols, flags, err := c.ColumnEEPROM(); err == nil {
		s.pf("Column-repair EEPROM: columns=%v flags=0x%04x\n", cols, flags)
	} else {
		s.pf("Column-repair EEPROM: %v\n", err)
	}
	if ls, err := c.LinkStatus(); err == nil {
		s.pf("Link status: linked=%v type=%s comTotal=%d comFailed=%d\n", ls.Linked, cameraTypeName(ls.CameraType), ls.ComTotal, ls.ComFailed)
	}
	t := c.Temperature()
	s.pf("Temperature: %s\n", t)
	s.info["temperature"] = t.String()
	if t.Valid && t.Advanced && t.CoolingEnabled&1 != 0 && !t.Frozen() && t.CoolerPower < 0.5 && t.CCDTemp > t.Setpoint+1 {
		s.pl("  !!! Cooling is ENABLED but the cooler gets 0% power while the CCD is above the setpoint.")
		s.pl("      The STXL switches its TE cooler off when the input voltage is <= 10.25 V (manual). Check the 12 V supply,")
		s.pl("      cable and the voltage LEDs on the side of the camera (green / 11V / 10V / 9V) - 9V lit = unreliable operation.")
		s.info["cooler_shutdown_suspected"] = true
	}
	if pl, err := c.PowerLow(); err == nil {
		s.pf("Power-supply-low bit (input < ~10 V on STL; may not be wired on STXL): %v\n", pl)
	} else {
		s.pf("Power-supply-low bit: %v\n", err)
	}
	for ffr, name := range []string{"FFR_CTRL_OFFSET_CORRECTION", "FFR_CTRL_EXT_SHUTTER_ONLY", "FFR_ASYNC_TRIGGER_IN"} {
		if ok, err := c.FeatureSupported(uint32(ffr)); err == nil {
			s.pf("Firmware feature %s: %v\n", name, ok)
		} else {
			s.pf("Firmware feature %s: %v\n", name, err)
		}
	}
	s.DumpDCPs("Driver control parameters (start)")
	s.SampleOffsets("info", 3)
}

func (s *Session) DumpDCPs(title string) map[int]uint32 {
	s.pf("\n== %s ==\n", title)
	vals := map[int]uint32{}
	for i, n := range dcpNames {
		v, err := s.cam.GetDCP(i)
		def, hasDef := dcpDocDefault[i]
		if err != nil {
			s.pf("  %2d %-31s  (not readable: %s)\n", i, n, shortErr(err))
			continue
		}
		vals[i] = v
		mark := ""
		if hasDef && int64(v) != def {
			mark = fmt.Sprintf("   <-- differs from documented default %d", def)
		}
		s.pf("  %2d %-31s = %d%s\n", i, n, v, mark)
	}
	if s.dcpStart == nil {
		s.dcpStart = vals
		m := map[string]uint32{}
		for i, v := range vals {
			m[dcpNames[i]] = v
		}
		s.info["dcp_start"] = m
	}
	return vals
}

func shortErr(err error) string {
	var se *SBIGError
	if e, ok := err.(*SBIGError); ok {
		se = e
		return errName(se.Code)
	}
	return err.Error()
}

// SampleOffsets reads the camera's own bias measurement (CC_READ_OFFSET2) n times.
func (s *Session) SampleOffsets(label string, n int) {
	if s.aborted() {
		return
	}
	var vals []string
	for k := 0; k < n; k++ {
		o, rms, err := s.cam.ReadOffset2()
		smp := OffsetSample{When: label, Offset: int(o), RMS: rms}
		if err != nil {
			o2, err2 := s.cam.ReadOffset()
			if err2 != nil {
				smp.Err = shortErr(err)
				s.offsets = append(s.offsets, smp)
				vals = append(vals, "err "+shortErr(err))
				break
			}
			smp.Offset, smp.RMS = int(o2), -1 // RMS unknown with CC_READ_OFFSET
		}
		s.offsets = append(s.offsets, smp)
		vals = append(vals, fmt.Sprintf("%d (rms %.1f)", smp.Offset, smp.RMS))
	}
	s.pf("Camera-measured offset [%s]: %s\n", label, strings.Join(vals, ", "))
}

// --- temporary settings with guaranteed restore ---

func (s *Session) SetDCPTemp(i int, v uint32) error {
	c := s.cam
	cur, err := c.GetDCP(i)
	if err != nil {
		return fmt.Errorf("cannot read %s before changing it (%v) - not changing it", dcpNames[i], err)
	}
	if _, saved := s.origDCP[i]; !saved {
		s.origDCP[i] = cur
	}
	if err := c.SetDCP(i, v); err != nil {
		return err
	}
	rb, err := c.GetDCP(i)
	if err != nil {
		return fmt.Errorf("readback of %s failed: %v", dcpNames[i], err)
	}
	s.pf("  set %s: %d -> %d (readback %d)\n", dcpNames[i], cur, v, rb)
	if rb != v {
		return fmt.Errorf("%s readback %d != requested %d", dcpNames[i], rb, v)
	}
	return nil
}

func (s *Session) SetOptionsTemp(o CustomerOptions) error {
	if s.aborted() {
		return errAborted
	}
	cur, err := s.cam.GetCustomerOptions()
	if err != nil {
		return fmt.Errorf("cannot read customer options (%v) - not changing them", err)
	}
	if s.origOpts == nil {
		s.origOpts = &cur
	}
	s.origOptsText.Store(s.origOpts.String())
	s.nvPending.Store(true)
	if err := writeJournal(Journal{Original: *s.origOpts, ChangedTo: o, Time: time.Now().Format(time.RFC3339), Dir: s.dir}); err != nil {
		s.pf("  (restore journal not written: %v)\n", err)
	}
	if err := s.cam.SetCustomerOptions(o); err != nil {
		return err
	}
	rb, err := s.cam.GetCustomerOptions()
	s.pf("  customer options: %s -> %s (readback %s)\n", cur, o, rb)
	if err == nil && rb != o {
		return fmt.Errorf("customer options readback mismatch")
	}
	return err
}

// RestoreAll puts every changed setting back and verifies it. Camera options
// (stored in the camera) go first: they are the ones that would outlive us.
func (s *Session) RestoreAll() bool {
	ok := s.restoreOptions()
	keys := make([]int, 0, len(s.origDCP))
	for k := range s.origDCP {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, i := range keys {
		if !s.restoreDCP(i) {
			ok = false
		}
	}
	return ok
}

// restoreDCP restores one driver control parameter changed with SetDCPTemp.
func (s *Session) restoreDCP(i int) bool {
	v, changed := s.origDCP[i]
	if !changed {
		return true
	}
	err := s.cam.SetDCP(i, v)
	rb, err2 := s.cam.GetDCP(i)
	if err != nil || err2 != nil || rb != v {
		s.pf("  RESTORE FAILED for %s (want %d, got %d, err %v %v)\n", dcpNames[i], v, rb, err, err2)
		return false
	}
	s.pf("  restored %s = %d\n", dcpNames[i], v)
	delete(s.origDCP, i)
	return true
}

// restoreOptions restores the camera customer options changed with SetOptionsTemp.
func (s *Session) restoreOptions() bool {
	if s.origOpts == nil {
		return true
	}
	var err, err2 error
	var rb CustomerOptions
	for try := 0; try < 3; try++ { // these live in the camera: try hard
		err = s.cam.SetCustomerOptions(*s.origOpts)
		rb, err2 = s.cam.GetCustomerOptions()
		if err == nil && err2 == nil && rb == *s.origOpts {
			s.pf("  restored customer options: %s\n", rb)
			s.origOpts = nil
			s.nvPending.Store(false)
			removeJournal()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	s.pf("  RESTORE FAILED for customer options (want %s, got %s, err %v %v)\n", *s.origOpts, rb, err, err2)
	s.pf("  -> please set them back in CCDOps: Camera Setup -> Customer Options: %s\n", *s.origOpts)
	return false
}

// --- exposures ---

func (s *Session) TakeFrame(exp string, spec ExposureSpec) *FrameResult {
	if s.aborted() {
		return nil
	}
	s.nframe++
	n := s.nframe
	s.pf("\n--- frame %d [%s] %s ---\n", n, exp, spec.Describe())
	f, err := s.cam.Expose(spec, s.aborted)
	if errors.Is(err, errAborted) {
		// Do nothing slow here: restoring settings and closing must start at once.
		s.pl("  frame aborted")
		r := &FrameResult{N: n, Experiment: exp, Spec: spec.Describe(), Time: time.Now().Format(time.RFC3339), Class: "ABORTED", Dark: spec.Dark}
		s.results = append(s.results, r)
		return r
	}
	if f == nil {
		s.pf("  EXPOSURE FAILED: %v\n", err)
		r := &FrameResult{N: n, Experiment: exp, Spec: spec.Describe(), Time: time.Now().Format(time.RFC3339), Class: "ERROR", Errors: []string{fmt.Sprint(err)}, Dark: spec.Dark, FatalErr: fmt.Sprint(err)}
		s.results = append(s.results, r)
		s.WriteJSON()
		return r
	}
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
		s.pf("  readout error: %v (analysing the %d lines that were read)\n", err, f.LinesRead)
	}
	r := s.analyseFrame(n, exp, f)
	if err != nil {
		r.FatalErr = err.Error()
	}
	s.WriteJSON() // keep summary.json/frames.csv current in case the run is killed
	return r
}

func (s *Session) analyseFrame(n int, exp string, f *Frame) *FrameResult {
	partial := f.LinesRead < f.H
	if partial {
		f.H = f.LinesRead // analyse and save only what was read
		f.Pix = f.Pix[:f.W*f.H]
	}
	a := Analyze(f.Pix, f.W, f.H)
	if f.Spec.Dark {
		a.ApplyDarkRules()
	}
	strict := *a // the same frame judged by the closed-shutter (bias) rules
	strict.Notes = nil
	if !f.Spec.Dark {
		strict.ApplyDarkRules()
	}
	if partial {
		a.Class = "PARTIAL"
		strict.Class = "PARTIAL"
	}
	r := &FrameResult{N: n, Experiment: exp, Spec: f.Spec.Describe(), Time: f.Start.Format(time.RFC3339),
		Class: a.Class, W: f.W, H: f.H, Stats: a.Stats, Boundaries: a.Boundaries, Segments: a.Segments,
		Notes: a.Notes, BitFrac: a.BitFrac, FirstSatRow: a.FirstSatRow, TopMed: a.TopMedian, BottomMed: a.BottomMedian,
		LeftMed: a.LeftMedian, RightMed: a.RightMedian,
		IntegrS: f.IntegrDone.Seconds(), ReadoutS: f.ReadoutTime.Seconds(), Errors: f.Errors,
		PowerLow: f.PowerLowAfter,
		Dark:     f.Spec.Dark, LinesRead: f.LinesRead, StrictClass: strict.Class}
	if f.TempAfter.Valid && f.TempAfter.Advanced {
		ct, hs := f.TempAfter.CCDTemp, f.TempAfter.HeatsinkTemp
		r.CCDTempC, r.HeatsinkC = &ct, &hs
	}
	r.ComValid = f.LinkValid
	if f.LinkValid {
		r.ComFailed = int64(f.LinkAfter.ComFailed) - int64(f.LinkBefore.ComFailed)
	}
	r.SHA256 = pixelHash(f.Pix)
	r.Exact65535 = a.Stats.Full65535Frac > 0.999
	phases := ""
	if f.PhasesValid {
		r.PreExpS = f.Phases[1].Seconds()
		phases = fmt.Sprintf("   phases: pre-exposure %.2f s, integrating %.2f s, post %.2f s", f.Phases[1].Seconds(), f.Phases[2].Seconds(), f.Phases[3].Seconds())
	}
	r.RowMedian = binProfile(a.RowMedian, 64)
	r.SlowLines = slowLines(f.LineTimes)
	// DCP snapshot for the record
	r.DCPs = map[string]uint32{}
	for _, i := range []int{DCP_VDD_OPTIMIZED, DCP_HIGH_THROUGHPUT, DCP_USB_FIFO_ENABLE, DCP_VERT_FLUSH_CONTROL_ENABLE, DCP_OVERSCAN_ROWSCOLS, DCP_AUTO_AD_GAIN} {
		if v, err := s.cam.GetDCP(i); err == nil {
			r.DCPs[dcpNames[i]] = v
		}
	}
	com := "n/a"
	if r.ComValid {
		com = fmt.Sprintf("+%d", r.ComFailed)
	}
	s.pf("  => %s   integration %.2f s, readout %.2f s, CCD %s C, heatsink %s C, comFailed %s, PS-low %s\n",
		a.Class, r.IntegrS, r.ReadoutS, fmtTemp(r.CCDTempC), fmtTemp(r.HeatsinkC), com, r.PowerLow)
	if phases != "" {
		s.pl(phases)
	}
	for _, l := range a.Summary() {
		s.pl("  " + l)
	}
	s.pl("  " + a.BitLine())
	if r.SlowLines != "" {
		s.pl("  line timing: " + r.SlowLines)
	}
	for _, l := range Thumbnail(f.Pix, f.W, f.H, 64, 16) {
		s.pl(l)
	}
	if s.dir != "" && f.H > 0 {
		base := fmt.Sprintf("frame%03d_%s", n, sanitize(exp))
		if s.saveFITS {
			cards := s.fitsCards(f, exp, r)
			if err := WriteFITS(filepath.Join(s.dir, base+".fits"), f.Pix, f.W, f.H, cards); err != nil {
				s.pf("  (FITS write failed: %v)\n", err)
			} else {
				r.File = base + ".fits"
			}
		}
		_ = WritePreview(filepath.Join(s.dir, base+".png"), f.Pix, f.W, f.H, a.Stats)
		_ = writeRowCSV(filepath.Join(s.dir, base+"_rows.csv"), a)
	}
	s.results = append(s.results, r)
	return r
}

func (s *Session) fitsCards(f *Frame, exp string, r *FrameResult) []fitsCard {
	typ := "BIAS"
	if f.Spec.Seconds > 0 {
		typ = "DARK"
	}
	if !f.Spec.Dark {
		typ = "LIGHT"
	}
	cards := []fitsCard{
		{"DATE-OBS", f.Start.UTC().Format("2006-01-02T15:04:05.000"), "UTC start of exposure"},
		{"EXPTIME", f.Spec.Seconds, "requested exposure [s]"},
		{"IMAGETYP", typ, ""},
		{"INSTRUME", fmt.Sprint(s.info["camera"]), ""},
		{"READMODE", readoutModeName(f.Spec.Mode), "SBIG readout mode"},
		{"SUBTOP", f.Spec.Top, "subframe top (binned px)"},
		{"SUBLEFT", f.Spec.Left, "subframe left (binned px)"},
		{"INTEGR-S", r.IntegrS, "start to integration complete [s]"},
		{"READOUTS", r.ReadoutS, "readout duration [s]"},
		{"SBIGTEST", exp, "sbigdiag experiment"},
		{"SBIGSPEC", f.Spec.Describe(), ""},
		{"ROWORDER", "TOP-DOWN", "row 0 = first line returned by driver"},
		{"SWCREATE", "sbigdiag " + version, ""},
	}
	if f.TempAfter.Valid && f.TempAfter.Advanced { // only real readings, never zeros for "unknown"
		cards = append(cards, fitsCard{"CCD-TEMP", f.TempAfter.CCDTemp, "after readout [C]"},
			fitsCard{"HSINK-T", f.TempAfter.HeatsinkTemp, "heatsink after readout [C]"},
			fitsCard{"COOLPWR", f.TempAfter.CoolerPower, "cooler power [%]"})
	}
	if r.ComValid {
		cards = append(cards, fitsCard{"COMFAIL", int(r.ComFailed), "link comFailed increase"})
	}
	cards = append(cards, fitsCard{"PIXSHA", r.SHA256[:min(16, len(r.SHA256))], "first 16 hex of pixel SHA-256"})
	short := map[string]string{"DCP_VDD_OPTIMIZED": "VDDOPT", "DCP_HIGH_THROUGHPUT": "HITHRU", "DCP_USB_FIFO_ENABLE": "USBFIFO",
		"DCP_VERT_FLUSH_CONTROL_ENABLE": "VFLUSH", "DCP_OVERSCAN_ROWSCOLS": "OVRSCAN", "DCP_AUTO_AD_GAIN": "AUTOADG"}
	keys := make([]string, 0, len(r.DCPs))
	for k := range r.DCPs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if sk, ok := short[k]; ok {
			cards = append(cards, fitsCard{sk, int(r.DCPs[k]), k})
		}
	}
	return cards
}

func sanitize(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func binProfile(p []float64, n int) []float64 {
	if len(p) <= n {
		return p
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		a, b := i*len(p)/n, (i+1)*len(p)/n
		out[i] = math.Round(median(p[a:b]))
	}
	return out
}

func slowLines(t []time.Duration) string {
	if len(t) < 10 {
		return ""
	}
	ms := make([]float64, 0, len(t))
	for _, d := range t {
		if d > 0 {
			ms = append(ms, float64(d.Microseconds())/1000)
		}
	}
	if len(ms) == 0 {
		return ""
	}
	med := median(ms)
	mx, at := 0.0, 0
	var slow []string
	for i, d := range t {
		v := float64(d.Microseconds()) / 1000
		if v > mx {
			mx, at = v, i
		}
		if v > math.Max(50, 10*med) && len(slow) < 8 {
			slow = append(slow, fmt.Sprintf("%d(%.0fms)", i, v))
		}
	}
	res := fmt.Sprintf("median %.2f ms/line, max %.1f ms at line %d", med, mx, at)
	if len(slow) > 0 {
		res += "; slow lines: " + strings.Join(slow, " ")
	}
	return res
}

func writeRowCSV(path string, a *Analysis) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"row", "median", "mean", "saturated_fraction"})
	for y := range a.RowMedian {
		w.Write([]string{fmt.Sprint(y), fmt.Sprintf("%.0f", a.RowMedian[y]), fmt.Sprintf("%.1f", a.RowMean[y]), fmt.Sprintf("%.4f", a.RowSatFrac[y])})
	}
	w.Flush()
	return w.Error()
}

// --- summary ---

func (s *Session) PrintTable() {
	s.pl("\n== Summary of all frames ==")
	s.pf("%3s %-26s %-38s %-9s %8s %8s %6s %6s %s\n", "#", "experiment", "spec", "class", "median", "mean", "sat%", "comF", "boundaries")
	for _, r := range s.results {
		var bs []string
		for _, b := range r.Boundaries {
			bs = append(bs, fmt.Sprintf("r%d:%.0f->%.0f", b.Row, b.Before, b.After))
		}
		spec := r.Spec
		if len(spec) > 38 {
			spec = spec[:38]
		}
		s.pf("%3d %-26.26s %-38s %-9s %8.0f %8.0f %6.1f %6d %s\n", r.N, r.Experiment, spec, r.Class,
			r.Stats.Median, r.Stats.Mean, 100*r.Stats.SatFrac, r.ComFailed, strings.Join(bs, " "))
	}
}

// finite replaces NaN/Inf (which encoding/json refuses) with -1.
func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return -1
	}
	return v
}

func (s *Session) WriteJSON() {
	if s.dir == "" {
		return
	}
	for _, r := range s.results {
		r.TopMed, r.BottomMed, r.LeftMed, r.RightMed = finite(r.TopMed), finite(r.BottomMed), finite(r.LeftMed), finite(r.RightMed)
		r.IntegrS, r.ReadoutS = finite(r.IntegrS), finite(r.ReadoutS)
		for i := range r.RowMedian {
			r.RowMedian[i] = finite(r.RowMedian[i])
		}
		for i := range r.Boundaries {
			r.Boundaries[i].Before, r.Boundaries[i].After = finite(r.Boundaries[i].Before), finite(r.Boundaries[i].After)
		}
		for i := range r.Segments {
			r.Segments[i].Median, r.Segments[i].SatFrac = finite(r.Segments[i].Median), finite(r.Segments[i].SatFrac)
		}
	}
	for i := range s.offsets {
		s.offsets[i].RMS = finite(s.offsets[i].RMS)
	}
	doc := map[string]interface{}{
		"version": version, "info": s.info, "frames": s.results, "offsets": s.offsets,
	}
	b, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		fmt.Fprintf(s.out, "summary.json not written: %v\n", err)
	} else {
		_ = os.WriteFile(filepath.Join(s.dir, "summary.json"), b, 0o644)
	}
	// One-line-per-frame CSV for spreadsheets.
	f, err := os.Create(filepath.Join(s.dir, "frames.csv"))
	if err != nil {
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	w.Write([]string{"n", "experiment", "spec", "class", "median", "mean", "std", "robust_sigma", "sat_frac", "top_median", "bottom_median",
		"first_sat_row", "boundaries", "integration_s", "readout_s", "ccd_temp", "heatsink", "com_failed_delta", "power_low", "file"})
	for _, r := range s.results {
		var bs []string
		for _, b := range r.Boundaries {
			bs = append(bs, fmt.Sprintf("%d:%.0f->%.0f", b.Row, b.Before, b.After))
		}
		w.Write([]string{fmt.Sprint(r.N), r.Experiment, r.Spec, r.Class, fmt.Sprintf("%.0f", r.Stats.Median), fmt.Sprintf("%.1f", r.Stats.Mean),
			fmt.Sprintf("%.1f", r.Stats.Std), fmt.Sprintf("%.1f", r.Stats.RobustSigma), fmt.Sprintf("%.4f", r.Stats.SatFrac),
			fmt.Sprintf("%.0f", r.TopMed), fmt.Sprintf("%.0f", r.BottomMed), fmt.Sprint(r.FirstSatRow), strings.Join(bs, " "),
			fmt.Sprintf("%.2f", r.IntegrS), fmt.Sprintf("%.2f", r.ReadoutS), csvTemp(r.CCDTempC), csvTemp(r.HeatsinkC),
			fmt.Sprint(r.ComFailed), r.PowerLow, r.File})
	}
	w.Flush()
}

func fmtTemp(t *float64) string {
	if t == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", *t)
}

func csvTemp(t *float64) string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("%.2f", *t)
}

// pixelHash identifies the exact pixel content of a frame (to catch a driver or
// camera buffer that delivers the same image twice).
func pixelHash(p []uint16) string {
	h := sha256.New()
	buf := make([]byte, 0, 1<<16)
	for i, v := range p {
		buf = append(buf, byte(v), byte(v>>8))
		if len(buf) == cap(buf) || i == len(p)-1 {
			h.Write(buf)
			buf = buf[:0]
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// --- restore journal for camera customer options ---
//
// Camera options live in the camera's non-volatile memory. If sbigdiag is killed
// (driver crash, forced kill, power loss) while one is changed, the journal lets the
// next start put it back.

type Journal struct {
	Original  CustomerOptions `json:"original"`
	ChangedTo CustomerOptions `json:"changed_to"`
	Time      string          `json:"time"`
	Dir       string          `json:"run_dir"`
}

func journalPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "sbigdiag", "pending_restore.json")
}

func writeJournal(j Journal) error {
	p := journalPath()
	if p == "" {
		return errors.New("no user config dir")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(j, "", " ")
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func removeJournal() {
	if p := journalPath(); p != "" {
		os.Remove(p)
	}
}

// RecoverJournal runs right after connecting: it restores camera options that an
// earlier, interrupted sbigdiag run left changed.
func (s *Session) RecoverJournal() {
	p := journalPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var j Journal
	if json.Unmarshal(b, &j) != nil {
		s.pf("Restore journal %s is unreadable - removed.\n", p)
		removeJournal()
		return
	}
	cur, err := s.cam.GetCustomerOptions()
	if err != nil {
		s.pf("Restore journal found (%s), but the camera options cannot be read (%v) - kept for the next run.\n", p, err)
		return
	}
	switch cur {
	case j.ChangedTo:
		s.pf("\n!!! An earlier sbigdiag run (%s) was interrupted while camera options were changed: %s\n", j.Time, cur)
		if err := s.cam.SetCustomerOptions(j.Original); err == nil {
			if rb, err := s.cam.GetCustomerOptions(); err == nil && rb == j.Original {
				s.pf("!!! Restored them to the original values: %s\n", rb)
				removeJournal()
				return
			}
		}
		s.pf("!!! Could not restore them - please set them in CCDOps (Camera Setup -> Customer Options): %s\n", j.Original)
	case j.Original:
		removeJournal() // already back to the original values
	default:
		s.pf("Restore journal from %s: the camera options (%s) were changed since; left as they are.\n", j.Time, cur)
		removeJournal()
	}
}
