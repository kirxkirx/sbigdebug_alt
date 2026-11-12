package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupFailureStopsSeries(t *testing.T) {
	for _, cmd := range []uint16{CC_END_EXPOSURE, CC_END_READOUT} {
		t.Run(cmdName(cmd), func(t *testing.T) {
			tr := newFault(func(c uint16, p []byte, n int) (uint16, bool) { return 8, c == cmd })
			cam := NewCamera(tr, nil)
			cam.Info = &CCDInfo{Modes: []ReadoutMode{{Mode: 0, Width: 4, Height: 3}}}
			s := &Session{cam: cam, out: &bytes.Buffer{}, info: map[string]interface{}{}, origDCP: map[int]uint32{}}
			frames := s.series("cleanup", bias(0), 3)
			if !s.halted.Load() || len(frames) != 1 || tr.counts[CC_START_EXPOSURE2] != 1 {
				t.Fatalf("did not stop: halted=%v frames=%d exposures=%d", s.halted.Load(), len(frames), tr.counts[CC_START_EXPOSURE2])
			}
			if frames[0].Class != "ERROR" || !strings.Contains(frames[0].FatalErr, errCameraState.Error()) {
				t.Fatalf("failed cleanup considered valid: %+v", frames[0])
			}
			if cmd == CC_END_EXPOSURE && tr.counts[CC_START_READOUT] != 0 {
				t.Fatal("readout started after failed EndExposure")
			}
			if tr.counts[CC_END_READOUT] == 0 {
				t.Fatal("readout cleanup was not attempted")
			}
		})
	}
}

func TestAbortCleansExposureAndReadout(t *testing.T) {
	tr := newFault(nil)
	cam := NewCamera(tr, nil)
	cam.Info = &CCDInfo{Modes: []ReadoutMode{{Mode: 0, Width: 4, Height: 3}}}
	_, err := cam.Expose(bias(0), func() bool { return true })
	if !errors.Is(err, errAborted) || tr.counts[CC_END_EXPOSURE] != 1 || tr.counts[CC_END_READOUT] != 1 {
		t.Fatalf("cleanup missing: %v calls %v", err, tr.counts)
	}
}

func TestRepeatedDCPFailureKeepsOriginal(t *testing.T) {
	tr := newFault(func(cmd uint16, p []byte, n int) (uint16, bool) {
		return CE_BAD_PARAMETER, cmd == CC_SET_DRIVER_CONTROL && n == 2
	})
	s, _ := testSession(t, tr)
	if err := s.SetDCPTemp(DCP_VDD_OPTIMIZED, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDCPTemp(DCP_VDD_OPTIMIZED, 1); err == nil {
		t.Fatal("fault not injected")
	}
	if !s.RestoreAll() || tr.dcp[DCP_VDD_OPTIMIZED] != 1 {
		t.Fatalf("original value lost: %v", tr.dcp)
	}
}

func TestJournalFailurePreventsPersistentWriteAndUnneededRestore(t *testing.T) {
	tr := newFault(nil)
	s, _ := testSession(t, tr)
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", blocked)
	t.Setenv("APPDATA", blocked)
	original := tr.opts
	changed := original
	changed.VddNormallyOff = 1
	calls := len(tr.cmds)
	ran := false
	s.withOptions(changed, func() { ran = true })
	if ran || tr.opts != original || s.origOpts != nil || s.nvPending.Load() {
		t.Fatalf("changed without journal: options=%v pending=%v", tr.opts, s.nvPending.Load())
	}
	for i, c := range tr.cmds[calls:] {
		if c == CC_CUSTOMER_OPTIONS && L_CustomerOptionsParams.U16(tr.params[calls+i], "bSetCustomerOptions") != 0 {
			t.Fatal("persistent write issued despite failed journal")
		}
	}
}

func TestUnknownSerialCannotRecoverOrChangeOptions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	tr := newFault(func(cmd uint16, p []byte, n int) (uint16, bool) {
		return CE_BAD_PARAMETER, cmd == CC_GET_CCD_INFO && L_GetCCDInfoParams.U16(p, "request") == 2
	})
	s := newSession2(t, tr, func(string) bool { t.Error("must not offer automatic recovery for unknown identity"); return true })
	original := tr.opts
	changed := original
	changed.VddNormallyOff = 1
	tr.opts = changed
	if err := writeJournal(Journal{Original: original, ChangedTo: changed, Time: time.Now().Format(time.RFC3339), CameraType: s.cam.CameraType}); err != nil {
		t.Fatal(err)
	}
	s.RecoverJournal(true)
	if tr.opts != changed || s.journalBlock == "" {
		t.Fatal("unknown camera identity accepted")
	}
	if err := s.SetOptionsTemp(original); err == nil {
		t.Fatal("persistent write allowed without serial")
	}
	if _, err := os.Stat(journalPath()); err != nil {
		t.Fatal("unresolved journal not retained")
	}
}

