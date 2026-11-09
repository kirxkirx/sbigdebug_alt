//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var (
	modAdvapi32               = syscall.NewLazyDLL("advapi32.dll")
	procRegEnumValueW         = modAdvapi32.NewProc("RegEnumValueW")
	procGetConsoleProcessList = modKernel32.NewProc("GetConsoleProcessList")
	procGetStdHandle          = modKernel32.NewProc("GetStdHandle")
	procGetConsoleMode        = modKernel32.NewProc("GetConsoleMode")
	procSetConsoleMode        = modKernel32.NewProc("SetConsoleMode")
	procIsWow64Process        = modKernel32.NewProc("IsWow64Process")
)

const keyWow6464 = 0x0100

func regOpen(root syscall.Handle, path string) (syscall.Handle, bool) {
	var h syscall.Handle
	p, _ := syscall.UTF16PtrFromString(path)
	if syscall.RegOpenKeyEx(root, p, 0, syscall.KEY_READ|keyWow6464, &h) != nil {
		return 0, false
	}
	return h, true
}

func regValue(h syscall.Handle, name string) (string, bool) {
	n, _ := syscall.UTF16PtrFromString(name)
	var typ, size uint32
	if syscall.RegQueryValueEx(h, n, nil, &typ, nil, &size) != nil || size == 0 {
		return "", false
	}
	buf := make([]byte, size+2)
	if syscall.RegQueryValueEx(h, n, nil, &typ, &buf[0], &size) != nil {
		return "", false
	}
	return formatRegValue(typ, buf[:size]), true
}

func formatRegValue(typ uint32, b []byte) string {
	switch typ {
	case syscall.REG_SZ, syscall.REG_EXPAND_SZ, 7: // 7 = REG_MULTI_SZ
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		parts := []string{}
		cur := []uint16{}
		for _, c := range u {
			if c == 0 {
				if len(cur) > 0 {
					parts = append(parts, syscall.UTF16ToString(cur))
				}
				cur = cur[:0]
				continue
			}
			cur = append(cur, c)
		}
		if len(cur) > 0 {
			parts = append(parts, syscall.UTF16ToString(cur))
		}
		return strings.Join(parts, " | ")
	case syscall.REG_DWORD:
		if len(b) >= 4 {
			return fmt.Sprintf("%d", le.Uint32(b))
		}
	case 11: // REG_QWORD
		if len(b) >= 8 {
			return fmt.Sprintf("%d", le.Uint64(b))
		}
	}
	return fmt.Sprintf("% x", b)
}

func regSubkeys(h syscall.Handle) []string {
	var out []string
	for i := uint32(0); ; i++ {
		buf := make([]uint16, 512)
		n := uint32(len(buf))
		if syscall.RegEnumKeyEx(h, i, &buf[0], &n, nil, nil, nil, nil) != nil {
			break
		}
		out = append(out, syscall.UTF16ToString(buf[:n]))
	}
	return out
}

func regValues(h syscall.Handle) [][2]string {
	var out [][2]string
	for i := uint32(0); i < 200; i++ {
		name := make([]uint16, 512)
		nlen := uint32(len(name))
		data := make([]byte, 4096)
		dlen := uint32(len(data))
		var typ uint32
		r, _, _ := procRegEnumValueW.Call(uintptr(h), uintptr(i), uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&nlen)),
			0, uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&dlen)))
		if r != 0 {
			break
		}
		out = append(out, [2]string{syscall.UTF16ToString(name[:nlen]), formatRegValue(typ, data[:dlen])})
	}
	return out
}

