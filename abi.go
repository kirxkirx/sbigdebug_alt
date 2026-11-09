package main

// Byte-exact layouts of the SBIG Universal Driver (SBIGUDrv.dll) parameter and
// result structures, as compiled for Windows by the vendor header sbigudrv.h
// (#pragma pack(8), so natural alignment; "unsigned long" is 32 bit on both
// Win32 and Win64; enums are 32 bit). The same table drives the marshalling code
// and the ABI self-check (abi_test.go generates C _Static_assert()s that CI
// compiles against the real header for both i686 and x86_64 Windows targets).

import (
	"encoding/binary"
	"fmt"
	"math"
)

type abiField struct {
	name string
	off  int
	size int // 1 (char array element), 2, 4 or 8
}

type abiLayout struct {
	name   string // C typedef name
	size   int    // sizeof()
	fields map[string]abiField
	order  []string
}

var abiLayouts []*abiLayout

// mk builds a layout from (name, offset, size) triples.
func mk(name string, size int, spec ...interface{}) *abiLayout {
	l := &abiLayout{name: name, size: size, fields: map[string]abiField{}}
	for i := 0; i+2 < len(spec); i += 3 {
		f := abiField{name: spec[i].(string), off: spec[i+1].(int), size: spec[i+2].(int)}
		l.fields[f.name] = f
		l.order = append(l.order, f.name)
	}
	abiLayouts = append(abiLayouts, l)
	return l
}

var le = binary.LittleEndian

func (l *abiLayout) New() []byte { return make([]byte, l.size) }

func (l *abiLayout) f(name string) abiField {
	f, ok := l.fields[name]
	if !ok {
		panic(fmt.Sprintf("abi: %s has no field %s", l.name, name))
	}
	return f
}

func (l *abiLayout) Put16(b []byte, name string, v uint16) { le.PutUint16(b[l.f(name).off:], v) }
func (l *abiLayout) Put32(b []byte, name string, v uint32) { le.PutUint32(b[l.f(name).off:], v) }
func (l *abiLayout) PutF64(b []byte, name string, v float64) {
	le.PutUint64(b[l.f(name).off:], math.Float64bits(v))
}
func (l *abiLayout) U16(b []byte, name string) uint16 { return le.Uint16(b[l.f(name).off:]) }
func (l *abiLayout) I16(b []byte, name string) int16  { return int16(le.Uint16(b[l.f(name).off:])) }
func (l *abiLayout) U32(b []byte, name string) uint32 { return le.Uint32(b[l.f(name).off:]) }
func (l *abiLayout) F64(b []byte, name string) float64 {
	return math.Float64frombits(le.Uint64(b[l.f(name).off:]))
}
func (l *abiLayout) Str(b []byte, name string, n int) string {
	off := l.f(name).off
	s := b[off : off+n]
	for i, c := range s {
		if c == 0 {
			s = s[:i]
			break
		}
	}
	return string(s)
}
func (l *abiLayout) PutStr(b []byte, name string, n int, s string) {
	off := l.f(name).off
	copy(b[off:off+n-1], s)
}

// --- Command parameter / result structures (Windows layout) ---

