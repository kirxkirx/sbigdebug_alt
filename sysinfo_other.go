//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
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

func preventSleep() bool { return false }
func allowSleep()        {}

func freeBytes(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true
}

func stdinIsConsole() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
