//go:build !windows

package main

import (
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
