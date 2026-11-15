//go:build windows

package store

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A reader never creates/removes a PID entry. Handles permit normal writer
// reads, writes and deletion; a read must not make writer Unlock fail.
func tryStreamReadLock(path, label string) (RunLock, streamReadLockOutcome, error) {
	before, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, streamReadLockQuiescent, nil
	}
	if e != nil {
		return nil, streamReadLockFailure, e
	}
	if !before.Mode().IsRegular() {
		return nil, streamReadLockFailure, ErrRunDataIdentityChanged
	}
	nativePath, e := streamWindowsReadPath(path)
	if e != nil {
		return nil, streamReadLockFailure, e
	}
	ptr, e := windows.UTF16PtrFromString(nativePath)
	if e != nil {
		return nil, streamReadLockFailure, e
	}
	handle, e := windows.CreateFile(ptr, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if errors.Is(e, windows.ERROR_FILE_NOT_FOUND) || errors.Is(e, windows.ERROR_PATH_NOT_FOUND) {
		return nil, streamReadLockQuiescent, nil
	}
	if e != nil {
		return nil, streamReadLockFailure, e
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		windows.CloseHandle(handle)
		return nil, streamReadLockFailure, ErrRunDataCapability
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, streamReadLockFailure, e
	}
	if !info.Mode().IsRegular() {
		return nil, streamReadLockFailure, ErrRunDataIdentityChanged
	}
	if !os.SameFile(before, info) {
		return nil, streamReadLockBusy, fmt.Errorf("store: %s PID entry changed", label)
	}
	// Current writers emit one decimal uint32 PID without newline. Eleven
	// bytes distinguish a complete maximum-width record from an oversized one.
	raw, e := io.ReadAll(io.LimitReader(f, 11))
	if e != nil {
		return nil, streamReadLockFailure, e
	}
	if len(raw) == 0 {
		return nil, streamReadLockBusy, fmt.Errorf("store: %s PID publication pending", label)
	}
	if len(raw) > 10 {
		return nil, streamReadLockFailure, ErrInvalidRunDataIdentity
	}
	for _, b := range raw {
		if b < '0' || b > '9' {
			return nil, streamReadLockFailure, ErrInvalidRunDataIdentity
		}
	}
	pid, e := strconv.ParseUint(string(raw), 10, 32)
	if e != nil || pid == 0 {
		return nil, streamReadLockFailure, errors.Join(ErrInvalidRunDataIdentity, e)
	}
	current, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, streamReadLockQuiescent, nil
	}
	if e != nil {
		return nil, streamReadLockFailure, e
	}
	if !current.Mode().IsRegular() {
		return nil, streamReadLockFailure, ErrRunDataIdentityChanged
	}
	if !os.SameFile(info, current) {
		return nil, streamReadLockBusy, fmt.Errorf("store: %s PID entry changed", label)
	}
	if pidAliveWindows(uint32(pid)) {
		return nil, streamReadLockBusy, fmt.Errorf("store: %s writer active", label)
	}
	return nil, streamReadLockQuiescent, nil
}

// Match Go's long-path support when calling the native sharing-aware opener.
// The input is the already captured canonical store path, not a fresh alias.
func streamWindowsReadPath(path string) (string, error) {
	if strings.HasPrefix(path, `\\?\`) {
		return path, nil
	}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return "", e
	}
	if strings.HasPrefix(absolute, `\\`) {
		return `\\?\UNC\` + absolute[2:], nil
	}
	return `\\?\` + absolute, nil
}
