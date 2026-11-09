package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
)

type transport interface {
	Call(cmd uint16, params, results []byte) uint16
	Close() error
	Describe() []string
}

// SBIGError is a non-zero return code of SBIGUnivDrvCommand.
type SBIGError struct {
	Cmd  uint16
	Code uint16
	Text string
}

func (e *SBIGError) Error() string {
	return fmt.Sprintf("%s failed: %s (%d) %q", cmdName(e.Cmd), errName(e.Code), e.Code, e.Text)
}

func isCode(err error, codes ...uint16) bool {
	var se *SBIGError
	if errors.As(err, &se) {
		for _, c := range codes {
			if se.Code == c {
				return true
			}
		}
	}
	return false
}

type Camera struct {
	t          transport
	trace      io.Writer
	driverOpen bool
	deviceOpen bool
	linked     bool
	CameraType uint16
	Info       *CCDInfo
}

func NewCamera(t transport, trace io.Writer) *Camera { return &Camera{t: t, trace: trace} }

func (c *Camera) tracef(format string, a ...interface{}) {
	if c.trace != nil {
		fmt.Fprintf(c.trace, time.Now().Format("15:04:05.000")+" "+format+"\n", a...)
	}
}

func (c *Camera) cmd(id uint16, p, r []byte) error {
	t0 := time.Now()
	code := c.t.Call(id, p, r)
	if id != CC_READOUT_LINE || code != 0 {
		c.tracef("%-30s rc=%-3d %-24s %8.1f ms params=% x", cmdName(id), code, errName(code),
			float64(time.Since(t0).Microseconds())/1000, p)
	}
	if code == 0 {
		return nil
	}
	return &SBIGError{Cmd: id, Code: code, Text: c.errorString(code)}
}

func (c *Camera) errorString(code uint16) string {
	p := L_GetErrorStringParams.New()
	L_GetErrorStringParams.Put16(p, "errorNo", code)
	r := L_GetErrorStringResults.New()
	if c.t.Call(CC_GET_ERROR_STRING, p, r) != 0 {
		return ""
	}
	return L_GetErrorStringResults.Str(r, "errorString", 64)
}

// --- driver / device ---

func (c *Camera) OpenDriver() error {
	err := c.cmd(CC_OPEN_DRIVER, nil, nil)
	if isCode(err, CE_DRIVER_NOT_CLOSED) {
		// Already open in this process (shared DLL state) - that's fine.
		err = nil
	}
	if err == nil {
		c.driverOpen = true
	}
	return err
}

func deviceParams(device, ip string) ([]byte, error) {
	p := L_OpenDeviceParams.New()
	d := strings.ToLower(strings.TrimSpace(device))
	switch {
	case d == "" || d == "usb":
		L_OpenDeviceParams.Put16(p, "deviceType", DEV_USB)
	case d == "eth" || d == "ethernet":
		addr := net.ParseIP(ip).To4()
		if addr == nil {
			return nil, fmt.Errorf("ethernet needs -ip a.b.c.d")
		}
		L_OpenDeviceParams.Put16(p, "deviceType", DEV_ETH)
		// The driver expects the address as 0xAABBCCDD for AA.BB.CC.DD.
		L_OpenDeviceParams.Put32(p, "ipAddress", uint32(addr[0])<<24|uint32(addr[1])<<16|uint32(addr[2])<<8|uint32(addr[3]))
	case strings.HasPrefix(d, "usb"):
		n, err := strconv.Atoi(d[3:])
		if err != nil || n < 1 || n > 24 {
			return nil, fmt.Errorf("device must be usb, usb1..usb24 or eth")
		}
		L_OpenDeviceParams.Put16(p, "deviceType", uint16(DEV_USB1+n-1))
	default:
		return nil, fmt.Errorf("device must be usb, usb1..usb24 or eth")
	}
	return p, nil
}

func (c *Camera) OpenDevice(device, ip string) error {
	p, err := deviceParams(device, ip)
	if err != nil {
		return err
	}
	if err := c.cmd(CC_OPEN_DEVICE, p, nil); err != nil {
		return err
	}
	c.deviceOpen = true
	return nil
}

