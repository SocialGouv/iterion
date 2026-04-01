//go:build linux

package cli

import (
	"syscall"
	"unsafe"
)

func isTerminal(fd int) bool {
	var termios syscall.Termios
	_, _, err := syscall.Syscall6(
		syscall.SYS_IOCTL,
		uintptr(fd),
		syscall.TCGETS,
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)
	return err == 0
}
