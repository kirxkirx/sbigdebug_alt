// sbigdiag - diagnostic tool for SBIG STX/STXL cameras (written for an
// STXL-11002 that produces corrupted / saturated frames). It talks to the
// vendor SBIG Universal Driver (SBIGUDrv.dll) directly, prints everything the
// driver and camera report, takes and analyses test frames, and runs controlled
// experiments with driver settings, restoring every changed setting afterwards.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func init() {
	// The SBIG driver must be called from one OS thread; all camera calls are made
	// from the main goroutine, which is locked to the main thread here.
	runtime.LockOSThread()
}

type options struct {
	dll, device, ip, outDir                             string
	info, quick, full, sim, simHealthy, noPause, noFITS bool
	frames                                              int
	custom                                              string
	sets                                                string
	analyze                                             bool
	rest                                                int
	driverLog                                           bool
	noEventLog                                          bool
}

func main() {
	var o options
	flag.StringVar(&o.dll, "dll", "", "path to SBIGUDrv.dll (default: the one Windows finds, normally C:\\Windows\\System32)")
	flag.StringVar(&o.device, "device", "usb", "usb, usb1..usb24 or eth")
	flag.StringVar(&o.ip, "ip", "", "camera IP address for -device eth")
	flag.StringVar(&o.outDir, "out", "", "output directory (default: sbigdiag_<date> next to the exe)")
	flag.BoolVar(&o.info, "info", false, "camera/driver information only, no exposures")
	flag.BoolVar(&o.quick, "quick", false, "information + 5 bias frames")
	flag.BoolVar(&o.full, "full", false, "information + the full automatic test plan (~10 min)")
	flag.IntVar(&o.frames, "n", 5, "number of frames for -quick / -custom")
	flag.StringVar(&o.custom, "custom", "", "custom series, e.g. \"exp=5,bin=1,dark=1,top=0,height=200,skipvdd=1,linedelay=2ms\"")
	flag.StringVar(&o.sets, "set", "", "temporary driver settings during -custom/-quick, e.g. \"VDD_OPTIMIZED=0,USB_FIFO_ENABLE=0\" (restored at exit)")
	flag.BoolVar(&o.sim, "simulate", false, "use a simulated camera that reproduces the fault (demo/testing, no hardware)")
	flag.BoolVar(&o.simHealthy, "simulate-healthy", false, "simulated camera without the fault")
	flag.BoolVar(&o.noPause, "nopause", false, "do not wait for Enter before exiting")
	flag.BoolVar(&o.noFITS, "nofits", false, "do not save FITS files (PNG previews and CSV are still written)")
	flag.IntVar(&o.rest, "rest", 90, "idle time in seconds for the 'rest' experiment of -full")
	flag.BoolVar(&o.driverLog, "driverlog", false, "verbose SBIG driver debug log (commands + USB), not only camera events")
	flag.BoolVar(&o.noEventLog, "noeventlog", false, "do not record the camera's event messages (e.g. 'LV TE Off') via the driver's debug log")
	flag.BoolVar(&o.analyze, "analyze", false, "analyse FITS files given as arguments (also: drag & drop FITS files onto the exe)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "sbigdiag %s - SBIG STX/STXL camera diagnostics\n\nUsage: sbigdiag [options] [file.fits ...]\n", version)
		fmt.Fprintln(os.Stderr, "Run without options for an interactive menu. Options:")
		flag.PrintDefaults()
	}
	flag.Parse()
	disableQuickEdit()
	code := run(o)
	if !o.noPause && ownsConsole() {
		flushConsoleInput() // ignore keys typed during the run
		fmt.Print("\nPress Enter to close this window...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(code)
}

func run(o options) int {
	files := flag.Args()
	interactive := !o.info && !o.quick && !o.full && o.custom == "" && len(files) == 0 && !o.analyze
	fmt.Printf("sbigdiag %s (%s/%s) - SBIG STX/STXL diagnostic tool\n", version, runtime.GOOS, runtime.GOARCH)
	if len(files) > 0 {
		return analyzeFiles(files, o)
	}
	in := bufio.NewReader(os.Stdin)
	if o.analyze {
		fmt.Print("FITS/XISF file(s) or a folder: ")
		l, _ := in.ReadString('\n')
		return analyzeFiles(splitPaths(l), o)
	}
	if interactive {
		fmt.Println(`
Close NINA / CCDOps / MaxIm (or disconnect the camera in them) before using options 1-4.
  1) Camera and driver information only (no exposures)              ~ 30 s
  2) Quick test: information + 5 bias frames with analysis           ~ 2 min
  3) FULL diagnostic: information + all experiments (recommended)    ~ 10 min
  4) Custom series (choose exposure, binning, subframe, settings)
  5) Analyse existing FITS/XISF files (e.g. from NINA) - no camera needed
  6) Demo with a simulated faulty camera (no hardware needed)
  q) Quit`)
		fmt.Print("Choice [3]: ")
		line, _ := in.ReadString('\n')
		switch strings.TrimSpace(strings.ToLower(line)) {
		case "1":
			o.info = true
		case "2":
			o.quick = true
		case "", "3":
			o.full = true
		case "4":
			o.custom = askCustom(in)
			fmt.Print("Temporary driver settings (e.g. VDD_OPTIMIZED=0, empty = none): ")
			l, _ := in.ReadString('\n')
			o.sets = strings.TrimSpace(l)
			fmt.Print("Number of frames [5]: ")
			l, _ = in.ReadString('\n')
			if n, err := strconv.Atoi(strings.TrimSpace(l)); err == nil && n > 0 {
				o.frames = n
			}
		case "5":
			fmt.Print("FITS/XISF file or folder (drag ONE file or a folder into this window, then Enter): ")
			l, _ := in.ReadString('\n')
			return analyzeFiles(splitPaths(l), o)
		case "6":
			o.sim, o.full = true, true
		default:
			return 0
		}
	}
	return runCamera(o)
}

func askCustom(in *bufio.Reader) string {
	ask := func(q, def string) string {
		fmt.Printf("%s [%s]: ", q, def)
		l, _ := in.ReadString('\n')
		l = strings.TrimSpace(l)
		if l == "" {
			return def
		}
		return l
	}
	exp := ask("Exposure seconds (0 = bias)", "0")
	bin := ask("Binning 1, 2 or 3", "1")
	dk := ask("Dark (shutter closed) 1/0", "1")
	top := ask("Subframe top row (binned px)", "0")
	h := ask("Subframe height (0 = full)", "0")
	return fmt.Sprintf("exp=%s,bin=%s,dark=%s,top=%s,height=%s", exp, bin, dk, top, h)
}

func splitPaths(l string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(l, func(r rune) bool { return r == ';' || r == '\n' || r == '\r' }) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, err := os.Stat(strings.Trim(part, `"`)); err == nil { // a typed path with spaces
			out = append(out, strings.Trim(part, `"`))
			continue
		}
		// Several dropped files: space separated, quoted only when they contain spaces.
		var cur strings.Builder
		inQ := false
		flush := func() {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
		for _, r := range part {
			switch {
			case r == '"':
				inQ = !inQ
			case (r == ' ' || r == '\t') && !inQ:
				flush()
			default:
				cur.WriteRune(r)
			}
		}
		flush()
	}
	return out
}

func parseCustom(s string) (ExposureSpec, error) {
	spec := ExposureSpec{Dark: true}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return spec, fmt.Errorf("bad item %q", kv)
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		fv, ferr := strconv.ParseFloat(v, 64)
		iv := int(fv)
		if k != "linedelay" && ferr != nil {
			return spec, fmt.Errorf("bad value in %q", kv)
		}
		switch k {
		case "exp":
			spec.Seconds = fv
		case "bin":
			if iv < 1 || iv > 3 {
				return spec, fmt.Errorf("bin must be 1, 2 or 3")
			}
			spec.Mode = uint16(iv - 1)
		case "mode":
			spec.Mode = uint16(iv)
		case "dark":
			spec.Dark = iv != 0
		case "top":
			spec.Top = iv
		case "left":
			spec.Left = iv
		case "height":
			spec.Height = iv
		case "width":
			spec.Width = iv
		case "skipvdd":
			if iv != 0 {
				spec.CCDFlags |= START_SKIP_VDD
			}
		case "linedelay":
			d, err := time.ParseDuration(v)
			if err != nil {
				return spec, err
			}
			spec.LineDelay = d
		default:
			return spec, fmt.Errorf("unknown key %q", k)
		}
	}
	return spec, nil
}

func makeOutDir(o options) string {
	if o.outDir != "" {
		if err := os.MkdirAll(o.outDir, 0o755); err == nil {
			return o.outDir
		}
	}
	name := "sbigdiag_" + time.Now().Format("20060102_150405")
	var bases []string
	if exe, err := os.Executable(); err == nil {
		bases = append(bases, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		bases = append(bases, wd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		bases = append(bases, filepath.Join(home, "Desktop"), home)
	}
	bases = append(bases, os.TempDir())
	for _, b := range bases {
		if protectedDir(b) {
			continue
		}
		d := filepath.Join(b, name)
		if err := os.MkdirAll(d, 0o755); err == nil {
			return d
		}
	}
	return ""
}

// protectedDir: Windows silently redirects writes of a non-elevated 32-bit
// program under these folders to VirtualStore, where nobody would find them.
func protectedDir(p string) bool {
	lp := strings.ToLower(filepath.Clean(p))
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "SystemRoot", "ProgramData"} {
		if v := os.Getenv(env); v != "" {
			lv := strings.ToLower(filepath.Clean(v))
			if lp == lv || strings.HasPrefix(lp, lv+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func runCamera(o options) (code int) {
	if o.sets != "" && !o.info && o.custom == "" && !o.quick {
		fmt.Println("-set is only supported with -custom or -quick (the full test changes settings itself)")
		return 2
	}
	dir := makeOutDir(o)
	var logf *os.File
	var out io.Writer = os.Stdout
	var trace io.Writer
	if dir != "" {
		if f, err := os.Create(filepath.Join(dir, "report.txt")); err == nil {
			logf = f
			defer f.Close()
			out = io.MultiWriter(os.Stdout, f)
		}
		if f, err := os.Create(filepath.Join(dir, "driver_calls.log")); err == nil {
			defer f.Close()
			trace = f
		}
	}
	_ = logf
	fmt.Fprintf(out, "sbigdiag %s  started %s  output folder: %s\n", version, time.Now().Format("2006-01-02 15:04:05 MST"), dir)
	sysInfo(out)
	regSnap := snapshotSBIGRegistry()
	eventLog := "" // SBIG driver debug log file (camera events), set once enabled

	var t transport
	var err error
	if o.sim || o.simHealthy {
		t = newSimTransport(o.simHealthy)
		fmt.Fprintln(out, "*** SIMULATED CAMERA - these are not real data ***")
	} else {
		t, err = loadDriverDLL(o.dll)
		if err != nil {
			fmt.Fprintln(out, "ERROR:", err)
			return 2
		}
	}
	for _, l := range t.Describe() {
		fmt.Fprintln(out, l)
	}
	cam := NewCamera(t, trace)
	s := &Session{cam: cam, out: out, dir: dir, device: o.device, ip: o.ip, saveFITS: !o.noFITS, restSec: o.rest,
		origDCP: map[int]uint32{}, info: map[string]interface{}{"started": time.Now().Format(time.RFC3339)}}

	// Ctrl+C / window close: stop at the next safe point, restore settings, close the camera.
	// While camera customer options (stored in the camera) are changed, further
	// interrupts do not kill the program until they are restored.
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		n := 0
		for sg := range sig {
			n++
			if n == 1 {
				fmt.Println("\n*** Interrupt: finishing the current driver call, then restoring settings and closing the camera ***")
				s.abort.Store(true)
				continue
			}
			if s.nvPending.Load() && (sg == syscall.SIGTERM || n < 4) {
				fmt.Printf("\n*** Camera customer options are changed IN THE CAMERA right now (original: %v) - restoring them, please wait"+
					" (press Ctrl+C %d more time(s) to quit anyway, then reset them in CCDOps) ***\n", s.origOptsText.Load(), 4-n)
				continue
			}
			fmt.Println("*** Exiting immediately (settings may not be restored) ***")
			os.Exit(130)
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(out, "\nINTERNAL ERROR: %v\n", r)
			code = 4
		}
		fmt.Fprintln(out, "\n== Restoring settings and closing ==")
		if !s.RestoreAll() {
			fmt.Fprintln(out, "WARNING: some settings could not be restored - see above. Driver settings are per-process and")
			fmt.Fprintln(out, "         vanish when this program exits; camera customer options are stored in the camera.")
		} else {
			fmt.Fprintln(out, "All changed settings are back to their original values.")
		}
		if eventLog != "" {
			// The registry already holds the original log settings again: reloading them
			// while the device is still open switches our logging off.
			_ = cam.ReloadDebugLog()
		}
		cam.Shutdown()
		for _, l := range restoreSBIGRegistry(regSnap) {
			fmt.Fprintln(out, "registry: "+l)
		}
		if eventLog != "" {
			summarizeEventLog(out, eventLog)
		}
		s.WriteJSON()
		fmt.Fprintf(out, "Results saved in: %s\n", dir)
	}()

	if err := s.Connect(); err != nil {
		fmt.Fprintln(out, "ERROR:", err)
		return 3
	}
	// Camera event messages (e.g. "LV TE Off" = input voltage too low): the driver
	// takes its debug-log settings only from HKCU\Software\SBIG\SBIGUDRV\DebugLog and
	// only while a device is open. Write them, let the driver load them, and put the
	// user's registry back straight away (the driver keeps the settings in memory).
	if (o.driverLog || !o.noEventLog) && dir != "" && !s.aborted() {
		long, _ := filepath.Abs(filepath.Join(dir, "sbigudrv_debug.log"))
		if p, ok := ansiPath(long); !ok {
			fmt.Fprintln(out, "SBIG driver camera-event log: skipped (output path not representable for the driver)")
		} else {
			flags := [4]uint32{0, 0, 0x80, 0} // DLF_FCE_CAMERA: camera event messages
			if o.driverLog {
				flags = [4]uint32{0x0F, 0x1F, 0x83, 0}
			}
			if err := setDebugLogRegistry(p, flags); err != nil {
				fmt.Fprintf(out, "SBIG driver camera-event log: not enabled (%v)\n", err)
			} else {
				err := cam.ReloadDebugLog()
				restored := restoreSBIGRegistry(regSnap)
				fmt.Fprintf(out, "SBIG driver camera-event log -> %s (%v; %d temporary registry value(s) put back)\n", p, errOrOK(err), len(restored))
				if err == nil {
					eventLog = long
				}
			}
		}
	}
	s.Info()
	if o.info || s.aborted() {
		return 0
	}
	// Temporary settings for quick/custom runs.
	if o.sets != "" {
		for _, kv := range strings.Split(o.sets, ",") {
			if s.aborted() {
				break
			}
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if !ok {
				continue
			}
			i := dcpIndex(strings.ToUpper(strings.TrimSpace(k)))
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
			if i < 0 || err != nil {
				fmt.Fprintf(out, "ignoring bad setting %q\n", kv)
				continue
			}
			if err := s.SetDCPTemp(i, uint32(n)); err != nil {
				fmt.Fprintf(out, "could not set %s: %v\n", dcpNames[i], err)
			}
		}
	}
	switch {
	case o.custom != "":
		spec, err := parseCustom(o.custom)
		if err != nil {
			fmt.Fprintln(out, "bad -custom:", err)
			return 2
		}
		if s.heading("custom series", o.custom+" "+o.sets) {
			s.series("custom", spec, o.frames)
			s.SampleOffsets("after custom series", 3)
		}
	case o.quick:
		s.QuickTest(o.frames)
	default:
		s.FullTest()
	}
	s.PrintTable()
	s.Interpret()
	return 0
}

func analyzeFiles(paths []string, o options) int {
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			fmt.Printf("%s: %v\n", p, err)
			continue
		}
		if st.IsDir() {
			for _, pat := range []string{"*.fits", "*.fit", "*.fts", "*.FITS", "*.FIT", "*.FTS", "*.xisf", "*.XISF"} {
				m, _ := filepath.Glob(filepath.Join(p, pat))
				files = append(files, m...)
			}
		} else {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		fmt.Println("no FITS/XISF files to analyse")
		return 1
	}
	s := &Session{out: os.Stdout, info: map[string]interface{}{}}
	for i, f := range files {
		pix, w, h, hdr, err := ReadImage(f)
		if err != nil {
			fmt.Printf("\n--- %s: %v\n", f, err)
			continue
		}
		fmt.Printf("\n--- file %d: %s ---\n", i+1, f)
		fmt.Printf("  EXPTIME=%s IMAGETYP=%s XBINNING=%s CCD-TEMP=%s DATE-OBS=%s INSTRUME=%s\n",
			hdr["EXPTIME"], hdr["IMAGETYP"], hdr["XBINNING"], hdr["CCD-TEMP"], hdr["DATE-OBS"], hdr["INSTRUME"])
		a := Analyze(pix, w, h)
		fmt.Printf("  => %s\n", a.Class)
		for _, l := range a.Summary() {
			fmt.Println("  " + l)
		}
		fmt.Println("  " + a.BitLine())
		for _, l := range Thumbnail(pix, w, h, 64, 16) {
			fmt.Println(l)
		}
		s.results = append(s.results, &FrameResult{N: i + 1, Experiment: filepath.Base(f), Spec: "file", Class: a.Class, W: w, H: h, Stats: a.Stats, Boundaries: a.Boundaries})
	}
	fmt.Println("\n" + levelLegend)
	s.PrintTable()
	return 0
}

// summarizeEventLog prints the camera events the SBIG driver logged.
func summarizeEventLog(out io.Writer, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(out, "SBIG driver debug log: not written (%v)\n", err)
		return
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n")
	var hits []string
	for _, l := range lines {
		u := strings.ToUpper(l)
		if strings.Contains(u, "LV ") || strings.Contains(u, "TE OFF") || strings.Contains(u, "EVENT") || strings.Contains(u, "ERROR") ||
			strings.Contains(u, "FAIL") || strings.Contains(u, "VOLT") || strings.Contains(u, "TIMEOUT") {
			hits = append(hits, strings.TrimSpace(l))
		}
	}
	fmt.Fprintf(out, "SBIG driver debug log: %d lines, %d look like camera events/errors (%s)\n", len(lines), len(hits), path)
	for i, h := range hits {
		if i >= 25 {
			fmt.Fprintf(out, "  ... %d more\n", len(hits)-25)
			break
		}
		fmt.Fprintln(out, "  "+h)
	}
	for _, h := range hits {
		if strings.Contains(strings.ToUpper(h), "LV") {
			fmt.Fprintln(out, "  !!! The camera reported a LOW-VOLTAGE event: check the 12/13 V supply (>= 10 A), cable and connector.")
			break
		}
	}
}

func errOrOK(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}
