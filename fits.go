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
		return fmt.Sprintf("%-80.80s", fmt.Sprintf("%-8.8s %s", c.key, c.cmt))
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
	return fmt.Sprintf("%-80.80s", line)
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

// ReadFITS reads the first 2-D image of a FITS file into uint16 (clamped).
func ReadFITS(path string) ([]uint16, int, int, map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	for hdu := 0; hdu < 8; hdu++ {
		hdr := map[string]string{}
		var order []string
		block := make([]byte, 2880)
		end := false
		for !end {
			if _, err := io.ReadFull(r, block); err != nil {
				return nil, 0, 0, nil, fmt.Errorf("reading FITS header: %v", err)
			}
			for i := 0; i < 36; i++ {
				card := string(block[i*80 : (i+1)*80])
				key := strings.TrimSpace(card[:8])
				if key == "END" {
					end = true
					break
				}
				if len(card) > 10 && card[8:10] == "= " {
					val := strings.TrimSpace(card[10:])
					if strings.HasPrefix(val, "'") {
						if j := strings.Index(val[1:], "'"); j >= 0 {
							val = val[1 : j+1]
						}
					} else if j := strings.Index(val, "/"); j >= 0 {
						val = strings.TrimSpace(val[:j])
					}
					hdr[key] = strings.TrimSpace(val)
					order = append(order, key)
				}
			}
		}
		atoi := func(k string) int { v, _ := strconv.Atoi(hdr[k]); return v }
		atof := func(k string, d float64) float64 {
			if s, ok := hdr[k]; ok {
				if v, err := strconv.ParseFloat(strings.Replace(s, "D", "E", 1), 64); err == nil {
					return v
				}
			}
			return d
		}
		bitpix := atoi("BITPIX")
		naxis := atoi("NAXIS")
		nbytes := 0
		if naxis > 0 {
			nbytes = abs(bitpix) / 8
			for i := 1; i <= naxis; i++ {
				nbytes *= atoi(fmt.Sprintf("NAXIS%d", i))
			}
		}
		if hdr["XTENSION"] == "BINTABLE" || naxis < 2 || bitpix == 0 {
			// skip this HDU's data (includes heap via PCOUNT)
			skip := nbytes + atoi("PCOUNT")
			skip = (skip + 2879) / 2880 * 2880
			if _, err := io.CopyN(io.Discard, r, int64(skip)); err != nil {
				return nil, 0, 0, nil, fmt.Errorf("no 2-D image found in %s", path)
			}
			continue
		}
		w, h := atoi("NAXIS1"), atoi("NAXIS2")
		bz, bs := atof("BZERO", 0), atof("BSCALE", 1)
		bpp := abs(bitpix) / 8
		raw := make([]byte, w*h*bpp)
		if _, err := io.ReadFull(r, raw); err != nil {
			return nil, 0, 0, nil, fmt.Errorf("reading FITS data: %v", err)
		}
		pix := make([]uint16, w*h)
		for i := range pix {
			var v float64
			b := raw[i*bpp:]
			switch bitpix {
			case 8:
				v = float64(b[0])
			case 16:
				v = float64(int16(binary.BigEndian.Uint16(b)))
			case 32:
				v = float64(int32(binary.BigEndian.Uint32(b)))
			case 64:
				v = float64(int64(binary.BigEndian.Uint64(b)))
			case -32:
				v = float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
			case -64:
				v = math.Float64frombits(binary.BigEndian.Uint64(b))
			}
			v = v*bs + bz
			if bitpix < 0 && atof("DATAMAX", 0) <= 1.0 && atof("DATAMAX", 0) > 0 {
				v *= 65535 // normalized float images
			}
			if math.IsNaN(v) || v < 0 {
				v = 0
			}
			if v > 65535 {
				v = 65535
			}
			pix[i] = uint16(v + 0.5)
		}
		_ = order
		return pix, w, h, hdr, nil
	}
	return nil, 0, 0, nil, fmt.Errorf("no 2-D image found in %s", path)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
