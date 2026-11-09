//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"runtime"
)

func sysInfo(out io.Writer) {
	fmt.Fprintf(out, "\n== Computer ==\n%s/%s (non-Windows: simulator and file analysis only)\n", runtime.GOOS, runtime.GOARCH)
}

func disableQuickEdit() {}
func ownsConsole() bool { return false }

type RegSnapshot map[string]struct{}

func snapshotSBIGRegistry() RegSnapshot               { return RegSnapshot{} }
func restoreSBIGRegistry(before RegSnapshot) []string { return nil }

func flushConsoleInput()               {}
func ansiPath(p string) (string, bool) { return p, true }
func setDebugLogRegistry(string, [4]uint32) error {
	return errors.New("only on Windows")
}