var (
	L_StartExposureParams2 = mk("StartExposureParams2", 24,
		"ccd", 0, 2, "exposureTime", 4, 4, "abgState", 8, 2, "openShutter", 10, 2,
		"readoutMode", 12, 2, "top", 14, 2, "left", 16, 2, "height", 18, 2, "width", 20, 2)
	L_EndExposureParams  = mk("EndExposureParams", 2, "ccd", 0, 2)
	L_StartReadoutParams = mk("StartReadoutParams", 12,
		"ccd", 0, 2, "readoutMode", 2, 2, "top", 4, 2, "left", 6, 2, "height", 8, 2, "width", 10, 2)
	L_ReadoutLineParams = mk("ReadoutLineParams", 8,
		"ccd", 0, 2, "readoutMode", 2, 2, "pixelStart", 4, 2, "pixelLength", 6, 2)
	L_DumpLinesParams  = mk("DumpLinesParams", 6, "ccd", 0, 2, "readoutMode", 2, 2, "lineLength", 4, 2)
	L_EndReadoutParams = mk("EndReadoutParams", 2, "ccd", 0, 2)

	L_SetTemperatureRegulationParams2 = mk("SetTemperatureRegulationParams2", 16,
		"regulation", 0, 2, "ccdSetpoint", 8, 8)
	L_QueryTemperatureStatusParams  = mk("QueryTemperatureStatusParams", 2, "request", 0, 2)
	L_QueryTemperatureStatusResults = mk("QueryTemperatureStatusResults", 10,
		"enabled", 0, 2, "ccdSetpoint", 2, 2, "power", 4, 2, "ccdThermistor", 6, 2, "ambientThermistor", 8, 2)
	L_QueryTemperatureStatusResults2 = mk("QueryTemperatureStatusResults2", 104,
		"coolingEnabled", 0, 2, "fanEnabled", 2, 2, "ccdSetpoint", 8, 8, "imagingCCDTemperature", 16, 8,
		"trackingCCDTemperature", 24, 8, "externalTrackingCCDTemperature", 32, 8, "ambientTemperature", 40, 8,
		"imagingCCDPower", 48, 8, "trackingCCDPower", 56, 8, "externalTrackingCCDPower", 64, 8,
		"heatsinkTemperature", 72, 8, "fanPower", 80, 8, "fanSpeed", 88, 8, "trackingCCDSetpoint", 96, 8)

	L_EstablishLinkParams   = mk("EstablishLinkParams", 2, "sbigUseOnly", 0, 2)
	L_EstablishLinkResults  = mk("EstablishLinkResults", 2, "cameraType", 0, 2)
	L_GetDriverInfoParams   = mk("GetDriverInfoParams", 2, "request", 0, 2)
	L_GetDriverInfoResults0 = mk("GetDriverInfoResults0", 68,
		"version", 0, 2, "name", 2, 1, "maxRequest", 66, 2)

	L_GetCCDInfoParams   = mk("GetCCDInfoParams", 2, "request", 0, 2)
	L_GetCCDInfoResults0 = mk("GetCCDInfoResults0", 392,
		"firmwareVersion", 0, 2, "cameraType", 2, 2, "name", 4, 1, "readoutModes", 68, 2, "readoutInfo", 72, 2)
	L_READOUT_INFO = mk("READOUT_INFO", 16,
		"mode", 0, 2, "width", 2, 2, "height", 4, 2, "gain", 6, 2, "pixel_width", 8, 4, "pixel_height", 12, 4)
	L_GetCCDInfoResults2 = mk("GetCCDInfoResults2", 22,
		"badColumns", 0, 2, "columns", 2, 2, "imagingABG", 10, 2, "serialNumber", 12, 1)
	L_GetCCDInfoResults3 = mk("GetCCDInfoResults3", 4, "adSize", 0, 2, "filterType", 2, 2)
	L_GetCCDInfoResults4 = mk("GetCCDInfoResults4", 4, "capabilitiesBits", 0, 2, "dumpExtra", 2, 2)
	L_GetCCDInfoResults6 = mk("GetCCDInfoResults6", 12, "cameraBits", 0, 4, "ccdBits", 4, 4, "extraBits", 8, 4)

	L_QueryCommandStatusParams   = mk("QueryCommandStatusParams", 2, "command", 0, 2)
	L_QueryCommandStatusResults  = mk("QueryCommandStatusResults", 2, "status", 0, 2)
	L_QueryCommandStatusResults2 = mk("QueryCommandStatusResults2", 4, "status", 0, 2, "info", 2, 2)
	L_MiscellaneousControlParams = mk("MiscellaneousControlParams", 6,
		"fanEnable", 0, 2, "shutterCommand", 2, 2, "ledState", 4, 2)
	L_ReadOffsetParams     = mk("ReadOffsetParams", 2, "ccd", 0, 2)
	L_ReadOffsetResults    = mk("ReadOffsetResults", 2, "offset", 0, 2)
	L_ReadOffsetResults2   = mk("ReadOffsetResults2", 16, "offset", 0, 2, "rms", 8, 8)
	L_OpenDeviceParams     = mk("OpenDeviceParams", 8, "deviceType", 0, 2, "lptBaseAddress", 2, 2, "ipAddress", 4, 4)
	L_GetLinkStatusResults = mk("GetLinkStatusResults", 16,
		"linkEstablished", 0, 2, "baseAddress", 2, 2, "cameraType", 4, 2, "comTotal", 8, 4, "comFailed", 12, 4)
	L_GetErrorStringParams    = mk("GetErrorStringParams", 2, "errorNo", 0, 2)
	L_GetErrorStringResults   = mk("GetErrorStringResults", 64, "errorString", 0, 1)
	L_SetDriverControlParams  = mk("SetDriverControlParams", 8, "controlParameter", 0, 2, "controlValue", 4, 4)
	L_GetDriverControlParams  = mk("GetDriverControlParams", 2, "controlParameter", 0, 2)
	L_GetDriverControlResults = mk("GetDriverControlResults", 4, "controlValue", 0, 4)
	L_USBADControlParams      = mk("USBADControlParams", 4, "command", 0, 2, "data", 2, 2)
	L_QUERY_USB_INFO          = mk("QUERY_USB_INFO", 78,
		"cameraFound", 0, 2, "cameraType", 2, 2, "name", 4, 1, "serialNumber", 68, 1)
	L_QueryUSBResults       = mk("QueryUSBResults", 314, "camerasFound", 0, 2, "usbInfo", 2, 2)
	L_QueryUSBResults2      = mk("QueryUSBResults2", 626, "camerasFound", 0, 2, "usbInfo", 2, 2)
	L_QueryUSBResults3      = mk("QueryUSBResults3", 1874, "camerasFound", 0, 2, "usbInfo", 2, 2)
	L_BitIOParams           = mk("BitIOParams", 6, "bitOperation", 0, 2, "bitName", 2, 2, "setBit", 4, 2)
	L_BitIOResults          = mk("BitIOResults", 2, "bitIsSet", 0, 2)
	L_ColumnEEPROMParams    = mk("ColumnEEPROMParams", 18, "writeData", 0, 2, "columns", 2, 2, "flags", 16, 2)
	L_CustomerOptionsParams = mk("CustomerOptionsParams", 10,
		"bSetCustomerOptions", 0, 2, "bOverscanRegions", 2, 2, "bWindowHeater", 4, 2,
		"bPreflashCcd", 6, 2, "bVddNormallyOff", 8, 2)
	L_GetReadoutInProgressResults  = mk("GetReadoutInProgressResults", 2, "RIP", 0, 2)
	L_GetRBIPreflashResults        = mk("GetRBIPreflashResults", 4, "darkFrameLength", 0, 2, "flushCount", 2, 2)
	L_QueryFeatureSupportedParams  = mk("QueryFeatureSupportedParams", 4, "ffr", 0, 4)
	L_QueryFeatureSupportedResults = mk("QueryFeatureSupportedResults", 2, "result", 0, 2)
	L_DebugLogParams               = mk("DebugLogParams", 1032,
		"ccFlags", 0, 2, "mcFlags", 2, 2, "fceFlags", 4, 2, "ioFlags", 6, 2, "logFilePathName", 8, 1)
)

