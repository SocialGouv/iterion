package cli

import (
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path through a temporary file beside it,
// renamed into place: a crash or an interrupt mid-write leaves the file as
// it was, never truncated — what a rewrite of a whole tree owes each file.
// The mode is the one asked for; the temporary file never outlives an error.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
