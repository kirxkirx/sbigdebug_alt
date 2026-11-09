package main

import (
	"math"
	"math/rand"
	"time"
)

// simTransport emulates an STXL-11002 at the SBIGUnivDrvCommand level, including
// the reported failure (first frame after power-on fine except a bright stripe at
// the bottom, then a bright region growing upward, then saturated frames).
// It is used for the demo mode and for CI tests of the whole program.
type simTransport struct {
	dcp                   map[uint16]uint32
	opts                  CustomerOptions
	frames                int
	expStart              time.Time
	expTicks              uint32
	mode, top, left, h, w uint16
	line                  int
	rng                   *rand.Rand
	healthy               bool // -simulate-healthy: no failure
	exposing              bool
}

func newSimTransport(healthy bool) *simTransport {
	s := &simTransport{dcp: map[uint16]uint32{}, rng: rand.New(rand.NewSource(1)), healthy: healthy}
	for k, v := range dcpDocDefault {
		s.dcp[uint16(k)] = uint32(v)
	}
	s.dcp[DCP_OVERSCAN_ROWSCOLS] = 0
	s.dcp[DCP_PIXEL_PIPELINE_ENABLE] = 1
	s.opts = CustomerOptions{WindowHeater: 1}
	return s
}

func (s *simTransport) Close() error { return nil }
func (s *simTransport) Describe() []string {
	return []string{"Driver DLL loaded : (simulator - no camera)"}
}

func (s *simTransport) modeDims(m uint16) (int, int) {
	switch m {
	case 1:
		return 2004, 1336
	case 2:
		return 1336, 890
	}
	return 4008, 2672
}

