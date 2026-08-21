//go:build unix

package sessionpack

import (
	"os"
	"syscall"
)

func isHardlink(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}
