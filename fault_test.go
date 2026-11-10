package main

import (
	"bytes"
	"os"
	"testing"
)

// faultTransport wraps the simulator, records every call and can fail chosen calls.
type faultTransport struct {
	*simTransport
	cmds   []uint16
	params [][]byte
	counts map[uint16]int
	fail   func(cmd uint16, p []byte, n int) (uint16, bool) // n = how many times cmd was called so far
}

func newFault(fail func(cmd uint16, p []byte, n int) (uint16, bool)) *faultTransport {
	return &faultTransport{simTransport: newSimTransport(true), counts: map[uint16]int{}, fail: fail}
}

func (f *faultTransport) Call(cmd uint16, p, r []byte) uint16 {
	f.counts[cmd]++
	f.cmds = append(f.cmds, cmd)
	f.params = append(f.params, append([]byte(nil), p...))
	if f.fail != nil {
		if code, ok := f.fail(cmd, p, f.counts[cmd]); ok {
			return code
		}
	}
	return f.simTransport.Call(cmd, p, r)
}

func testSession(t *testing.T, tr transport) (*Session, *bytes.Buffer) {
	t.Helper()
	cfg := t.TempDir() // the restore journal lives in the user config dir
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("APPDATA", cfg)
	t.Setenv("HOME", cfg)
	var buf bytes.Buffer
	s := &Session{cam: NewCamera(tr, nil), out: &buf, device: "usb", origDCP: map[int]uint32{},
		info: map[string]interface{}{"status": "running"}}
	if err := s.Connect(); err != nil {
		t.Fatal(err)
	}
	return s, &buf
}

func TestDCPRestoredAfterCaptureError(t *testing.T) {
	tr := newFault(func(cmd uint16, p []byte, n int) (uint16, bool) {
		return CE_BAD_PARAMETER, cmd == CC_START_EXPOSURE2
	})
	s, out := testSession(t, tr)
	s.withDCP(DCP_VDD_OPTIMIZED, 0, func() { s.series("x", bias(0), 1) })
	if tr.dcp[DCP_VDD_OPTIMIZED] != 1 || len(s.origDCP) != 0 || s.halted.Load() {
		t.Fatalf("VDD_OPTIMIZED=%d origDCP=%v halted=%v\n%s", tr.dcp[DCP_VDD_OPTIMIZED], s.origDCP, s.halted.Load(), out)
	}
	if len(s.results) != 1 || s.results[0].Class != "ERROR" {
		t.Errorf("expected one ERROR frame, got %+v", s.results)
	}
}

func TestFailedDCPRestoreHalts(t *testing.T) {
	tr := newFault(func(cmd uint16, p []byte, n int) (uint16, bool) {
		// refuse to write the original value back
		return CE_BAD_PARAMETER, cmd == CC_SET_DRIVER_CONTROL && L_SetDriverControlParams.U32(p, "controlValue") == 1
	})
	s, out := testSession(t, tr)
	s.withDCP(DCP_VDD_OPTIMIZED, 0, func() {})
	if !s.halted.Load() {
		t.Fatalf("a failed restore must halt the run\n%s", out)
	}
	if s.heading("next", "") {
		t.Error("heading must refuse further experiments after a halt")
	}
}

func TestOptionsRestoredAfterReadoutFailure(t *testing.T) {
	tr := newFault(func(cmd uint16, p []byte, n int) (uint16, bool) {
		return 8, cmd == CC_READOUT_LINE && n == 10 // CE_RX_TIMEOUT in the middle of a readout
	})
	s, out := testSession(t, tr)
	orig := tr.opts
	changed := orig
	changed.VddNormallyOff = 1
	s.withOptions(changed, func() { s.series("x", bias(0), 2) })
	if tr.opts != orig || s.nvPending.Load() || s.halted.Load() {
		t.Fatalf("options %v (want %v) nvPending=%v halted=%v\n%s", tr.opts, orig, s.nvPending.Load(), s.halted.Load(), out)
	}
	if _, err := os.Stat(journalPath()); err == nil {
		t.Error("journal must be removed after a verified restore")
	}
	if s.results[0].Class != "PARTIAL" || s.results[1].Class == "PARTIAL" {
		t.Errorf("first frame should be PARTIAL, second complete: %s %s", s.results[0].Class, s.results[1].Class)
	}
}

func TestJournalSurvivesFailedRestoreAndIsRecovered(t *testing.T) {
	var orig CustomerOptions
	tr := newFault(func(cmd uint16, p []byte, n int) (uint16, bool) {
		L := L_CustomerOptionsParams
		isRestore := cmd == CC_CUSTOMER_OPTIONS && L.U16(p, "bSetCustomerOptions") == 1 && L.U16(p, "bVddNormallyOff") == orig.VddNormallyOff
		return CE_BAD_PARAMETER, isRestore
	})
	s, out := testSession(t, tr)
	orig = tr.opts
	changed := orig
	changed.VddNormallyOff = 1
	s.withOptions(changed, func() {})
	if !s.halted.Load() || !s.nvPending.Load() {
		t.Fatalf("failed options restore must halt and stay pending\n%s", out)
	}
	if _, err := os.Stat(journalPath()); err != nil {
		t.Fatalf("journal must survive a failed restore: %v", err)
	}
	// Next start: a healthy driver, camera still has the changed options.
	tr2 := newFault(nil)
	tr2.opts = changed
	s2 := &Session{cam: NewCamera(tr2, nil), out: out, device: "usb", origDCP: map[int]uint32{}, info: map[string]interface{}{}}
	if err := s2.Connect(); err != nil {
		t.Fatal(err)
	}
	s2.RecoverJournal()
	if tr2.opts != orig {
		t.Fatalf("journal recovery did not restore %v (camera has %v)\n%s", orig, tr2.opts, out)
	}
	if _, err := os.Stat(journalPath()); err == nil {
		t.Error("journal must be removed after recovery")
	}
}

func TestInfoModeNeverWrites(t *testing.T) {
	tr := newFault(nil)
	s, _ := testSession(t, tr)
	s.Info()
	for i, c := range tr.cmds {
		switch c {
		case CC_SET_DRIVER_CONTROL, CC_MISCELLANEOUS_CONTROL, CC_SET_TEMPERATURE_REGULATION2, CC_SET_RBI_PARAMETERS, CC_READOUT_IN_PROGRESS, CC_USB_AD_CONTROL:
			t.Errorf("info mode sent %s", cmdName(c))
		case CC_CUSTOMER_OPTIONS:
			if L_CustomerOptionsParams.U16(tr.params[i], "bSetCustomerOptions") != 0 {
				t.Error("info mode wrote camera customer options")
			}
		case CC_BIT_IO:
			if L_BitIOParams.U16(tr.params[i], "bitOperation") != BITIO_READ {
				t.Error("info mode wrote a BIT_IO bit")
			}
		}
	}
}

func TestAbortBeforeExperimentsChangesNothing(t *testing.T) {
	tr := newFault(nil)
	s, _ := testSession(t, tr)
	s.abort.Store(true)
	n := len(tr.cmds)
	s.FullTest()
	for _, c := range tr.cmds[n:] {
		if c == CC_SET_DRIVER_CONTROL || c == CC_CUSTOMER_OPTIONS || c == CC_START_EXPOSURE2 {
			t.Errorf("aborted run still sent %s", cmdName(c))
		}
	}
}