func (s *simTransport) Call(cmd uint16, p, r []byte) uint16 {
	switch cmd {
	case CC_OPEN_DRIVER, CC_CLOSE_DRIVER, CC_OPEN_DEVICE, CC_CLOSE_DEVICE, CC_MISCELLANEOUS_CONTROL, CC_DEBUG_LOG:
		return 0
	case CC_ESTABLISH_LINK:
		L_EstablishLinkResults.Put16(r, "cameraType", 17)
		return 0
	case CC_GET_ERROR_STRING:
		L_GetErrorStringResults.PutStr(r, "errorString", 64, errName(L_GetErrorStringParams.U16(p, "errorNo")))
		return 0
	case CC_GET_DRIVER_INFO:
		L_GetDriverInfoResults0.Put16(r, "version", 0x0499)
		L_GetDriverInfoResults0.PutStr(r, "name", 64, "SBIG Universal Driver (simulated) Build 8")
		L_GetDriverInfoResults0.Put16(r, "maxRequest", 2)
		return 0
	case CC_GET_CCD_INFO:
		req := L_GetCCDInfoParams.U16(p, "request")
		switch req {
		case 0:
			L := L_GetCCDInfoResults0
			L.Put16(r, "firmwareVersion", 0x0284)
			L.Put16(r, "cameraType", 17)
			L.PutStr(r, "name", 64, "SBIG STXL-11002 3 CCD Camera (simulated)")
			L.Put16(r, "readoutModes", 3)
			for k := 0; k < 3; k++ {
				e := r[L.f("readoutInfo").off+k*16:]
				w, h := s.modeDims(uint16(k))
				L_READOUT_INFO.Put16(e, "mode", uint16(k))
				L_READOUT_INFO.Put16(e, "width", uint16(w))
				L_READOUT_INFO.Put16(e, "height", uint16(h))
				L_READOUT_INFO.Put16(e, "gain", 0x0080)
				L_READOUT_INFO.Put32(e, "pixel_width", uint32(0x900*(k+1)))
				L_READOUT_INFO.Put32(e, "pixel_height", uint32(0x900*(k+1)))
			}
			return 0
		case 1:
			return CE_BAD_PARAMETER
		case 2:
			L_GetCCDInfoResults2.PutStr(r, "serialNumber", 10, "SIM0001")
			L_GetCCDInfoResults2.Put16(r, "imagingABG", 1)
			return 0
		case 4:
			L_GetCCDInfoResults4.Put16(r, "capabilitiesBits", 0x0062)
			return 0
		case 6:
			return 0
		}
		return CE_BAD_PARAMETER
	case CC_QUERY_TEMPERATURE_STATUS:
		if L_QueryTemperatureStatusParams.U16(p, "request") == TEMP_STATUS_ADVANCED2 {
			L := L_QueryTemperatureStatusResults2
			L.PutF64(r, "ccdSetpoint", 0)
			L.PutF64(r, "imagingCCDTemperature", -0.3+0.01*float64(s.frames))
			L.PutF64(r, "ambientTemperature", 1.5)
			L.PutF64(r, "heatsinkTemperature", 2.0+0.05*float64(s.frames))
			L.Put16(r, "fanEnabled", 1)
			L.PutF64(r, "fanPower", 100)
			return 0
		}
		return 0
	case CC_GET_LINK_STATUS:
		L_GetLinkStatusResults.Put16(r, "linkEstablished", 1)
		L_GetLinkStatusResults.Put16(r, "cameraType", 17)
		L_GetLinkStatusResults.Put32(r, "comTotal", uint32(1000+s.frames*3000))
		return 0
	case CC_GET_DRIVER_CONTROL:
		i := L_GetDriverControlParams.U16(p, "controlParameter")
		if int(i) >= len(dcpNames) {
			return CE_BAD_PARAMETER
		}
		L_GetDriverControlResults.Put32(r, "controlValue", s.dcp[i])
		return 0
	case CC_SET_DRIVER_CONTROL:
		i := L_SetDriverControlParams.U16(p, "controlParameter")
		if int(i) >= len(dcpNames) {
			return CE_BAD_PARAMETER
		}
		s.dcp[i] = L_SetDriverControlParams.U32(p, "controlValue")
		return 0
	case CC_CUSTOMER_OPTIONS:
		L := L_CustomerOptionsParams
		if L.U16(p, "bSetCustomerOptions") != 0 {
			s.opts = CustomerOptions{L.U16(p, "bOverscanRegions"), L.U16(p, "bWindowHeater"), L.U16(p, "bPreflashCcd"), L.U16(p, "bVddNormallyOff")}
		}
		L.Put16(r, "bOverscanRegions", s.opts.Overscan)
		L.Put16(r, "bWindowHeater", s.opts.WindowHeater)
		L.Put16(r, "bPreflashCcd", s.opts.Preflash)
		L.Put16(r, "bVddNormallyOff", s.opts.VddNormallyOff)
		return 0
	case CC_GET_RBI_PARAMETERS:
		return CE_BAD_CAMERA_COMMAND
	case CC_COLUMN_EEPROM:
		return 0
	case CC_READ_OFFSET2:
		L_ReadOffsetResults2.Put16(r, "offset", s.offset())
		L_ReadOffsetResults2.PutF64(r, "rms", 11.5)
		return 0
	case CC_READ_OFFSET:
		L_ReadOffsetResults.Put16(r, "offset", s.offset())
		return 0
	case CC_BIT_IO:
		return 0
	case CC_QUERY_FEATURE_SUPPORTED, CC_READOUT_IN_PROGRESS:
		return 0
	case CC_QUERY_USB3:
		L := L_QueryUSBResults3
		L.Put16(r, "camerasFound", 1)
		e := r[L.f("usbInfo").off:]
		L_QUERY_USB_INFO.Put16(e, "cameraFound", 1)
		L_QUERY_USB_INFO.Put16(e, "cameraType", 17)
		L_QUERY_USB_INFO.PutStr(e, "name", 64, "SBIG STXL-11002 (simulated)")
		L_QUERY_USB_INFO.PutStr(e, "serialNumber", 10, "SIM0001")
		return 0
	case CC_START_EXPOSURE2:
		L := L_StartExposureParams2
		s.expStart = time.Now()
		s.expTicks = L.U32(p, "exposureTime") & EXP_TIME_MASK
		s.mode, s.top, s.left = L.U16(p, "readoutMode"), L.U16(p, "top"), L.U16(p, "left")
		s.h, s.w = L.U16(p, "height"), L.U16(p, "width")
		s.exposing = true
		return 0
	case CC_QUERY_COMMAND_STATUS:
		st := uint16(CS_IDLE)
		if s.exposing {
			st = CS_INTEGRATING
			if time.Since(s.expStart) >= time.Duration(max(18, int(s.expTicks)))*10*time.Millisecond {
				st = CS_INTEGRATION_COMPLETE
			}
		}
		L_QueryCommandStatusResults.Put16(r, "status", st)
		return 0
	case CC_END_EXPOSURE:
		s.exposing = false
		return 0
	case CC_START_READOUT:
		s.line = 0
		return 0
	case CC_READOUT_LINE:
		n := int(L_ReadoutLineParams.U16(p, "pixelLength"))
		_, fullH := s.modeDims(s.mode)
		y := int(s.top) + s.line
		frac := float64(y) / float64(fullH) // 0 top .. 1 bottom
		for x := 0; x < n; x++ {
			le.PutUint16(r[2*x:], s.pixel(frac, x))
		}
		s.line++
		return 0
	case CC_END_READOUT:
		s.frames++
		return 0
	case CC_DUMP_LINES:
		return 0
	}
	return CE_UNKNOWN_COMMAND
}

func (s *simTransport) offset() uint16 {
	if s.healthy || s.frames == 0 {
		return 1012
	}
	return 65535
}

// pixel models the observed failure: a level that runs away from the bottom of the
// frame upwards with every frame.
func (s *simTransport) pixel(frac float64, x int) uint16 {
	base := 1000 + 11*s.rng.NormFloat64()
	if s.healthy {
		return clamp16(base)
	}
	// Edge of the bad region moves up with each frame: 0.97, 0.5, -0.2 ...
	edge := 0.97 - 0.48*float64(s.frames)
	if s.dcp[DCP_VDD_OPTIMIZED] == 0 {
		edge += 0 // (no effect in the simulator - the real camera decides)
	}
	if frac >= edge {
		return clamp16(56000 + 9000*math.Min(1, (frac-edge)*20) + 300*s.rng.NormFloat64())
	}
	return clamp16(base)
}

func clamp16(v float64) uint16 {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}
