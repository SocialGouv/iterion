//go:build !windows

package store

import "syscall"

func runLogPlatformIO(syscall.Errno) bool { return false }
