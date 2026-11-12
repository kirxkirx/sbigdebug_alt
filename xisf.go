package main

import (
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// ReadXISF reads an uncompressed monochrome XISF 1.0 image. Unsupported
// channel layouts are rejected instead of being mistaken for monochrome pixels.
func ReadXISF(path string) ([]uint16, int, int, map[string]string, error) {
	f, err := openImageFile(path)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	defer f.Close()
	fail := func(e error) ([]uint16, int, int, map[string]string, error) { return nil, 0, 0, nil, e }
	st, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	pre := make([]byte, 16)
	if _, err := io.ReadFull(f, pre); err != nil || string(pre[:8]) != "XISF0100" {
		return fail(fmt.Errorf("not an XISF 1.0 file"))
	}
	hlen := int64(binary.LittleEndian.Uint32(pre[8:]))
	if hlen <= 0 || hlen > 4<<20 || hlen > st.Size()-16 {
		return fail(fmt.Errorf("invalid or excessive XISF header size"))
	}
	header := make([]byte, int(hlen))
	if _, err = io.ReadFull(f, header); err != nil {
		return fail(err)
	}
	type keyword struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	}
	type imageHeader struct {
		Attrs    []xml.Attr `xml:",any,attr"`
		Keywords []keyword  `xml:"FITSKeyword"`
	}
	var doc struct {
		XMLName xml.Name      `xml:"xisf"`
		Version string        `xml:"version,attr"`
		Images  []imageHeader `xml:"Image"`
	}
	if err = xml.Unmarshal(header, &doc); err != nil {
		return fail(fmt.Errorf("XISF XML: %w", err))
	}
	if doc.Version != "1.0" || len(doc.Images) == 0 {
		return fail(fmt.Errorf("missing XISF 1.0 image"))
	}
	img := doc.Images[0]
	attr := map[string]string{}
	for _, a := range img.Attrs {
		if _, exists := attr[a.Name.Local]; exists {
			return fail(fmt.Errorf("duplicate XISF attribute %s", a.Name.Local))
		}
		attr[a.Name.Local] = a.Value
	}
	if attr["compression"] != "" {
		return fail(fmt.Errorf("compressed XISF unsupported; save uncompressed or FITS"))
	}
	geo := strings.Split(attr["geometry"], ":")
	loc := strings.Split(attr["location"], ":")
	if len(geo) != 3 || geo[2] != "1" || len(loc) != 3 || loc[0] != "attachment" {
		return fail(fmt.Errorf("only 2-D monochrome attached XISF images are supported"))
	}
	if p := attr["pixelStorage"]; p != "" && p != "Planar" && p != "Normal" {
		return fail(fmt.Errorf("unknown XISF pixelStorage %q", p))
	}
	if c := attr["colorSpace"]; c != "" && c != "Gray" {
		return fail(fmt.Errorf("only grayscale XISF is supported"))
	}
	w, e := strconv.ParseInt(geo[0], 10, 64)
	if e != nil {
		return fail(e)
	}
	h, e := strconv.ParseInt(geo[1], 10, 64)
	if e != nil {
		return fail(e)
	}
	bpp := map[string]int64{"UInt8": 1, "UInt16": 2, "UInt32": 4, "Float32": 4, "Float64": 8}[attr["sampleFormat"]]
	n, e := imageSize(w, h, bpp)
	if e != nil {
		return fail(e)
	}
	pos, e := strconv.ParseInt(loc[1], 10, 64)
	if e != nil {
		return fail(e)
	}
	length, e := strconv.ParseInt(loc[2], 10, 64)
	if e != nil {
		return fail(e)
	}
	if pos < 16+hlen || pos > st.Size() || length != int64(n)*bpp || length > st.Size()-pos {
		return fail(fmt.Errorf("invalid or truncated XISF attachment"))
	}
	var bo binary.ByteOrder = binary.LittleEndian
	switch attr["byteOrder"] {
	case "", "little":
	case "big":
		bo = binary.BigEndian
	default:
		return fail(fmt.Errorf("unknown XISF byte order"))
	}
	raw := make([]byte, int(length))
	if _, e = f.ReadAt(raw, pos); e != nil {
		return fail(e)
	}
	pix := make([]uint16, n)
	for i := range pix {
		b := raw[i*int(bpp):]
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
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 65535 {
			return fail(fmt.Errorf("XISF sample %d outside finite normalized range; refusing clipping", i))
		}
		pix[i] = uint16(math.Round(v))
	}
	hdr := map[string]string{}
	for _, k := range img.Keywords {
		hdr[k.Name] = strings.Trim(strings.TrimSpace(k.Value), "'")
	}
	if attr["sampleFormat"] != "UInt16" {
		hdr["SBIGDIAG_CONVERSION"] = "XISF " + attr["sampleFormat"] + " scaled to 0..65535 and rounded"
	}
	return pix, int(w), int(h), hdr, nil
}

// ReadImage dispatches on the file signature.
func ReadImage(path string) ([]uint16, int, int, map[string]string, error) {
	f, err := openImageFile(path)
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
