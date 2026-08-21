//go:build !unix

package sessionpack

import "os"

func isHardlink(os.FileInfo) bool { return false }
