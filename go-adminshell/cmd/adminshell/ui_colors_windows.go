//go:build windows

package main

import (
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

const enableVirtualTerminalProcessing = 0x0004

var (
	vtOnce    sync.Once
	vtEnabled bool
)

func enableVirtualTerminalSequences() bool {
	vtOnce.Do(func() {
		stdoutEnabled := enableVTOnHandle(windows.Handle(os.Stdout.Fd()))
		stderrEnabled := enableVTOnHandle(windows.Handle(os.Stderr.Fd()))
		vtEnabled = stdoutEnabled || stderrEnabled
	})
	return vtEnabled
}

func enableVTOnHandle(handle windows.Handle) bool {
	if handle == windows.InvalidHandle {
		return false
	}
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	if err := windows.SetConsoleMode(handle, mode|enableVirtualTerminalProcessing); err != nil {
		return false
	}
	return true
}
