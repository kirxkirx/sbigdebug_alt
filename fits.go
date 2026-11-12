package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

type fitsCard struct {
	key   string
	value interface{}
	cmt   string
}

func formatCard(c fitsCard) string {
	var v string
	switch x := c.value.(type) {
	case nil:
		return card80(fmt.Sprintf("%-8.8s %s", c.key, c.cmt))
	case bool:
		if x {
			v = fmt.Sprintf("%20s", "T")
		} else {
			v = fmt.Sprintf("%20s", "F")
		}
	case int:
		v = fmt.Sprintf("%20d", x)
	case int64:
		v = fmt.Sprintf("%20d", x)
	case uint32:
		v = fmt.Sprintf("%20d", x)
	case float64:
		v = fmt.Sprintf("%20s", strconv.FormatFloat(x, 'G', 10, 64))
		if !strings.ContainsAny(v, ".E") {
			v = fmt.Sprintf("%20s", strings.TrimSpace(v)+".0")
		}
	case string:
		s := strings.ReplaceAll(x, "'", "''")
		if len(s) > 68 {
			s = s[:68]
		}
		v = fmt.Sprintf("'%-8s'", s)
	default:
		v = fmt.Sprintf("'%v'", x)
	}
	line := fmt.Sprintf("%-8.8s= %s", c.key, v)
	if c.cmt != "" {
		line += " / " + c.cmt
	}
	return card80(line)
}

// card80 makes an exactly 80-byte FITS card of printable ASCII (FITS forbids
// other bytes; e.g. "µs" from time.Duration would otherwise shift the header).
func card80(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] < 0x20 || b[i] > 0x7E {
			b[i] = '?'
		}
	}
	if len(b) > 80 {
		b = b[:80]
	}
	return string(b) + strings.Repeat(" ", 80-len(b))
}

// WriteFITS writes a 16-bit unsigned image (BITPIX=16, BZERO=32768).
// Row 0 of the data array is the first line returned by the driver.
func WriteFITS(path string, pix []uint16, w, h int, extra []fitsCard) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 1<<20)
	cards := []fitsCard{
		{"SIMPLE", true, "conforms to FITS standard"},
		{"BITPIX", 16, "16-bit integers"},
		{"NAXIS", 2, ""},
		{"NAXIS1", w, "columns"},
		{"NAXIS2", h, "rows"},
		{"BZERO", 32768.0, "unsigned 16-bit data"},
		{"BSCALE", 1.0, ""},
	}
	cards = append(cards, extra...)
	n := 0
	for _, c := range cards {
		bw.WriteString(formatCard(c))
		n++
	}
	bw.WriteString(fmt.Sprintf("%-80s", "END"))
	n++
	for ; n%36 != 0; n++ {
		bw.WriteString(strings.Repeat(" ", 80))
	}
	buf := make([]byte, 2)
	for _, v := range pix[:w*h] {
		binary.BigEndian.PutUint16(buf, uint16(int32(v)-32768))
		bw.Write(buf)
	}
	pad := (2880 - (2*w*h)%2880) % 2880
	bw.Write(make([]byte, pad))
	return bw.Flush()
}

