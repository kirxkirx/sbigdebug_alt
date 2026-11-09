//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// dllTransport calls SBIGUnivDrvCommand() in the vendor SBIGUDrv.dll.
// short __stdcall SBIGUnivDrvCommand(short command, void *Params, void *pResults);
// (On Win64 there is only one calling convention; Go's syscall layer handles
// stdcall on Win32.) All calls happen on the main OS thread (see main.go).
type dllTransport struct {
	dll  *syscall.DLL
	proc *syscall.Proc
	path string
}

func loadDriverDLL(path string) (transport, error) {
	if path == "" {
		path = "SBIGUDrv.dll"
	}
	d, err := syscall.LoadDLL(path)
	if err != nil {
		return nil, fmt.Errorf("cannot load %s: %v (is the SBIG driver installed? 64-bit exe needs the 64-bit DLL, 32-bit exe the 32-bit DLL; try the other exe or -dll <path>)", path, err)
	}
	p, err := d.FindProc("SBIGUnivDrvCommand")
	if err != nil {
		d.Release()
		return nil, fmt.Errorf("%s has no SBIGUnivDrvCommand export: %v", path, err)
	}
	t := &dllTransport{dll: d, proc: p, path: modulePath(d.Handle)}
	if t.path == "" {
		t.path = path
	}
	return t, nil
}

func ptr(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&b[0]))
}

func (t *dllTransport) Call(cmd uint16, params, results []byte) uint16 {
	var pp, rp uintptr
	if len(params) > 0 {
		pp = uintptr(unsafe.Pointer(&params[0]))
	}
	if len(results) > 0 {
		rp = uintptr(unsafe.Pointer(&results[0]))
	}
	r, _, _ := syscall.SyscallN(t.proc.Addr(), uintptr(cmd), pp, rp)
	return uint16(r)
}

func (t *dllTransport) Close() error { return t.dll.Release() }

func (t *dllTransport) Describe() []string {
	return []string{
		"Driver DLL loaded : " + t.path,
		"Driver DLL version: " + fileVersion(t.path),
		"Driver DLL arch   : " + peMachine(t.path),
	}
}

var (
	modKernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetModuleFileNameW      = modKernel32.NewProc("GetModuleFileNameW")
	modVersion                  = syscall.NewLazyDLL("version.dll")
	procGetFileVersionInfoSizeW = modVersion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = modVersion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = modVersion.NewProc("VerQueryValueW")
)

func modulePath(h syscall.Handle) string {
	buf := make([]uint16, 1024)
	n, _, _ := procGetModuleFileNameW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

// fileVersion returns "FileVersion (ProductVersion) size date" of a PE file.
func fileVersion(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "not found"
	}
	info := fmt.Sprintf("%d bytes, modified %s", st.Size(), st.ModTime().Format("2006-01-02 15:04"))
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return info
	}
	var h uint32
	sz, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&h)))
	if sz == 0 {
		return "no version resource, " + info
	}
	buf := make([]byte, sz)
	ok, _, _ := procGetFileVersionInfoW.Call(uintptr(unsafe.Pointer(p)), 0, sz, uintptr(unsafe.Pointer(&buf[0])))
	if ok == 0 {
		return info
	}
	var fixed *[13]uint32
	var flen uint32
	root, _ := syscall.UTF16PtrFromString(`\`)
	ok, _, _ = procVerQueryValueW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(root)),
		uintptr(unsafe.Pointer(&fixed)), uintptr(unsafe.Pointer(&flen)))
	if ok == 0 || flen < 52 || fixed == nil {
		return info
	}
	// VS_FIXEDFILEINFO: [0]sig [1]strucver [2]FileVersionMS [3]FileVersionLS [4]ProductVersionMS [5]ProductVersionLS
	fv := fmt.Sprintf("%d.%d.%d.%d", fixed[2]>>16, fixed[2]&0xFFFF, fixed[3]>>16, fixed[3]&0xFFFF)
	pv := fmt.Sprintf("%d.%d.%d.%d", fixed[4]>>16, fixed[4]&0xFFFF, fixed[5]>>16, fixed[5]&0xFFFF)
	return fmt.Sprintf("file %s, product %s, %s", fv, pv, info)
}

// candidateDLLs lists SBIGUDrv.dll copies a Windows PC may have (system, NINA, CCDOps, MaxIm).
func candidateDLLs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		lp := strings.ToLower(p)
		if seen[lp] {
			return
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			seen[lp] = true
			out = append(out, p)
		}
	}
	win := os.Getenv("SystemRoot")
	if win == "" {
		win = `C:\Windows`
	}
	// A 32-bit process sees SysWOW64 as System32; Sysnative reveals the real 64-bit System32.
	add(filepath.Join(win, "Sysnative", "SBIGUDrv.dll"))
	add(filepath.Join(win, "System32", "SBIGUDrv.dll"))
	add(filepath.Join(win, "SysWOW64", "SBIGUDrv.dll"))
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432"} {
		root := os.Getenv(env)
		if root == "" {
			continue
		}
		for _, pat := range []string{
			`N.I.N.A*\*SBIG*.dll`, `N.I.N.A*\*\*SBIG*.dll`, `N.I.N.A*\*\*\*SBIG*.dll`, `N.I.N.A*\*\*\*\*SBIG*.dll`,
			`SBIG\*\SBIGUDrv.dll`, `SBIG\*\*\SBIGUDrv.dll`, `SBIG\SBIGUDrv.dll`,
			`Diffraction Limited\*\SBIGUDrv.dll`, `Diffraction Limited\*\*\SBIGUDrv.dll`,
			`Common Files\ASCOM\Camera\*SBIG*\SBIGUDrv.dll`,
		} {
			m, _ := filepath.Glob(filepath.Join(root, pat))
			for _, p := range m {
				if strings.HasSuffix(strings.ToLower(p), ".dll") {
					add(p)
				}
			}
		}
	}
	return out
}

// peMachine reports whether a PE file is x86 or x64.
func peMachine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "?"
	}
	defer f.Close()
	hdr := make([]byte, 64)
	if _, err := f.ReadAt(hdr, 0); err != nil || hdr[0] != 'M' || hdr[1] != 'Z' {
		return "not a PE file"
	}
	off := int64(le.Uint32(hdr[0x3C:]))
	pe := make([]byte, 6)
	if _, err := f.ReadAt(pe, off); err != nil || string(pe[:4]) != "PE\x00\x00" {
		return "bad PE header"
	}
	switch le.Uint16(pe[4:]) {
	case 0x14c:
		return "32-bit (x86)"
	case 0x8664:
		return "64-bit (x64)"
	case 0xAA64:
		return "ARM64"
	}
	return fmt.Sprintf("machine 0x%x", le.Uint16(pe[4:]))
}
