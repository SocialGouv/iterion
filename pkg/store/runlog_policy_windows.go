//go:build windows

package store

import "syscall"

// Windows filesystem calls return Win32 error numbers, not the invented POSIX
// errno constants. Keep this allowlist limited to equivalents of the IO classes
// already accepted by runLogIOLeaves. Values match the Win32 definitions in
// golang.org/x/sys/windows/zerrors_windows.go; no extra dependency is needed.
func runLogPlatformIO(errno syscall.Errno) bool {
	const (
		errorTooManyOpenFiles  syscall.Errno = 4    // EMFILE / ENFILE
		errorWriteProtect      syscall.Errno = 19   // EROFS
		errorWriteFault        syscall.Errno = 29   // EIO
		errorReadFault         syscall.Errno = 30   // EIO
		errorSharingViolation  syscall.Errno = 32   // EBUSY
		errorLockViolation     syscall.Errno = 33   // EBUSY
		errorHandleDiskFull    syscall.Errno = 39   // ENOSPC
		errorDiskFull          syscall.Errno = 112  // ENOSPC
		errorIODevice          syscall.Errno = 1117 // EIO
		errorDiskQuotaExceeded syscall.Errno = 1295 // EDQUOT
	)
	switch errno {
	case errorTooManyOpenFiles, errorWriteProtect, errorWriteFault, errorReadFault,
		errorSharingViolation, errorLockViolation, errorHandleDiskFull, errorDiskFull,
		errorIODevice, errorDiskQuotaExceeded:
		return true
	default:
		return false
	}
}