func (c *Camera) EstablishLink() (uint16, error) {
	p := L_EstablishLinkParams.New()
	r := L_EstablishLinkResults.New()
	if err := c.cmd(CC_ESTABLISH_LINK, p, r); err != nil {
		return 0, err
	}
	c.linked = true
	c.CameraType = L_EstablishLinkResults.U16(r, "cameraType")
	return c.CameraType, nil
}

func (c *Camera) CloseDevice() error {
	if !c.deviceOpen {
		return nil
	}
	c.deviceOpen, c.linked = false, false
	return c.cmd(CC_CLOSE_DEVICE, nil, nil)
}

func (c *Camera) CloseDriver() error {
	if !c.driverOpen {
		return nil
	}
	c.driverOpen = false
	return c.cmd(CC_CLOSE_DRIVER, nil, nil)
}

func (c *Camera) Shutdown() {
	_ = c.CloseDevice()
	_ = c.CloseDriver()
	_ = c.t.Close()
}

// --- information ---

type DriverInfo struct {
	Request    int
	Version    uint16
	Name       string
	MaxRequest uint16
}

func (d DriverInfo) VersionString() string {
	// BCD encoded XX.XX
	return fmt.Sprintf("%x.%02x", d.Version>>8, d.Version&0xFF)
}

func (c *Camera) DriverInfo(req uint16) (DriverInfo, error) {
	p := L_GetDriverInfoParams.New()
	L_GetDriverInfoParams.Put16(p, "request", req)
	r := L_GetDriverInfoResults0.New()
	if err := c.cmd(CC_GET_DRIVER_INFO, p, r); err != nil {
		return DriverInfo{}, err
	}
	return DriverInfo{Request: int(req), Version: L_GetDriverInfoResults0.U16(r, "version"),
		Name: L_GetDriverInfoResults0.Str(r, "name", 64), MaxRequest: L_GetDriverInfoResults0.U16(r, "maxRequest")}, nil
}

type ReadoutMode struct {
	Mode           uint16
	Width          int
	Height         int
	GainEPerADU    float64
	PixelW, PixelH float64 // microns
}

type CCDInfo struct {
	Firmware   uint16
	CameraType uint16
	Name       string
	Modes      []ReadoutMode
}

func (i *CCDInfo) FirmwareString() string {
	return fmt.Sprintf("%x.%02x", i.Firmware>>8, i.Firmware&0xFF)
}

func (i *CCDInfo) Mode(m uint16) (ReadoutMode, bool) {
	for _, x := range i.Modes {
		if x.Mode == m {
			return x, true
		}
	}
	return ReadoutMode{}, false
}

func (c *Camera) CCDInfo(req uint16) (*CCDInfo, error) {
	p := L_GetCCDInfoParams.New()
	L_GetCCDInfoParams.Put16(p, "request", req)
	r := L_GetCCDInfoResults0.New()
	if err := c.cmd(CC_GET_CCD_INFO, p, r); err != nil {
		return nil, err
	}
	L := L_GetCCDInfoResults0
	info := &CCDInfo{Firmware: L.U16(r, "firmwareVersion"), CameraType: L.U16(r, "cameraType"), Name: L.Str(r, "name", 64)}
	n := int(L.U16(r, "readoutModes"))
	if n > 20 {
		n = 20
	}
	base := L.f("readoutInfo").off
	for k := 0; k < n; k++ {
		e := r[base+k*L_READOUT_INFO.size:]
		info.Modes = append(info.Modes, ReadoutMode{
			Mode:        L_READOUT_INFO.U16(e, "mode"),
			Width:       int(L_READOUT_INFO.U16(e, "width")),
			Height:      int(L_READOUT_INFO.U16(e, "height")),
			GainEPerADU: bcd(uint32(L_READOUT_INFO.U16(e, "gain"))),
			PixelW:      bcd(L_READOUT_INFO.U32(e, "pixel_width")),
			PixelH:      bcd(L_READOUT_INFO.U32(e, "pixel_height")),
		})
	}
	if req == 0 {
		c.Info = info
	}
	return info, nil
}

type CCDInfoExt struct {
	BadColumns []uint16
	ABG        uint16
	Serial     string
}