func dumpRegTree(out io.Writer, root syscall.Handle, path, indent string, depth int) {
	h, ok := regOpen(root, path)
	if !ok {
		return
	}
	defer syscall.RegCloseKey(h)
	for _, kv := range regValues(h) {
		fmt.Fprintf(out, "%s%s = %s\n", indent, kv[0], kv[1])
	}
	if depth <= 0 {
		return
	}
	for _, sk := range regSubkeys(h) {
		fmt.Fprintf(out, "%s[%s]\n", indent, sk)
		dumpRegTree(out, root, path+`\`+sk, indent+"  ", depth-1)
	}
}

func sysInfo(out io.Writer) {
	fmt.Fprintln(out, "\n== Computer ==")
	if h, ok := regOpen(syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`); ok {
		pn, _ := regValue(h, "ProductName")
		dv, _ := regValue(h, "DisplayVersion")
		cb, _ := regValue(h, "CurrentBuild")
		ubr, _ := regValue(h, "UBR")
		syscall.RegCloseKey(h)
		win := ""
		var b int
		fmt.Sscanf(cb, "%d", &b)
		if b >= 22000 {
			win = " (= Windows 11; ProductName may still say Windows 10)"
		}
		fmt.Fprintf(out, "Windows: %s %s build %s.%s%s\n", pn, dv, cb, ubr, win)
	}
	var wow int32
	procIsWow64Process.Call(uintptr(^uintptr(0)), uintptr(unsafe.Pointer(&wow)))
	fmt.Fprintf(out, "This program: %s build, WOW64=%v\n", runtime.GOARCH, wow != 0)
	if h, ok := regOpen(syscall.HKEY_LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\DeviceGuard\Scenarios\HypervisorEnforcedCodeIntegrity`); ok {
		v, _ := regValue(h, "Enabled")
		syscall.RegCloseKey(h)
		fmt.Fprintf(out, "Memory integrity (HVCI) Enabled=%s\n", v)
	}
	if h, ok := regOpen(syscall.HKEY_LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\CI\Config`); ok {
		v, _ := regValue(h, "VulnerableDriverBlocklistEnable")
		syscall.RegCloseKey(h)
		fmt.Fprintf(out, "Vulnerable driver blocklist VulnerableDriverBlocklistEnable=%s\n", v)
	}
	fmt.Fprintln(out, "SBIGUDrv.dll copies on this PC:")
	for _, p := range candidateDLLs() {
		fmt.Fprintf(out, "  %s\n      %s, %s\n", p, peMachine(p), fileVersion(p))
	}
	win := os.Getenv("SystemRoot")
	if win == "" {
		win = `C:\Windows`
	}
	drv := filepath.Join(win, "Sysnative", "drivers")
	if _, err := os.Stat(drv); err != nil {
		drv = filepath.Join(win, "System32", "drivers")
	}
	fmt.Fprintln(out, "Kernel drivers (SBIG / USBIO):")
	for _, pat := range []string{"sbig*.sys", "usbio*.sys", "SBIG*.SYS"} {
		m, _ := filepath.Glob(filepath.Join(drv, pat))
		for _, p := range m {
			fmt.Fprintf(out, "  %s: %s\n", p, fileVersion(p))
		}
	}
	// SBIG USB devices known to Windows (vendor ID 0x0D97).
	if h, ok := regOpen(syscall.HKEY_LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\USB`); ok {
		for _, k := range regSubkeys(h) {
			if !strings.HasPrefix(strings.ToUpper(k), "VID_0D97") {
				continue
			}
			if hk, ok := regOpen(syscall.HKEY_LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\USB\`+k); ok {
				for _, inst := range regSubkeys(hk) {
					if hi, ok := regOpen(syscall.HKEY_LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\USB\`+k+`\`+inst); ok {
						desc, _ := regValue(hi, "DeviceDesc")
						svc, _ := regValue(hi, "Service")
						drvk, _ := regValue(hi, "Driver")
						fmt.Fprintf(out, "USB device %s\\%s: %s, service=%s, driver=%s\n", k, inst, desc, svc, drvk)
						syscall.RegCloseKey(hi)
					}
				}
				syscall.RegCloseKey(hk)
			}
		}
		syscall.RegCloseKey(h)
	}
	fmt.Fprintln(out, `Registry HKCU\Software\SBIG:`)
	dumpRegTree(out, syscall.HKEY_CURRENT_USER, `Software\SBIG`, "  ", 3)
	fmt.Fprintln(out, `Registry HKLM\SOFTWARE\SBIG:`)
	dumpRegTree(out, syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\SBIG`, "  ", 2)
}

func disableQuickEdit() {
	h, _, _ := procGetStdHandle.Call(uintptr(^uint32(10 - 1))) // STD_INPUT_HANDLE = -10
	if h == 0 || h == ^uintptr(0) {
		return
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return
	}
	procSetConsoleMode.Call(h, uintptr((mode&^0x0040)|0x0080)) // clear ENABLE_QUICK_EDIT_MODE, set EXTENDED_FLAGS
}

// ownsConsole is true when the program was started by double-click (it is the only
// process attached to its console), so the window would vanish at exit.
func ownsConsole() bool {
	var ids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), 4)
	return n == 1
}