// --- Command numbers (PAR_COMMAND) ---
const (
	CC_START_EXPOSURE              = 1
	CC_END_EXPOSURE                = 2
	CC_READOUT_LINE                = 3
	CC_DUMP_LINES                  = 4
	CC_QUERY_TEMPERATURE_STATUS    = 6
	CC_ESTABLISH_LINK              = 9
	CC_GET_DRIVER_INFO             = 10
	CC_GET_CCD_INFO                = 11
	CC_QUERY_COMMAND_STATUS        = 12
	CC_MISCELLANEOUS_CONTROL       = 13
	CC_READ_OFFSET                 = 16
	CC_OPEN_DRIVER                 = 17
	CC_CLOSE_DRIVER                = 18
	CC_END_READOUT                 = 25
	CC_OPEN_DEVICE                 = 27
	CC_CLOSE_DEVICE                = 28
	CC_GET_LINK_STATUS             = 32
	CC_START_READOUT               = 35
	CC_GET_ERROR_STRING            = 36
	CC_SET_DRIVER_CONTROL          = 37
	CC_GET_DRIVER_CONTROL          = 38
	CC_USB_AD_CONTROL              = 39
	CC_QUERY_USB                   = 40
	CC_BIT_IO                      = 44
	CC_START_EXPOSURE2             = 50
	CC_SET_TEMPERATURE_REGULATION2 = 51
	CC_READ_OFFSET2                = 52
	CC_COLUMN_EEPROM               = 54
	CC_CUSTOMER_OPTIONS            = 55
	CC_DEBUG_LOG                   = 56
	CC_QUERY_USB2                  = 57
	CC_QUERY_USB3                  = 60
	CC_QUERY_COMMAND_STATUS2       = 61
	CC_READOUT_IN_PROGRESS         = 106
	CC_GET_RBI_PARAMETERS          = 107
	CC_SET_RBI_PARAMETERS          = 108
	CC_QUERY_FEATURE_SUPPORTED     = 109
)

