//go:build windows

package cli

import (
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
)

func isTerminal(fd int) bool {
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(fd), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}
