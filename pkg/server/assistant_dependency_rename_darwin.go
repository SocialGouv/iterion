package server

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func renameDependencyAt(fromFD int, from string, toFD int, to string) error {
	return unix.RenameatxNp(fromFD, from, toFD, to, unix.RENAME_EXCL)
}

func requireDependencySameFilesystem(a, b string) error {
	var left, right unix.Stat_t
	if err := unix.Stat(a, &left); err != nil {
		return err
	}
	if err := unix.Stat(b, &right); err != nil {
		return err
	}
	if left.Dev != right.Dev {
		return fmt.Errorf("dependency recovery storage must share the install filesystem: %w", unix.EXDEV)
	}
	return nil
}