func (c *Camera) CCDInfoExtended() (CCDInfoExt, error) {
	p := L_GetCCDInfoParams.New()
	L_GetCCDInfoParams.Put16(p, "request", 2)
	r := L_GetCCDInfoResults2.New()
	if err := c.cmd(CC_GET_CCD_INFO, p, r); err != nil {
		return CCDInfoExt{}, err
	}
	L := L_GetCCDInfoResults2
	x := CCDInfoExt{ABG: L.U16(r, "imagingABG"), Serial: L.Str(r, "serialNumber", 10)}
	n := int(L.U16(r, "badColumns"))
	for k := 0; k < n && k < 4; k++ {
		x.BadColumns = append(x.BadColumns, le.Uint16(r[L.f("columns").off+2*k:]))
	}
	return x, nil
}

func (c *Camera) CCDCapabilities() (uint16, uint16, error) {
	p := L_GetCCDInfoParams.New()
	L_GetCCDInfoParams.Put16(p, "request", 4)
	r := L_GetCCDInfoResults4.New()
	if err := c.cmd(CC_GET_CCD_INFO, p, r); err != nil {
		return 0, 0, err
	}
	return L_GetCCDInfoResults4.U16(r, "capabilitiesBits"), L_GetCCDInfoResults4.U16(r, "dumpExtra"), nil
}

func (c *Camera) CCDInfo6() (uint32, uint32, uint32, error) {
	p := L_GetCCDInfoParams.New()
	L_GetCCDInfoParams.Put16(p, "request", 6)
	r := L_GetCCDInfoResults6.New()
	if err := c.cmd(CC_GET_CCD_INFO, p, r); err != nil {
		return 0, 0, 0, err
	}
	L := L_GetCCDInfoResults6
	return L.U32(r, "cameraBits"), L.U32(r, "ccdBits"), L.U32(r, "extraBits"), nil
}

type TempStatus struct {
	Valid          bool
	Advanced       bool
	CoolingEnabled uint16
	FanEnabled     uint16
	Setpoint       float64
	CCDTemp        float64
	AmbientTemp    float64
	HeatsinkTemp   float64
	CoolerPower    float64 // percent
	FanPower       float64
	FanSpeed       float64
	Raw            string
}

func (t TempStatus) String() string {
	if !t.Valid {
		return "unavailable"
	}
	if t.Advanced {
		return fmt.Sprintf("CCD %.2f C, setpoint %.2f C, cooling %s (%.0f%%), heatsink %.2f C, ambient %.2f C, fan %d (%.0f%%, %.0f rpm)",
			t.CCDTemp, t.Setpoint, onoff(t.CoolingEnabled&1 != 0), t.CoolerPower, t.HeatsinkTemp, t.AmbientTemp,
			t.FanEnabled, t.FanPower, t.FanSpeed)
	}
	return t.Raw
}

func onoff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func (c *Camera) Temperature() TempStatus {
	p := L_QueryTemperatureStatusParams.New()
	L_QueryTemperatureStatusParams.Put16(p, "request", TEMP_STATUS_ADVANCED2)
	r := L_QueryTemperatureStatusResults2.New()
	if err := c.cmd(CC_QUERY_TEMPERATURE_STATUS, p, r); err == nil {
		L := L_QueryTemperatureStatusResults2
		return TempStatus{Valid: true, Advanced: true,
			CoolingEnabled: L.U16(r, "coolingEnabled"), FanEnabled: L.U16(r, "fanEnabled"),
			Setpoint: L.F64(r, "ccdSetpoint"), CCDTemp: L.F64(r, "imagingCCDTemperature"),
			AmbientTemp: L.F64(r, "ambientTemperature"), HeatsinkTemp: L.F64(r, "heatsinkTemperature"),
			CoolerPower: L.F64(r, "imagingCCDPower"), FanPower: L.F64(r, "fanPower"), FanSpeed: L.F64(r, "fanSpeed")}
	}
	L_QueryTemperatureStatusParams.Put16(p, "request", TEMP_STATUS_STANDARD)
	r = L_QueryTemperatureStatusResults.New()
	if err := c.cmd(CC_QUERY_TEMPERATURE_STATUS, p, r); err == nil {
		L := L_QueryTemperatureStatusResults
		return TempStatus{Valid: true, Raw: fmt.Sprintf("enabled=%d setpoint=%d power=%d/255 ccdThermistor=%d ambientThermistor=%d (raw A/D units)",
			L.U16(r, "enabled"), L.U16(r, "ccdSetpoint"), L.U16(r, "power"), L.U16(r, "ccdThermistor"), L.U16(r, "ambientThermistor"))}
	}
	return TempStatus{}
}

