//go:build !unix

package runstream

// inodeOf returns 0 on platforms whose stat exposes no inode; the rotation
// detector then leans on the size check alone.
func inodeOf(st interface{ Sys() any }) uint64 {
	return 0
}