// ReadFITS reads the first supported 2-D image. It never clamps an invalid
// physical value into a plausible camera sample. Floating physical values are
// rounded explicitly; DATAMAX describes the image and never changes its units.
func ReadFITS(path string) ([]uint16, int, int, map[string]string, error) {
	f, err := openImageFile(path)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, 0, nil, err
	}
	fail := func(err error) ([]uint16, int, int, map[string]string, error) { return nil, 0, 0, nil, err }
	for hdu := 0; hdu < 64; hdu++ {
		hdr := map[string]string{}
		block := make([]byte, 2880)
		end := false
		for blocks := 0; !end; blocks++ {
			if blocks >= 1024 {
				return fail(fmt.Errorf("FITS header exceeds 2880 KiB"))
			}
			if _, err = io.ReadFull(f, block); err != nil {
				return fail(fmt.Errorf("reading FITS header: %w", err))
			}
			if blocks == 0 {
				first := "XTENSION"
				if hdu == 0 {
					first = "SIMPLE  "
				}
				if string(block[:8]) != first {
					return fail(fmt.Errorf("expected %s as the first FITS card", strings.TrimSpace(first)))
				}
			}
			for i := 0; i < 36; i++ {
				card := string(block[i*80 : (i+1)*80])
				key := strings.TrimSpace(card[:8])
				if key == "END" {
					end = true
					break
				}
				if card[8:10] == "= " {
					if _, exists := hdr[key]; exists && structuralFITSKeyword(key) {
						return fail(fmt.Errorf("duplicate structural FITS keyword %s", key))
					}
					value, e := fitsValue(card[10:])
					if e != nil {
						return fail(e)
					}
					hdr[key] = value
				}
			}
		}
		if hdu == 0 && hdr["SIMPLE"] != "T" {
			return fail(fmt.Errorf("FITS SIMPLE must be T"))
		}
		if hdr["GROUPS"] == "T" {
			return fail(fmt.Errorf("FITS random groups unsupported"))
		}
		integer := func(k string, def int64, required bool) (int64, error) {
			v, ok := hdr[k]
			if !ok {
				if required {
					return 0, fmt.Errorf("missing FITS %s", k)
				}
				return def, nil
			}
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil {
				return 0, fmt.Errorf("invalid FITS %s: %q", k, v)
			}
			return n, nil
		}
		number := func(k string, def float64) (float64, error) {
			v, ok := hdr[k]
			if !ok {
				return def, nil
			}
			n, e := strconv.ParseFloat(strings.ReplaceAll(strings.ToUpper(v), "D", "E"), 64)
			if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				return 0, fmt.Errorf("invalid FITS %s: %q", k, v)
			}
			return n, nil
		}
		bitpix, e := integer("BITPIX", 0, true)
		if e != nil {
			return fail(e)
		}
		bpp := map[int64]int64{8: 1, 16: 2, 32: 4, 64: 8, -32: 4, -64: 8}[bitpix]
		if bpp == 0 {
			return fail(fmt.Errorf("unsupported FITS BITPIX %d", bitpix))
		}
		naxis, e := integer("NAXIS", 0, true)
		if e != nil {
			return fail(e)
		}
		if naxis < 0 || naxis > 9 {
			return fail(fmt.Errorf("unsupported FITS NAXIS %d", naxis))
		}
		dimensions := make([]int64, naxis)
		elements := int64(0)
		if naxis > 0 {
			elements = 1
		}
		for i := range dimensions {
			v, e := integer(fmt.Sprintf("NAXIS%d", i+1), 0, true)
			if e != nil {
				return fail(e)
			}
			if v < 0 {
				return fail(fmt.Errorf("negative FITS dimension"))
			}
			dimensions[i] = v
			if v != 0 && elements > math.MaxInt64/v {
				return fail(fmt.Errorf("FITS dimension overflow"))
			}
			elements *= v
		}
		pcount, e := integer("PCOUNT", 0, false)
		if e != nil {
			return fail(e)
		}
		gcount, e := integer("GCOUNT", 1, false)
		if e != nil {
			return fail(e)
		}
		if pcount < 0 || gcount < 1 || elements > math.MaxInt64-pcount {
			return fail(fmt.Errorf("invalid FITS group sizes"))
		}
		elements += pcount
		if elements > math.MaxInt64/gcount || elements*gcount > math.MaxInt64/bpp {
			return fail(fmt.Errorf("FITS byte count overflow"))
		}
		nbytes := elements * gcount * bpp
		pos, e := f.Seek(0, io.SeekCurrent)
		if e != nil {
			return fail(e)
		}
		if pos > st.Size() || nbytes > st.Size()-pos {
			return fail(fmt.Errorf("truncated FITS data"))
		}
		image := naxis == 2 && (hdr["XTENSION"] == "" || hdr["XTENSION"] == "IMAGE")
		if !image {
			pad := (2880 - nbytes%2880) % 2880
			if nbytes > math.MaxInt64-pad || nbytes+pad > st.Size()-pos {
				return fail(fmt.Errorf("truncated FITS HDU"))
			}
			if _, e = f.Seek(nbytes+pad, io.SeekCurrent); e != nil {
				return fail(e)
			}
			continue
		}
		if pcount != 0 || gcount != 1 || hdr["GROUPS"] == "T" {
			return fail(fmt.Errorf("FITS random groups unsupported"))
		}
		if bitpix == 64 {
			return fail(fmt.Errorf("64-bit integer FITS images are unsupported; export without loss to 16/32-bit ADU"))
		}
		n, e := imageSize(dimensions[0], dimensions[1], bpp)
		if e != nil {
			return fail(e)
		}
		bz, e := number("BZERO", 0)
		if e != nil {
			return fail(e)
		}
		bs, e := number("BSCALE", 1)
		if e != nil {
			return fail(e)
		}
		blank, e := integer("BLANK", 0, false)
		if e != nil {
			return fail(e)
		}
		_, hasBlank := hdr["BLANK"]
		if hasBlank && bitpix < 0 {
			return fail(fmt.Errorf("BLANK is only valid for integer FITS images"))
		}
		raw := make([]byte, n*int(bpp))
		if _, e = io.ReadFull(f, raw); e != nil {
			return fail(e)
		}
		pix := make([]uint16, n)
		for i := range pix {
			b := raw[i*int(bpp):]
			var v float64
			switch bitpix {
			case 8:
				v = float64(b[0])
			case 16:
				v = float64(int16(binary.BigEndian.Uint16(b)))
			case 32:
				v = float64(int32(binary.BigEndian.Uint32(b)))
			case -32:
				v = float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
			case -64:
				v = math.Float64frombits(binary.BigEndian.Uint64(b))
			}
			if hasBlank && int64(v) == blank {
				return fail(fmt.Errorf("undefined FITS sample %d (BLANK)", i))
			}
			v = v*bs + bz
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 65535 {
				return fail(fmt.Errorf("FITS sample %d is outside finite 0..65535 ADU; refusing clipping", i))
			}
			pix[i] = uint16(math.Round(v))
		}
		if bitpix < 0 || bs != math.Trunc(bs) || bz != math.Trunc(bz) {
			hdr["SBIGDIAG_CONVERSION"] = "physical samples rounded to integer ADU"
		}
		return pix, int(dimensions[0]), int(dimensions[1]), hdr, nil
	}
	return fail(fmt.Errorf("no supported 2-D image found in %s", path))
}

