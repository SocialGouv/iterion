//go:build linux

package config

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func setNonDumpable() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("config: prctl(PR_SET_DUMPABLE, 0): %w", err)
	}
	return nil
}