var commandNames = map[uint16]string{
	1: "CC_START_EXPOSURE", 2: "CC_END_EXPOSURE", 3: "CC_READOUT_LINE", 4: "CC_DUMP_LINES",
	6: "CC_QUERY_TEMPERATURE_STATUS", 9: "CC_ESTABLISH_LINK", 10: "CC_GET_DRIVER_INFO",
	11: "CC_GET_CCD_INFO", 12: "CC_QUERY_COMMAND_STATUS", 13: "CC_MISCELLANEOUS_CONTROL",
	16: "CC_READ_OFFSET", 17: "CC_OPEN_DRIVER", 18: "CC_CLOSE_DRIVER", 25: "CC_END_READOUT",
	27: "CC_OPEN_DEVICE", 28: "CC_CLOSE_DEVICE", 32: "CC_GET_LINK_STATUS", 35: "CC_START_READOUT",
	36: "CC_GET_ERROR_STRING", 37: "CC_SET_DRIVER_CONTROL", 38: "CC_GET_DRIVER_CONTROL",
	39: "CC_USB_AD_CONTROL", 40: "CC_QUERY_USB", 44: "CC_BIT_IO", 50: "CC_START_EXPOSURE2",
	51: "CC_SET_TEMPERATURE_REGULATION2", 52: "CC_READ_OFFSET2", 54: "CC_COLUMN_EEPROM",
	55: "CC_CUSTOMER_OPTIONS", 56: "CC_DEBUG_LOG", 57: "CC_QUERY_USB2", 60: "CC_QUERY_USB3",
	61: "CC_QUERY_COMMAND_STATUS2", 106: "CC_READOUT_IN_PROGRESS", 107: "CC_GET_RBI_PARAMETERS",
	108: "CC_SET_RBI_PARAMETERS", 109: "CC_QUERY_FEATURE_SUPPORTED",
}

func cmdName(c uint16) string {
	if n, ok := commandNames[c]; ok {
		return n
	}
	return fmt.Sprintf("CC_%d", c)
}

// PAR_ERROR codes.
var errorNames = []string{
	"CE_NO_ERROR", "CE_CAMERA_NOT_FOUND", "CE_EXPOSURE_IN_PROGRESS", "CE_NO_EXPOSURE_IN_PROGRESS",
	"CE_UNKNOWN_COMMAND", "CE_BAD_CAMERA_COMMAND", "CE_BAD_PARAMETER", "CE_TX_TIMEOUT", "CE_RX_TIMEOUT",
	"CE_NAK_RECEIVED", "CE_CAN_RECEIVED", "CE_UNKNOWN_RESPONSE", "CE_BAD_LENGTH", "CE_AD_TIMEOUT",
	"CE_KBD_ESC", "CE_CHECKSUM_ERROR", "CE_EEPROM_ERROR", "CE_SHUTTER_ERROR", "CE_UNKNOWN_CAMERA",
	"CE_DRIVER_NOT_FOUND", "CE_DRIVER_NOT_OPEN", "CE_DRIVER_NOT_CLOSED", "CE_SHARE_ERROR",
	"CE_TCE_NOT_FOUND", "CE_AO_ERROR", "CE_ECP_ERROR", "CE_MEMORY_ERROR", "CE_DEVICE_NOT_FOUND",
	"CE_DEVICE_NOT_OPEN", "CE_DEVICE_NOT_CLOSED", "CE_DEVICE_NOT_IMPLEMENTED", "CE_DEVICE_DISABLED",
	"CE_OS_ERROR", "CE_SOCK_ERROR", "CE_SERVER_NOT_FOUND", "CE_CFW_ERROR", "CE_MF_ERROR",
	"CE_FIRMWARE_ERROR", "CE_DIFF_GUIDER_ERROR", "CE_RIPPLE_CORRECTION_ERROR", "CE_EZUSB_RESET",
	"CE_INCOMPATIBLE_FIRMWARE", "CE_INVALID_HANDLE",
}

