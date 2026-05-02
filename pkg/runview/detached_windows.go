//go:build windows

package runview

import "syscall"

// newDetachedSysProcAttr is the Windows fallback. iterion does not
// currently target Windows for the editor (CI builds only linux+darwin
// amd64/arm64) — the file exists so a future port has a single hook
// to fill in.
func newDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: 0x00000200, // CREATE_NEW_PROCESS_GROUP
	}
}
