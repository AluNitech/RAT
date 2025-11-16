//go:build !windows

package main

func enableVirtualTerminalSequences() bool {
	return true
}