type LinkStatus struct {
	Linked     bool
	CameraType uint16
	ComTotal   uint32
	ComFailed  uint32
}

func (c *Camera) LinkStatus() (LinkStatus, error) {
	r := L_GetLinkStatusResults.New()
	if err := c.cmd(CC_GET_LINK_STATUS, nil, r); err != nil {
		return LinkStatus{}, err
	}
	L := L_GetLinkStatusResults
	return LinkStatus{Linked: L.U16(r, "linkEstablished") != 0, CameraType: L.U16(r, "cameraType"),
		ComTotal: L.U32(r, "comTotal"), ComFailed: L.U32(r, "comFailed")}, nil
}

func (c *Camera) GetDCP(i int) (uint32, error) {
	p := L_GetDriverControlParams.New()
	L_GetDriverControlParams.Put16(p, "controlParameter", uint16(i))
	r := L_GetDriverControlResults.New()
	if err := c.cmd(CC_GET_DRIVER_CONTROL, p, r); err != nil {
		return 0, err
	}
	return L_GetDriverControlResults.U32(r, "controlValue"), nil
}

func (c *Camera) SetDCP(i int, v uint32) error {
	p := L_SetDriverControlParams.New()
	L_SetDriverControlParams.Put16(p, "controlParameter", uint16(i))
	L_SetDriverControlParams.Put32(p, "controlValue", v)
	return c.cmd(CC_SET_DRIVER_CONTROL, p, nil)
}

type CustomerOptions struct {
	Overscan, WindowHeater, Preflash, VddNormallyOff uint16
}

func (o CustomerOptions) String() string {
	return fmt.Sprintf("overscanRegions=%d windowHeater=%d preflashCcd=%d vddNormallyOff=%d",
		o.Overscan, o.WindowHeater, o.Preflash, o.VddNormallyOff)
}

func (c *Camera) GetCustomerOptions() (CustomerOptions, error) {
	L := L_CustomerOptionsParams
	p := L.New() // bSetCustomerOptions = FALSE -> read
	r := L.New()
	if err := c.cmd(CC_CUSTOMER_OPTIONS, p, r); err != nil {
		return CustomerOptions{}, err
	}
	return CustomerOptions{Overscan: L.U16(r, "bOverscanRegions"), WindowHeater: L.U16(r, "bWindowHeater"),
		Preflash: L.U16(r, "bPreflashCcd"), VddNormallyOff: L.U16(r, "bVddNormallyOff")}, nil
}

func (c *Camera) SetCustomerOptions(o CustomerOptions) error {
	L := L_CustomerOptionsParams
	p := L.New()
	L.Put16(p, "bSetCustomerOptions", 1)
	L.Put16(p, "bOverscanRegions", o.Overscan)
	L.Put16(p, "bWindowHeater", o.WindowHeater)
	L.Put16(p, "bPreflashCcd", o.Preflash)
	L.Put16(p, "bVddNormallyOff", o.VddNormallyOff)
	r := L.New()
	return c.cmd(CC_CUSTOMER_OPTIONS, p, r)
}

func (c *Camera) RBIParams() (uint16, uint16, error) {
	r := L_GetRBIPreflashResults.New()
	if err := c.cmd(CC_GET_RBI_PARAMETERS, nil, r); err != nil {
		return 0, 0, err
	}
	return L_GetRBIPreflashResults.U16(r, "darkFrameLength"), L_GetRBIPreflashResults.U16(r, "flushCount"), nil
}

func (c *Camera) ColumnEEPROM() ([]uint16, uint16, error) {
	L := L_ColumnEEPROMParams
	p := L.New()
	r := L.New()
	if err := c.cmd(CC_COLUMN_EEPROM, p, r); err != nil {
		return nil, 0, err
	}
	var cols []uint16
	for k := 0; k < 7; k++ {
		cols = append(cols, le.Uint16(r[L.f("columns").off+2*k:]))
	}
	return cols, L.U16(r, "flags"), nil
}

