//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

// dllTransport calls SBIGUnivDrvCommand() in the vendor SBIGUDrv.dll.
// short __stdcall SBIGUnivDrvCommand(short command, void *Params, void *pResults);
// (On Win64 there is only one calling convention; Go's syscall layer handles
// stdcall on Win32.) All calls happen on the main OS thread (see main.go).
type dllTransport struct {
	dll   *syscall.DLL
	proc  *syscall.Proc
	path  string
	notes []string // preferred candidates that were skipped or failed
}

// knownDLLs identifies vendor builds by SHA-256 (verified copies from Diffraction
// Limited's downloads and NINA's installer).
var knownDLLs = map[string]string{
	"52d93d6a122967a3949fd7805b2c224813131f5011224aa6b7a114ba95aeaae1": "SBIGUDrv 4.99 Build 8 x64 (the copy NINA bundles in External\\x64\\SBIG)",
	"9c35deff654413dbc3ea04ea6ee6664c0e01684ccc78d8d9c702a6ae72990eab": "SBIGUDrv 5.0.2.0 x64 (DL download)",
	"c319e8246b864508ab9ea80de257c1200f6017d11159f76eb355edd1acc42fa8": "SBIGUDrv 5.1.1.0 x64 (DL download, newest)",
	"de0e84e27721411853052008a7ee05928de029e34d692793fe3313df8d182d24": "SBIGUDrv 5.0.2.0 x86 (Driver Checker 64)",
	"d3e29e82c3ee2345350c0f92306a7d62357cb8599e75f5a1230e89930bede404": "SBIGUDrv 4.99 Build 7 x86 (legacy bundle)",
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// processArch is the PE machine string our own process needs.
func processArch() string {
	if runtime.GOARCH == "386" {
		return "32-bit (x86)"
	}
	return "64-bit (x64)"
}

// loadDriverDLL loads an explicit -dll path, or picks the first SBIGUDrv.dll of the
// right bitness: for the 64-bit exe NINA's bundled copy (the very DLL NINA uses) and
// then System32; for the 32-bit exe the system copy that CCDOps uses. Loading by bare
// name is only the last resort: Windows may find the 32-bit copy in C:\Windows\System
// first and fail with "not a valid Win32 application" in a 64-bit process.
func loadDriverDLL(path string) (transport, error) {
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		return loadOneDLL(path)
	}
	want := processArch()
	var tried, notes []string
	for _, p := range preferredDLLs() {
		if m := peMachine(p); m != want {
			notes = append(notes, fmt.Sprintf("skipped %s (%s; this exe is %s)", p, m, want))
			continue
		}
		t, err := loadOneDLL(p)
		if err == nil {
			t.(*dllTransport).notes = notes
			return t, nil
		}
		tried = append(tried, err.Error())
		notes = append(notes, "failed "+err.Error())
	}
	t, err := loadOneDLL("SBIGUDrv.dll")
	if err == nil {
		t.(*dllTransport).notes = append(notes, "loaded by bare name through the Windows DLL search order")
		return t, nil
	}
	tried = append(tried, err.Error())
	return nil, fmt.Errorf("no usable %s SBIGUDrv.dll:\n  %s\n  The 64-bit exe needs a 64-bit DLL (NINA's External\\x64\\SBIG copy or System32),\n  the 32-bit exe the 32-bit DLL that CCDOps uses. Try the other exe, or -dll <path>.",
		want, strings.Join(tried, "\n  "))
}

func preferredDLLs() []string {
	all := candidateDLLs()
	rank := func(p string) int {
		l := strings.ToLower(p)
		switch {
		case strings.Contains(l, `n.i.n.a`) && runtime.GOARCH != "386":
			return 0
		case strings.Contains(l, `\system32\`) || strings.Contains(l, `\sysnative\`):
			return 1
		case strings.HasSuffix(filepath.Dir(l), `\system`):
			return 2
		case strings.Contains(l, `\syswow64\`):
			return 3
		}
		return 4
	}
	sort.SliceStable(all, func(i, j int) bool { return rank(all[i]) < rank(all[j]) })
	return all
}

var procSetDllDirectoryW = modKernel32.NewProc("SetDllDirectoryW")

func loadOneDLL(path string) (transport, error) {
	if strings.ContainsAny(path, `\/`) {
		// Let the DLL find vcruntime140.dll: next to it, or in NINA's install root.
		dir := filepath.Dir(path)
		for d, k := dir, 0; k < 5; d, k = filepath.Dir(d), k+1 {
			if _, err := os.Stat(filepath.Join(d, "vcruntime140.dll")); err == nil {
				dir = d
				break
			}
		}
		if p16, err := syscall.UTF16PtrFromString(dir); err == nil {
			procSetDllDirectoryW.Call(uintptr(unsafe.Pointer(p16)))
		}
	}
	d, err := syscall.LoadDLL(path)
	if err != nil {
		return nil, fmt.Errorf("cannot load %s: %v", path, err)
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

func (t *dllTransport) Call(cmd uint16, params, results []byte) uint16 {
	var p, r unsafe.Pointer
	if len(params) > 0 {
		p = unsafe.Pointer(&params[0])
	}
	if len(results) > 0 {
		r = unsafe.Pointer(&results[0])
	}
	// Pointer->uintptr conversions inline in the call (unsafe.Pointer rule 4), and keep
	// both buffers alive until the driver has returned.
	ret, _, _ := syscall.SyscallN(t.proc.Addr(), uintptr(cmd), uintptr(p), uintptr(r))
	runtime.KeepAlive(params)
	runtime.KeepAlive(results)
	return uint16(ret) // C 'short' return value: only the low 16 bits are defined
}

func (t *dllTransport) Close() error { return t.dll.Release() }

func (t *dllTransport) Describe() []string {
	var out []string
	for _, n := range t.notes {
		out = append(out, "Driver DLL note   : "+n)
	}
	sum := fileSHA256(t.path)
	known := knownDLLs[sum]
	if known == "" {
		known = "not one of the builds sbigdiag knows"
	}
	out = append(out,
		"Driver DLL loaded : "+t.path,
		"Driver DLL version: "+fileVersion(t.path),
		"Driver DLL arch   : "+peMachine(t.path),
		"Driver DLL sha256 : "+sum+" = "+known)
	if runtime.GOARCH == "amd64" && !strings.Contains(strings.ToLower(t.path), "n.i.n.a") {
		out = append(out,
			"!!! This 64-bit run did NOT use NINA's own SBIGUDrv.dll (External\\x64\\SBIG). For the README's decisive",
			"!!! test (step 3) point -dll at NINA's copy, e.g. -dll \"C:\\Program Files\\N.I.N.A. - Nighttime Imaging 'N' Astronomy\\External\\x64\\SBIG\\SBIGUDrv.dll\"")
	}
	return out
}

var (
	modKernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGetModuleFileNameW      = modKernel32.NewProc("GetModuleFileNameW")
	modVersion                  = syscall.NewLazyDLL(systemDLLPath("version.dll"))
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
	add(filepath.Join(win, "System", "SBIGUDrv.dll")) // where Driver Checker puts the 32-bit DLL
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

// systemDLLPath returns the absolute System32 path of a system DLL, so that a DLL
// of the same name next to the exe (e.g. in Downloads) is never picked up.
func systemDLLPath(name string) string {
	buf := make([]uint16, 512)
	n, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemDirectoryW").Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || int(n) >= len(buf) {
		return name
	}
	return syscall.UTF16ToString(buf[:n]) + `\` + name
}
