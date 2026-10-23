package store

import "os"

// RenameNoReplace renames from to to — a file or a directory — and replaces
// nothing: a name that exists at to, whatever it is (an empty directory
// included), makes it fail with an error that is fs.ErrExist, from left as
// it was. It is how a writer publishes something it built under a name of
// its own (a staging directory) without ever taking a name another writer
// created meanwhile. from and to must lie on one filesystem.
func RenameNoReplace(from, to string) error { return renameNoReplace(from, to) }

// wrapRenameErr names the rename an error came from; nil stays nil.
func wrapRenameErr(from, to string, err error) error {
	if err == nil {
		return nil
	}
	return &os.LinkError{Op: "rename (no replace)", Old: from, New: to, Err: err}
}