// ReadOffset asks the camera to measure its own bias (offset) level.
func (c *Camera) ReadOffset2() (uint16, float64, error) {
	p := L_ReadOffsetParams.New()
	r := L_ReadOffsetResults2.New()
	if err := c.cmd(CC_READ_OFFSET2, p, r); err != nil {
		return 0, 0, err
	}
	return L_ReadOffsetResults2.U16(r, "offset"), L_ReadOffsetResults2.F64(r, "rms"), nil
}

func (c *Camera) ReadOffset() (uint16, error) {
	p := L_ReadOffsetParams.New()
	r := L_ReadOffsetResults.New()
	if err := c.cmd(CC_READ_OFFSET, p, r); err != nil {
		return 0, err
	}
	return L_ReadOffsetResults.U16(r, "offset"), nil
}

// PowerLow reads the "input supply below ~10 V" status bit (documented for STL; may work on STX/STXL).
func (c *Camera) PowerLow() (bool, error) {
	p := L_BitIOParams.New()
	L_BitIOParams.Put16(p, "bitOperation", BITIO_READ)
	L_BitIOParams.Put16(p, "bitName", BITI_PS_LOW)
	r := L_BitIOResults.New()
	if err := c.cmd(CC_BIT_IO, p, r); err != nil {
		return false, err
	}
	return L_BitIOResults.U16(r, "bitIsSet") != 0, nil
}

func (c *Camera) FeatureSupported(ffr uint32) (bool, error) {
	p := L_QueryFeatureSupportedParams.New()
	L_QueryFeatureSupportedParams.Put32(p, "ffr", ffr)
	r := L_QueryFeatureSupportedResults.New()
	if err := c.cmd(CC_QUERY_FEATURE_SUPPORTED, p, r); err != nil {
		return false, err
	}
	return L_QueryFeatureSupportedResults.U16(r, "result") != 0, nil
}

func (c *Camera) ReadoutInProgress() (bool, error) {
	r := L_GetReadoutInProgressResults.New()
	if err := c.cmd(CC_READOUT_IN_PROGRESS, nil, r); err != nil {
		return false, err
	}
	return L_GetReadoutInProgressResults.U16(r, "RIP") != 0, nil
}

type USBCam struct {
	Found  bool
	Type   uint16
	Name   string
	Serial string
}

func (c *Camera) QueryUSB() ([]USBCam, error) {
	L := L_QueryUSBResults3
	r := L.New()
	n := 24
	if err := c.cmd(CC_QUERY_USB3, nil, r); err != nil {
		L = L_QueryUSBResults
		r = L.New()
		n = 4
		if err2 := c.cmd(CC_QUERY_USB, nil, r); err2 != nil {
			return nil, err2
		}
	}
	var out []USBCam
	base := L.f("usbInfo").off
	for k := 0; k < n; k++ {
		e := r[base+k*L_QUERY_USB_INFO.size:]
		if L_QUERY_USB_INFO.U16(e, "cameraFound") == 0 {
			continue
		}
		out = append(out, USBCam{Found: true, Type: L_QUERY_USB_INFO.U16(e, "cameraType"),
			Name: L_QUERY_USB_INFO.Str(e, "name", 64), Serial: L_QUERY_USB_INFO.Str(e, "serialNumber", 10)})
	}
	return out, nil
}

func (c *Camera) MiscControl(fan, shutter, led uint16) error {
	p := L_MiscellaneousControlParams.New()
	L_MiscellaneousControlParams.Put16(p, "fanEnable", fan)
	L_MiscellaneousControlParams.Put16(p, "shutterCommand", shutter)
	L_MiscellaneousControlParams.Put16(p, "ledState", led)
	return c.cmd(CC_MISCELLANEOUS_CONTROL, p, nil)
}

