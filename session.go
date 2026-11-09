package main

import (
	"encoding/csv"
	"encoding/json"
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
	CCDTempC    float64           `json:"ccd_temp_c"`
	HeatsinkC   float64           `json:"heatsink_c"`
	ComFailed   int64             `json:"com_failed_delta"`
	PowerLow    string            `json:"power_low_bit"`
	DCPs        map[string]uint32 `json:"dcps"`
	RowMedian   []float64         `json:"row_median_binned"`
	Errors      []string          `json:"errors"`
	File        string            `json:"file"`
	PreExpS     float64           `json:"pre_exposure_s"`
	Exact65535  bool              `json:"all_exactly_65535"`
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
		s.pf("Name: %s\nFirmware: %s   camera type: %s\n", info.Name, info.FirmwareString(), cameraTypeName(info.CameraType))
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
	if t.Valid && t.Advanced && t.CoolingEnabled&1 != 0 && t.CoolerPower < 0.5 && t.CCDTemp > t.Setpoint+1 {
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
	if rip, err := c.ReadoutInProgress(); err == nil {
		s.pf("Readout in progress: %v\n", rip)
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
	cur, err := s.cam.GetCustomerOptions()
	if err != nil {
		return fmt.Errorf("cannot read customer options (%v) - not changing them", err)
	}
	if s.origOpts == nil {
		s.origOpts = &cur
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

// RestoreAll puts every changed setting back and verifies it.
func (s *Session) RestoreAll() bool {
	ok := true
	if len(s.origDCP) == 0 && s.origOpts == nil {
		return true
	}
	keys := make([]int, 0, len(s.origDCP))
	for k := range s.origDCP {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, i := range keys {
		v := s.origDCP[i]
		err := s.cam.SetDCP(i, v)
		rb, err2 := s.cam.GetDCP(i)
		if err != nil || err2 != nil || rb != v {
			ok = false
			s.pf("  RESTORE FAILED for %s (want %d, got %d, err %v %v)\n", dcpNames[i], v, rb, err, err2)
			continue
		}
		s.pf("  restored %s = %d\n", dcpNames[i], v)
		delete(s.origDCP, i)
	}
	if s.origOpts != nil {
		err := s.cam.SetCustomerOptions(*s.origOpts)
		rb, err2 := s.cam.GetCustomerOptions()
		if err != nil || err2 != nil || rb != *s.origOpts {
			ok = false
			s.pf("  RESTORE FAILED for customer options (want %s, got %s, err %v %v)\n", *s.origOpts, rb, err, err2)
		} else {
			s.pf("  restored customer options: %s\n", rb)
			s.origOpts = nil
		}
	}
	return ok
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
	if f == nil {
		s.pf("  EXPOSURE FAILED: %v\n", err)
		r := &FrameResult{N: n, Experiment: exp, Spec: spec.Describe(), Time: time.Now().Format(time.RFC3339), Class: "ERROR", Errors: []string{fmt.Sprint(err)}}
		s.results = append(s.results, r)
		return r
	}
	if err != nil {
		f.Errors = append(f.Errors, err.Error())
		s.pf("  readout error: %v (analysing the lines that were read)\n", err)
	}
	return s.analyseFrame(n, exp, f)
}

func (s *Session) analyseFrame(n int, exp string, f *Frame) *FrameResult {
	a := Analyze(f.Pix, f.W, f.H)
	r := &FrameResult{N: n, Experiment: exp, Spec: f.Spec.Describe(), Time: f.Start.Format(time.RFC3339),
		Class: a.Class, W: f.W, H: f.H, Stats: a.Stats, Boundaries: a.Boundaries, Segments: a.Segments,
		Notes: a.Notes, BitFrac: a.BitFrac, FirstSatRow: a.FirstSatRow, TopMed: a.TopMedian, BottomMed: a.BottomMedian,
		LeftMed: a.LeftMedian, RightMed: a.RightMedian,
		IntegrS: f.IntegrDone.Seconds(), ReadoutS: f.ReadoutTime.Seconds(), Errors: f.Errors,
		CCDTempC: f.TempAfter.CCDTemp, HeatsinkC: f.TempAfter.HeatsinkTemp, PowerLow: f.PowerLowAfter}
	r.ComFailed = int64(f.LinkAfter.ComFailed) - int64(f.LinkBefore.ComFailed)
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
	s.pf("  => %s   integration %.2f s, readout %.2f s, CCD %.1f C, heatsink %.1f C, comFailed +%d, PS-low %s\n",
		a.Class, r.IntegrS, r.ReadoutS, r.CCDTempC, r.HeatsinkC, r.ComFailed, r.PowerLow)
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
	if s.dir != "" {
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
		{"CCD-TEMP", f.TempAfter.CCDTemp, "after readout [C]"},
		{"HSINK-T", f.TempAfter.HeatsinkTemp, "heatsink after readout [C]"},
		{"COOLPWR", f.TempAfter.CoolerPower, "cooler power [%]"},
		{"INTEGR-S", r.IntegrS, "start to integration complete [s]"},
		{"READOUTS", r.ReadoutS, "readout duration [s]"},
		{"COMFAIL", int(r.ComFailed), "link comFailed increase"},
		{"SBIGTEST", exp, "sbigdiag experiment"},
		{"SBIGSPEC", f.Spec.Describe(), ""},
		{"ROWORDER", "TOP-DOWN", "row 0 = first line returned by driver"},
		{"SWCREATE", "sbigdiag " + version, ""},
	}
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
		r.IntegrS, r.ReadoutS, r.CCDTempC, r.HeatsinkC = finite(r.IntegrS), finite(r.ReadoutS), finite(r.CCDTempC), finite(r.HeatsinkC)
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
			fmt.Sprintf("%.2f", r.IntegrS), fmt.Sprintf("%.2f", r.ReadoutS), fmt.Sprintf("%.2f", r.CCDTempC), fmt.Sprintf("%.2f", r.HeatsinkC),
			fmt.Sprint(r.ComFailed), r.PowerLow, r.File})
	}
	w.Flush()
}
