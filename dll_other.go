//go:build !windows

package main

import "errors"

// On non-Windows platforms only the simulator and FITS analysis are available
// (useful for development and CI). The real driver is a Windows DLL.
func loadDriverDLL(path string) (transport, error) {
	return nil, errors.New("the SBIG Windows driver can only be loaded on Windows (use -simulate or -analyze here)")
}

func candidateDLLs() []string   { return nil }
func fileVersion(string) string { return "n/a" }
func peMachine(string) string   { return "n/a" }