// DebugLog enables the SBIG driver's own debug log file (flags 0 disables).
func (c *Camera) DebugLog(path string, cc, mc, fce, io uint16) error {
	L := L_DebugLogParams
	p := L.New()
	L.Put16(p, "ccFlags", cc)
	L.Put16(p, "mcFlags", mc)
	L.Put16(p, "fceFlags", fce)
	L.Put16(p, "ioFlags", io)
	L.PutStr(p, "logFilePathName", 1024, path)
	return c.cmd(CC_DEBUG_LOG, p, nil)
}

// --- exposures ---

type ExposureSpec struct {
	Label     string
	Seconds   float64
	Dark      bool // closed shutter
	Mode      uint16
	Top, Left int           // binned pixels
	Width     int           // 0 = full
	Height    int           // 0 = full
	CCDFlags  uint16        // START_SKIP_VDD etc. OR-ed into the ccd field
	ExpFlags  uint32        // EXP_* OR-ed into exposureTime
	LineDelay time.Duration // optional pause between ReadoutLine calls (timing test)
}

func (s ExposureSpec) Describe() string {
	sh := "dark"
	if !s.Dark {
		sh = "light"
	}
	extra := ""
	if s.CCDFlags&START_SKIP_VDD != 0 {
		extra += " START_SKIP_VDD"
	}
	if s.CCDFlags&START_MOTOR_ALWAYS_ON != 0 {
		extra += " MOTOR_ALWAYS_ON"
	}
	if s.ExpFlags != 0 {
		extra += fmt.Sprintf(" expflags=0x%08x", s.ExpFlags)
	}
	if s.LineDelay > 0 {
		extra += fmt.Sprintf(" linedelay=%v", s.LineDelay)
	}
	sub := "full"
	if s.Width > 0 || s.Height > 0 || s.Top > 0 || s.Left > 0 {
		sub = fmt.Sprintf("sub top=%d left=%d h=%d w=%d", s.Top, s.Left, s.Height, s.Width)
	}
	return fmt.Sprintf("%.3fs %s %s %s%s", s.Seconds, sh, readoutModeName(s.Mode), sub, extra)
}

type Frame struct {
	Spec          ExposureSpec
	W, H          int
	Pix           []uint16
	Start         time.Time
	IntegrDone    time.Duration // start -> integration complete reported
	ReadoutTime   time.Duration
	LineTimes     []time.Duration
	TempBefore    TempStatus
	TempAfter     TempStatus
	LinkBefore    LinkStatus
	LinkAfter     LinkStatus
	PowerLowAfter string
	Errors        []string
	DCPs          map[string]uint32
}

var errAborted = errors.New("aborted by user")