func TestPreviewIncludesPartialEdgeBlocks(t *testing.T) {
	const w, h = 1001, 21
	pix := make([]uint16, w*h)
	for i := range pix {
		pix[i] = 1000
	}
	stats := computeStats(pix)
	for x := 0; x < w; x++ {
		pix[(h-1)*w+x] = 65535
	}
	for y := 0; y < h; y++ {
		pix[y*w+w-1] = 65535
	}
	path := filepath.Join(t.TempDir(), "preview.png")
	if err := WritePreview(path, pix, w, h, stats); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	pw := (img.Bounds().Dx() - 4) / 2
	for _, pt := range [][2]int{{0, img.Bounds().Dy() - 1}, {pw - 1, 0}} {
		r, _, _, _ := img.At(pt[0], pt[1]).RGBA()
		if r != 65535 {
			t.Fatalf("edge block omitted/diluted at %v: %d", pt, r)
		}
	}
}

func reviewFITS(t *testing.T, cards []fitsCard, data []byte) string {
	t.Helper()
	var b bytes.Buffer
	for _, c := range cards {
		b.WriteString(formatCard(c))
	}
	b.WriteString(fmt.Sprintf("%-80s", "END"))
	for b.Len()%2880 != 0 {
		b.WriteByte(' ')
	}
	b.Write(data)
	p := filepath.Join(t.TempDir(), "input.fits")
	if err := os.WriteFile(p, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFITSRejectsMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name         string
		bitpix, w, h interface{}
		data         []byte
	}{
		{"negative", 16, -1, 1, nil}, {"overflow", 16, int64(1 << 62), 4, nil}, {"invalid", 16, "abc", 2, nil},
		{"unsupported", 12, 1, 1, []byte{0, 0}}, {"truncated", 16, 2, 2, []byte{0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := reviewFITS(t, []fitsCard{{"SIMPLE", true, ""}, {"BITPIX", tc.bitpix, ""}, {"NAXIS", 2, ""}, {"NAXIS1", tc.w, ""}, {"NAXIS2", tc.h, ""}}, tc.data)
			if _, _, _, _, err := ReadFITS(p); err == nil {
				t.Fatal("malformed FITS accepted")
			}
		})
	}
	if _, err := imageSize(1<<62, 4, 2); err == nil {
		t.Fatal("overflow accepted")
	}
	if _, err := imageSize(32000001, 1, 2); err == nil {
		t.Fatal("budget ignored")
	}
}