func structuralFITSKeyword(key string) bool {
	switch key {
	case "SIMPLE", "XTENSION", "BITPIX", "BSCALE", "BZERO", "BLANK", "PCOUNT", "GCOUNT", "GROUPS":
		return true
	}
	return strings.HasPrefix(key, "NAXIS")
}

// Reject special files before opening them (notably a FIFO that could block
// before f.Stat), and recheck the descriptor to catch ordinary path changes.
func openImageFile(path string) (*os.File, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("image input must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("image input must be a regular file")
	}
	return f, nil
}

func fitsValue(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "'") {
		return strings.TrimSpace(strings.SplitN(s, "/", 2)[0]), nil
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			b.WriteByte('\'')
			i++
			continue
		}
		return strings.TrimSpace(b.String()), nil
	}
	return "", fmt.Errorf("unterminated FITS string")
}

// Bound the combined input/pixel allocations on both 32- and 64-bit builds.
// STXL-11002 and STXL-16200 full frames fit comfortably within this budget.
func imageSize(w, h, bpp int64) (int, error) {
	const maxPixels = 32_000_000
	const maxBytes = 256 << 20
	if w <= 0 || h <= 0 || w > 65535 || h > 65535 || bpp <= 0 || w > maxPixels/h {
		return 0, fmt.Errorf("invalid or excessive image dimensions %dx%d (limit %d pixels and 65535 per axis)", w, h, maxPixels)
	}
	n := w * h
	if n > maxBytes/bpp {
		return 0, fmt.Errorf("image exceeds %d-byte input budget", maxBytes)
	}
	return int(n), nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