// Expose runs one complete exposure + readout.
func (c *Camera) Expose(s ExposureSpec, abort func() bool) (*Frame, error) {
	if c.Info == nil {
		if _, err := c.CCDInfo(0); err != nil {
			return nil, err
		}
	}
	m, ok := c.Info.Mode(s.Mode)
	if !ok {
		return nil, fmt.Errorf("camera does not report readout mode %s", readoutModeName(s.Mode))
	}
	w, h := s.Width, s.Height
	if w <= 0 {
		w = m.Width - s.Left
	}
	if h <= 0 {
		h = m.Height - s.Top
	}
	if s.Left+w > m.Width || s.Top+h > m.Height || w <= 0 || h <= 0 {
		return nil, fmt.Errorf("subframe outside the %dx%d %s frame", m.Width, m.Height, readoutModeName(s.Mode))
	}
	f := &Frame{Spec: s, W: w, H: h}
	f.TempBefore = c.Temperature()
	f.LinkBefore, _ = c.LinkStatus()

	ticks := uint32(math.Round(s.Seconds * 100))
	if ticks > EXP_TIME_MASK {
		ticks = EXP_TIME_MASK
	}
	L := L_StartExposureParams2
	p := L.New()
	L.Put16(p, "ccd", CCD_IMAGING|s.CCDFlags)
	L.Put32(p, "exposureTime", ticks|s.ExpFlags)
	L.Put16(p, "abgState", ABG_LOW7)
	sh := uint16(SC_OPEN_SHUTTER)
	if s.Dark {
		sh = SC_CLOSE_SHUTTER
	}
	L.Put16(p, "openShutter", sh)
	L.Put16(p, "readoutMode", s.Mode)
	L.Put16(p, "top", uint16(s.Top))
	L.Put16(p, "left", uint16(s.Left))
	L.Put16(p, "height", uint16(h))
	L.Put16(p, "width", uint16(w))
	f.Start = time.Now()
	if err := c.cmd(CC_START_EXPOSURE2, p, nil); err != nil {
		return nil, err
	}
	// Poll for the end of integration.
	q := L_QueryCommandStatusParams.New()
	L_QueryCommandStatusParams.Put16(q, "command", CC_START_EXPOSURE2)
	qr := L_QueryCommandStatusResults.New()
	deadline := f.Start.Add(time.Duration(s.Seconds*float64(time.Second)) + 90*time.Second)
	for {
		if abort != nil && abort() {
			c.endExposure(ABORT_DONT_END)
			return nil, errAborted
		}
		if err := c.cmd(CC_QUERY_COMMAND_STATUS, q, qr); err != nil {
			c.endExposure(ABORT_DONT_END)
			return nil, err
		}
		if L_QueryCommandStatusResults.U16(qr, "status")&3 == CS_INTEGRATION_COMPLETE {
			break
		}
		if time.Now().After(deadline) {
			c.endExposure(ABORT_DONT_END)
			return nil, fmt.Errorf("timeout waiting for integration to complete (status=0x%04x)", L_QueryCommandStatusResults.U16(qr, "status"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.IntegrDone = time.Since(f.Start)
	if err := c.endExposure(0); err != nil {
		f.Errors = append(f.Errors, err.Error())
	}
	// Readout.
	sr := L_StartReadoutParams.New()
	L_StartReadoutParams.Put16(sr, "ccd", CCD_IMAGING)
	L_StartReadoutParams.Put16(sr, "readoutMode", s.Mode)
	L_StartReadoutParams.Put16(sr, "top", uint16(s.Top))
	L_StartReadoutParams.Put16(sr, "left", uint16(s.Left))
	L_StartReadoutParams.Put16(sr, "height", uint16(h))
	L_StartReadoutParams.Put16(sr, "width", uint16(w))
	t0 := time.Now()
	if err := c.cmd(CC_START_READOUT, sr, nil); err != nil {
		return nil, err
	}
	rl := L_ReadoutLineParams.New()
	L_ReadoutLineParams.Put16(rl, "ccd", CCD_IMAGING)
	L_ReadoutLineParams.Put16(rl, "readoutMode", s.Mode)
	L_ReadoutLineParams.Put16(rl, "pixelStart", uint16(s.Left))
	L_ReadoutLineParams.Put16(rl, "pixelLength", uint16(w))
	line := make([]byte, 2*w)
	f.Pix = make([]uint16, w*h)
	f.LineTimes = make([]time.Duration, h)
	var readErr error
	for y := 0; y < h; y++ {
		if abort != nil && abort() {
			readErr = errAborted
			break
		}
		lt := time.Now()
		if err := c.cmd(CC_READOUT_LINE, rl, line); err != nil {
			readErr = fmt.Errorf("line %d: %w", y, err)
			break
		}
		f.LineTimes[y] = time.Since(lt)
		row := f.Pix[y*w : (y+1)*w]
		for x := range row {
			row[x] = le.Uint16(line[2*x:])
		}
		if s.LineDelay > 0 {
			time.Sleep(s.LineDelay)
		}
	}
	er := L_EndReadoutParams.New()
	L_EndReadoutParams.Put16(er, "ccd", CCD_IMAGING)
	if err := c.cmd(CC_END_READOUT, er, nil); err != nil {
		f.Errors = append(f.Errors, err.Error())
	}
	f.ReadoutTime = time.Since(t0)
	f.TempAfter = c.Temperature()
	f.LinkAfter, _ = c.LinkStatus()
	if pl, err := c.PowerLow(); err == nil {
		f.PowerLowAfter = fmt.Sprintf("%v", pl)
	} else {
		f.PowerLowAfter = "n/a"
	}
	if readErr != nil {
		return f, readErr
	}
	return f, nil
}

func (c *Camera) endExposure(flags uint16) error {
	p := L_EndExposureParams.New()
	L_EndExposureParams.Put16(p, "ccd", CCD_IMAGING|flags)
	return c.cmd(CC_END_EXPOSURE, p, nil)
}
