//go:build windows

package jsonl

import "os"

// Advisory flock is a Unix concept; on Windows the OS-level exclusive
// write handle already prevents interleaving between processes for the
// append pattern used here, so locking is a no-op.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) {}