const (
	CE_NO_ERROR               = 0
	CE_CAMERA_NOT_FOUND       = 1
	CE_UNKNOWN_COMMAND        = 4
	CE_BAD_CAMERA_COMMAND     = 5
	CE_BAD_PARAMETER          = 6
	CE_DRIVER_NOT_CLOSED      = 21
	CE_SHARE_ERROR            = 22
	CE_DEVICE_NOT_FOUND       = 27
	CE_DEVICE_NOT_CLOSED      = 29
	CE_DEVICE_NOT_IMPLEMENTED = 30
	CE_INCOMPATIBLE_FIRMWARE  = 41
)

func errName(code uint16) string {
	if int(code) < len(errorNames) {
		return errorNames[code]
	}
	return fmt.Sprintf("CE_%d", code)
}

// DRIVER_CONTROL_PARAM.
var dcpNames = []string{
	"DCP_USB_FIFO_ENABLE", "DCP_CALL_JOURNAL_ENABLE", "DCP_IVTOH_RATIO", "DCP_USB_FIFO_SIZE",
	"DCP_USB_DRIVER", "DCP_KAI_RELGAIN", "DCP_USB_PIXEL_DL_ENABLE", "DCP_HIGH_THROUGHPUT",
	"DCP_VDD_OPTIMIZED", "DCP_AUTO_AD_GAIN", "DCP_NO_HCLKS_FOR_INTEGRATION", "DCP_TDI_MODE_ENABLE",
	"DCP_VERT_FLUSH_CONTROL_ENABLE", "DCP_ETHERNET_PIPELINE_ENABLE", "DCP_FAST_LINK",
	"DCP_OVERSCAN_ROWSCOLS", "DCP_PIXEL_PIPELINE_ENABLE", "DCP_COLUMN_REPAIR_ENABLE",
	"DCP_WARM_PIXEL_REPAIR_ENABLE", "DCP_WARM_PIXEL_REPAIR_COUNT", "DCP_TDI_MODE_DRIFT_RATE",
	"DCP_OVERRIDE_AD_GAIN", "DCP_ENABLE_AUTO_OFFSET",
}

const (
	DCP_USB_FIFO_ENABLE           = 0
	DCP_CALL_JOURNAL_ENABLE       = 1
	DCP_USB_FIFO_SIZE             = 3
	DCP_USB_PIXEL_DL_ENABLE       = 6
	DCP_HIGH_THROUGHPUT           = 7
	DCP_VDD_OPTIMIZED             = 8
	DCP_AUTO_AD_GAIN              = 9
	DCP_NO_HCLKS_FOR_INTEGRATION  = 10
	DCP_TDI_MODE_ENABLE           = 11
	DCP_VERT_FLUSH_CONTROL_ENABLE = 12
	DCP_ETHERNET_PIPELINE_ENABLE  = 13
	DCP_FAST_LINK                 = 14
	DCP_OVERSCAN_ROWSCOLS         = 15
	DCP_PIXEL_PIPELINE_ENABLE     = 16
	DCP_COLUMN_REPAIR_ENABLE      = 17
	DCP_WARM_PIXEL_REPAIR_ENABLE  = 18
	DCP_WARM_PIXEL_REPAIR_COUNT   = 19
	DCP_OVERRIDE_AD_GAIN          = 21
	DCP_ENABLE_AUTO_OFFSET        = 22
)

// Documented defaults (SBIGUDrv application note / sbigudrv.h comments); -1 = no documented default.
var dcpDocDefault = map[int]int64{
	DCP_USB_FIFO_ENABLE: 1, DCP_CALL_JOURNAL_ENABLE: 0, 2: 5, DCP_USB_FIFO_SIZE: 16384,
	DCP_USB_PIXEL_DL_ENABLE: 1, DCP_HIGH_THROUGHPUT: 0, DCP_VDD_OPTIMIZED: 1, DCP_AUTO_AD_GAIN: 1,
	DCP_NO_HCLKS_FOR_INTEGRATION: 0, DCP_TDI_MODE_ENABLE: 0, DCP_VERT_FLUSH_CONTROL_ENABLE: 1,
	DCP_ETHERNET_PIPELINE_ENABLE: 0, DCP_FAST_LINK: 0, DCP_COLUMN_REPAIR_ENABLE: 0,
	DCP_WARM_PIXEL_REPAIR_ENABLE: 0,
}