// --- SBIG registry snapshot / restore (HKCU\Software\SBIG) ---

var (
	procRegSetValueExW = modAdvapi32.NewProc("RegSetValueExW")
	procRegDeleteValue = modAdvapi32.NewProc("RegDeleteValueW")
)

type regVal struct {
	typ  uint32
	data []byte
}

// RegSnapshot records every value under HKCU\Software\SBIG (the SBIG driver keeps
// its user settings there) so that anything the driver writes during our run can
// be put back.
type RegSnapshot map[string]regVal // key: subkey + "\x00" + value name

func snapshotSBIGRegistry() RegSnapshot {
	snap := RegSnapshot{}
	var walk func(path string, depth int)
	walk = func(path string, depth int) {
		h, ok := regOpen(syscall.HKEY_CURRENT_USER, path)
		if !ok {
			return
		}
		for i := uint32(0); i < 500; i++ {
			name := make([]uint16, 512)
			nlen := uint32(len(name))
			data := make([]byte, 8192)
			dlen := uint32(len(data))
			var typ uint32
			r, _, _ := procRegEnumValueW.Call(uintptr(h), uintptr(i), uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&nlen)),
				0, uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&dlen)))
			if r != 0 {
				break
			}
			snap[path+"\x00"+syscall.UTF16ToString(name[:nlen])] = regVal{typ, append([]byte(nil), data[:dlen]...)}
		}
		subs := regSubkeys(h)
		syscall.RegCloseKey(h)
		if depth > 0 {
			for _, sk := range subs {
				walk(path+`\`+sk, depth-1)
			}
		}
	}
	walk(`Software\SBIG`, 6)
	return snap
}

// restoreSBIGRegistry undoes value changes made since 'before'. Returns a report.
func restoreSBIGRegistry(before RegSnapshot) []string {
	after := snapshotSBIGRegistry()
	var out []string
	for k, v := range after {
		old, existed := before[k]
		if existed && old.typ == v.typ && string(old.data) == string(v.data) {
			continue
		}
		path, name, _ := strings.Cut(k, "\x00")
		var h syscall.Handle
		p, _ := syscall.UTF16PtrFromString(path)
		if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, p, 0, syscall.KEY_SET_VALUE|syscall.KEY_QUERY_VALUE, &h) != nil {
			out = append(out, fmt.Sprintf("HKCU\\%s\\%s changed but cannot be opened for restore", path, name))
			continue
		}
		n, _ := syscall.UTF16PtrFromString(name)
		var r uintptr
		if existed {
			var dp uintptr
			if len(old.data) > 0 {
				dp = uintptr(unsafe.Pointer(&old.data[0]))
			}
			r, _, _ = procRegSetValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(n)), 0, uintptr(old.typ), dp, uintptr(len(old.data)))
			out = append(out, fmt.Sprintf("HKCU\\%s\\%s was changed during the run -> restored (rc=%d)", path, name, r))
		} else {
			r, _, _ = procRegDeleteValue.Call(uintptr(h), uintptr(unsafe.Pointer(n)))
			out = append(out, fmt.Sprintf("HKCU\\%s\\%s was created during the run -> removed (rc=%d)", path, name, r))
		}
		syscall.RegCloseKey(h)
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			path, name, _ := strings.Cut(k, "\x00")
			out = append(out, fmt.Sprintf("HKCU\\%s\\%s disappeared during the run (not recreated)", path, name))
		}
	}
	return out
}
