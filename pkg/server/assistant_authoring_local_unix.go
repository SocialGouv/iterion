//go:build linux || darwin

package server

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func renameAuthoringAt(from *os.File, fromName string, to *os.File, toName string) error {
	return renameDependencyAt(int(from.Fd()), fromName, int(to.Fd()), toName)
}

// replaceAuthoringAt renames fromName over toName inside dir, replacing it in
// one step — the one authoring rename that replaces (replaceFile).
func replaceAuthoringAt(dir *os.File, fromName, toName string) error {
	return unix.Renameat(int(dir.Fd()), fromName, int(dir.Fd()), toName)
}
func requireAuthoringSameFilesystem(a, b *os.File) error {
	var left, right unix.Stat_t
	if err := unix.Fstat(int(a.Fd()), &left); err != nil {
		return err
	}
	if err := unix.Fstat(int(b.Fd()), &right); err != nil {
		return err
	}
	if left.Dev != right.Dev {
		return fmt.Errorf("authoring recovery storage must share the destination filesystem: %w", unix.EXDEV)
	}
	return nil
}

// authoringFileLinks is how many names the open file has (its link count).
func authoringFileLinks(f *os.File) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return uint64(st.Nlink), nil //nolint:unconvert // Nlink is uint16 on darwin, uint64 on linux
}
