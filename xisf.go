package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var (
	reImage   = regexp.MustCompile(`(?s)<Image\s[^>]*>`)
	reAttr    = regexp.MustCompile(`(\w+)="([^"]*)"`)
	reFITSKey = regexp.MustCompile(`<FITSKeyword\s+name="([^"]*)"\s+value="([^"]*)"`)
)

// ReadXISF reads the first image of an uncompressed XISF 1.0 file (as written by
// NINA/PixInsight with compression off) into uint16. Only the first channel is used.
func ReadXISF(path string) ([]uint16, int, int, map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	defer f.Close()
	pre := make([]byte, 16)
	if _, err := io.ReadFull(f, pre); err != nil || string(pre[:8]) != "XISF0100" {
		return nil, 0, 0, nil, fmt.Errorf("not an XISF 1.0 file")
	}
	hlen := binary.LittleEndian.Uint32(pre[8:])
	if hlen > 64<<20 {
		return nil, 0, 0, nil, fmt.Errorf("XISF header too large")
	}
	xml := make([]byte, hlen)
	if _, err := io.ReadFull(f, xml); err != nil {
		return nil, 0, 0, nil, err
	}
	img := reImage.Find(xml)
	if img == nil {
		return nil, 0, 0, nil, fmt.Errorf("no <Image> in XISF header")
	}
	attr := map[string]string{}
	for _, m := range reAttr.FindAllSubmatch(img, -1) {
		attr[string(m[1])] = string(m[2])
	}
	if attr["compression"] != "" {
		return nil, 0, 0, nil, fmt.Errorf("compressed XISF (%s) not supported - save uncompressed or FITS", attr["compression"])
	}
	geo := strings.Split(attr["geometry"], ":")
	loc := strings.Split(attr["location"], ":")
	if len(geo) < 2 || len(loc) != 3 || loc[0] != "attachment" {
		return nil, 0, 0, nil, fmt.Errorf("unsupported XISF geometry/location %q %q", attr["geometry"], attr["location"])
	}
	w, _ := strconv.Atoi(geo[0])
	h, _ := strconv.Atoi(geo[1])
	pos, _ := strconv.ParseInt(loc[1], 10, 64)
	bpp := map[string]int{"UInt8": 1, "UInt16": 2, "UInt32": 4, "Float32": 4, "Float64": 8}[attr["sampleFormat"]]
	if w <= 0 || h <= 0 || bpp == 0 {
		return nil, 0, 0, nil, fmt.Errorf("unsupported XISF image %q %q", attr["geometry"], attr["sampleFormat"])
	}
	raw := make([]byte, w*h*bpp)
	if _, err := f.ReadAt(raw, pos); err != nil {
		return nil, 0, 0, nil, err
	}
	big := attr["byteOrder"] == "big"
	var bo binary.ByteOrder = binary.LittleEndian
	if big {
		bo = binary.BigEndian
	}
	pix := make([]uint16, w*h)
	for i := range pix {
		b := raw[i*bpp:]
		var v float64
		switch attr["sampleFormat"] {
		case "UInt8":
			v = float64(b[0]) * 257
		case "UInt16":
			v = float64(bo.Uint16(b))
		case "UInt32":
			v = float64(bo.Uint32(b)) / 65537
		case "Float32":
			v = float64(math.Float32frombits(bo.Uint32(b))) * 65535
		case "Float64":
			v = math.Float64frombits(bo.Uint64(b)) * 65535
		}
		pix[i] = clamp16(v + 0.5)
	}
	hdr := map[string]string{}
	for _, m := range reFITSKey.FindAllSubmatch(xml, -1) {
		hdr[string(m[1])] = strings.Trim(strings.TrimSpace(string(m[2])), "'")
	}
	return pix, w, h, hdr, nil
}

// ReadImage dispatches on the file signature.
func ReadImage(path string) ([]uint16, int, int, map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	sig := make([]byte, 8)
	_, _ = io.ReadFull(f, sig)
	f.Close()
	if string(sig) == "XISF0100" {
		return ReadXISF(path)
	}
	return ReadFITS(path)
}
