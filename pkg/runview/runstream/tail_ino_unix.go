//go:build unix

package runstream

import "syscall"

// inodeOf returns the file's inode, 0 when the platform stat does not
// expose one. Unix only; the rotation detector treats 0 as "unknown" and
// leans on the size check alone.
func inodeOf(st interface{ Sys() any }) uint64 {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys == nil {
		return 0
	}
	return sys.Ino
}