func dcpIndex(name string) int {
	for i, n := range dcpNames {
		if n == name || n == "DCP_"+name {
			return i
		}
	}
	return -1
}

// Camera types (CAMERA_TYPE).
var cameraTypeNames = map[uint16]string{
	4: "ST-7", 5: "ST-8", 6: "ST-5C", 7: "TCE", 8: "ST-237", 9: "ST-K", 10: "ST-9", 11: "STV",
	12: "ST-10", 13: "ST-1000", 14: "ST-2000", 15: "STL", 16: "ST-402", 17: "STX", 18: "ST-4000",
	19: "STT", 20: "ST-i", 21: "STF", 22: "NEXT", 0xFFFF: "NO_CAMERA",
}

func cameraTypeName(t uint16) string {
	if n, ok := cameraTypeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type%d", t)
}

// Readout (binning) modes.
var readoutModeNames = map[uint16]string{
	0: "1x1", 1: "2x2", 2: "3x3", 3: "Nx1", 4: "Nx2", 5: "Nx3", 6: "1x1 off-chip", 7: "2x2 off-chip",
	8: "3x3 off-chip", 9: "9x9", 10: "NxN",
}

func readoutModeName(m uint16) string {
	if n, ok := readoutModeNames[m&0xFF]; ok {
		if m>>8 != 0 {
			return fmt.Sprintf("%s(N=%d)", n, m>>8)
		}
		return n
	}
	return fmt.Sprintf("mode%d", m)
}

const (
	CCD_IMAGING  = 0
	CCD_TRACKING = 1

	SC_LEAVE_SHUTTER      = 0
	SC_OPEN_SHUTTER       = 1
	SC_CLOSE_SHUTTER      = 2
	SC_INITIALIZE_SHUTTER = 3

	ABG_LOW7 = 0

	START_SKIP_VDD        = 0x8000
	START_MOTOR_ALWAYS_ON = 0x4000
	ABORT_DONT_END        = 0x2000
	END_SKIP_DELAY        = 0x8000

	EXP_FAST_READOUT      = 0x08000000
	EXP_DUAL_CHANNEL_MODE = 0x04000000
	EXP_MS_EXPOSURE       = 0x10000000
	EXP_LIGHT_CLEAR       = 0x20000000
	EXP_TIME_MASK         = 0x00FFFFFF

	DEV_USB  = 0x7F00
	DEV_ETH  = 0x7F01
	DEV_USB1 = 0x7F02

	CS_IDLE                 = 0
	CS_IN_PROGRESS          = 1
	CS_INTEGRATING          = 2
	CS_INTEGRATION_COMPLETE = 3

	TEMP_STATUS_STANDARD  = 0
	TEMP_STATUS_ADVANCED2 = 2

	BITIO_READ  = 1
	BITI_PS_LOW = 0
)

// GetCCDInfoResults4 capability bits.
var capabilityBits = []struct {
	mask uint16
	set  string
	clr  string
}{
	{0x0001, "frame-transfer CCD", "full-frame/interline CCD"},
	{0x0002, "electronic shutter", "no electronic shutter"},
	{0x0004, "external tracker supported", "no external tracker"},
	{0x0008, "BTDI supported", "no BTDI"},
	{0x0010, "AO-8 present", "no AO-8"},
	{0x0020, "internal frame buffer", "no frame buffer"},
	{0x0040, "requires StartExposure2", "StartExposure ok"},
}

// bcd decodes SBIG's BCD encoded values: gain XX.XX e/ADU, pixel size XXXXXX.XX um.
func bcd(n uint32) float64 {
	v, scale := 0.0, 1.0
	for n > 0 {
		d := n & 0xF
		if d > 9 {
			return math.NaN()
		}
		v += float64(d) * scale
		scale *= 10
		n >>= 4
	}
	return v / 100
}