func reviewXISF(t *testing.T, header string, data []byte) string {
	t.Helper()
	b := make([]byte, 4096+len(data))
	copy(b, "XISF0100")
	binary.LittleEndian.PutUint32(b[8:], uint32(len(header)))
	copy(b[16:], header)
	copy(b[4096:], data)
	p := filepath.Join(t.TempDir(), "input.xisf")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestXISFValidatesGeometryLayoutAndAttachment(t *testing.T) {
	for _, attrs := range []string{
		`geometry="4611686018427387904:1:1" sampleFormat="UInt16" location="attachment:4096:2"`,
		`geometry="2:1:3" sampleFormat="UInt16" pixelStorage="Normal" location="attachment:4096:12"`,
		`geometry="1:1:1" sampleFormat="UInt16" location="attachment:4096:1"`,
		`geometry="1:1:1" sampleFormat="UInt16" location="attachment:8:2"`,
		`geometry="1:1:1" sampleFormat="UInt16" location="attachment:4096:2" byteOrder="garbled"`,
	} {
		p := reviewXISF(t, `<xisf version="1.0"><Image `+attrs+`/></xisf>`, []byte{1, 0})
		if _, _, _, _, err := ReadXISF(p); err == nil {
			t.Fatalf("invalid XISF accepted: %s", attrs)
		}
	}
	// XML single quotes, attribute order, and entity decoding work without regex assumptions.
	p := reviewXISF(t, `<xisf version='1.0'><Image location='attachment:4096:2' sampleFormat='UInt16' geometry='1:1:1'><FITSKeyword value='Bias Frame' name='IMAGETYP'/><FITSKeyword value='A &amp; B' name='OBJECT'/></Image></xisf>`, []byte{232, 3})
	pixels, w, h, hdr, err := ReadXISF(p)
	if err != nil || w != 1 || h != 1 || pixels[0] != 1000 || hdr["OBJECT"] != "A & B" || !darkImageType(hdr["IMAGETYP"]) {
		t.Fatalf("valid XML not parsed: %v %v %v", pixels, hdr, err)
	}
}

func TestLiveAndOfflineDarkAnalysisAgree(t *testing.T) {
	pix := make([]uint16, 40*30)
	for i := range pix {
		pix[i] = 10000
	}
	live := AnalyzeFrame(pix, 40, 30, true)
	for _, typ := range []string{"BIAS", "Dark Frame", " bias frame "} {
		offline := AnalyzeFrame(pix, 40, 30, darkImageType(typ))
		if live.Class != "ELEVATED" || live.Class != offline.Class {
			t.Fatalf("live %s offline %s", live.Class, offline.Class)
		}
	}
	for i := range pix {
		pix[i] = 50000
	}
	a := AnalyzeFrame(pix, 40, 30, true)
	if a.Class == "SATURATED" || a.Stats.SatFrac != 0 {
		t.Fatal("high ADU mistaken for measured saturation")
	}
}

func TestQuickTestDoesNotIssueDiagnosticsAfterCleanupFailure(t *testing.T) {
	tr := newFault(func(c uint16, p []byte, n int) (uint16, bool) { return 8, c == CC_END_READOUT })
	cam := NewCamera(tr, nil)
	cam.Info = &CCDInfo{Modes: []ReadoutMode{{Mode: 0, Width: 4, Height: 3}}}
	var out bytes.Buffer
	s := &Session{cam: cam, out: &out, info: map[string]interface{}{}, origDCP: map[int]uint32{}}
	s.QuickTest(3)
	if tr.counts[CC_START_EXPOSURE2] != 1 || tr.counts[CC_READ_OFFSET2] != 0 || tr.counts[CC_READ_OFFSET] != 0 {
		t.Fatalf("continued diagnostics after failure: %v", tr.counts)
	}
	if !strings.Contains(out.String(), "=> ERROR") || strings.Contains(out.String(), "=> OK") {
		t.Fatalf("console misclassified failed cleanup:\n%s", &out)
	}
	calls := len(tr.cmds)
	if s.SetDCPTemp(DCP_VDD_OPTIMIZED, 0) == nil || s.SetOptionsTemp(tr.opts) == nil {
		t.Fatal("halted run accepted a new setting change")
	}
	if len(tr.cmds) != calls {
		t.Fatal("halted setting operation called the driver")
	}
}

func TestAbortedSessionRefusesNewSettingsButAllowsRestore(t *testing.T) {
	tr := newFault(nil)
	s, _ := testSession(t, tr)
	if err := s.SetDCPTemp(DCP_VDD_OPTIMIZED, 0); err != nil {
		t.Fatal(err)
	}
	s.abort.Store(true)
	if !errors.Is(s.SetDCPTemp(DCP_VDD_OPTIMIZED, 1), errAborted) || !errors.Is(s.SetOptionsTemp(tr.opts), errAborted) {
		t.Fatal("aborted session accepted settings")
	}
	if !s.RestoreAll() || tr.dcp[DCP_VDD_OPTIMIZED] != 1 {
		t.Fatal("aborting prevented restoration")
	}
}

func TestFITSDataMaxDoesNotNormalizePhysicalADU(t *testing.T) {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, math.Float64bits(1))
	path := reviewFITS(t, []fitsCard{{"SIMPLE", true, ""}, {"BITPIX", -64, ""}, {"NAXIS", 2, ""}, {"NAXIS1", 1, ""}, {"NAXIS2", 1, ""}, {"DATAMAX", 1.0, ""}}, data)
	pixels, _, _, hdr, err := ReadFITS(path)
	if err != nil || len(pixels) != 1 || pixels[0] != 1 || strings.Contains(hdr["SBIGDIAG_CONVERSION"], "normalized") {
		t.Fatalf("DATAMAX changed physical values: %v %v %v", pixels, hdr, err)
	}
}

func TestFITSRejectsUndefinedAmbiguousAndInexactInput(t *testing.T) {
	base := []fitsCard{{"SIMPLE", true, ""}, {"BITPIX", 16, ""}, {"NAXIS", 2, ""}, {"NAXIS1", 1, ""}, {"NAXIS2", 1, ""}}
	for _, extra := range [][]fitsCard{
		{{"BLANK", 0, ""}, {"BZERO", 32768, ""}},
		{{"BITPIX", 16, ""}},
		{{"SIMPLE", false, ""}},
	} {
		cards := append(append([]fitsCard(nil), base...), extra...)
		if _, _, _, _, err := ReadFITS(reviewFITS(t, cards, []byte{0, 0})); err == nil {
			t.Fatalf("invalid structural/undefined input accepted: %v", extra)
		}
	}
	for _, mutate := range []func([]fitsCard){
		func(c []fitsCard) { c[0].key = "NOTFITS" },
		func(c []fitsCard) { c[0].value = false },
		func(c []fitsCard) { c[1].value = 64 },
	} {
		cards := append([]fitsCard(nil), base...)
		mutate(cards)
		if _, _, _, _, err := ReadFITS(reviewFITS(t, cards, make([]byte, 8))); err == nil {
			t.Fatal("invalid/inexact input accepted")
		}
	}
	if _, err := imageSize(65536, 1, 2); err == nil {
		t.Fatal("oversized axis accepted")
	}
	if _, _, _, _, err := ReadImage(t.TempDir()); err == nil {
		t.Fatal("directory accepted as image input")
	}
}

func TestOfflineBatchFailureReturnsNonzero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.fits")
	if err := os.WriteFile(path, []byte("invalid input"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := analyzeFiles([]string{path}, options{}); code == 0 {
		t.Fatal("failed offline batch returned success")
	}
}
