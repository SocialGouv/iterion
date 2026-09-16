package cli

import (
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path through a temporary file beside it,
// synced to disk and renamed into place, the directory synced after: a
// crash, an interrupt or a power loss mid-write leaves the file as it was
// or whole, never truncated — what a rewrite of a whole tree owes each
// file. The mode is the one asked for; the temporary file never outlives
// an error. (The directory sync is best effort: a filesystem that refuses
// it has already made the rename durable, or cannot.)
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	// A symlinked file is written through the link, as os.WriteFile does:
	// the temporary file and the rename land beside the target, and the
	// link stays a link — a checkout that shares a fragment by symlink
	// keeps sharing it.
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
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
	if err := tmp.Sync(); err != nil {
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
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
