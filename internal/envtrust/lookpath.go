package envtrust

import (
	"os"
	"os/exec"
	"strings"
)

// LookPath is exec.LookPath that never resolves against a PATH a project
// `.env` planted. Which executable a launcher-started process runs is an
// authority question (#1945 closed it one step earlier, at the config), and
// the live PATH answers it with the repository's voice when applyDotEnv
// filled it: a `.env` can only plant names that were UNSET, so a planted
// PATH means the operator's process had none — and nothing resolves. A file
// that already carries a path separator consults no PATH and resolves
// exactly as exec.LookPath.
//
// The refusal is loud (exec.ErrNotFound, as a miss on an empty PATH would
// be): the caller reports a missing binary, never runs the repository's.
func LookPath(file string) (string, error) {
	if !Planted("PATH") || namesAPath(file) {
		return exec.LookPath(file)
	}
	return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
}

// namesAPath reports whether file consults no PATH at all — the same
// separator set exec.LookPath itself uses on the platform: os.PathSeparator,
// plus '/' on Windows, where the syscall layer honours both.
func namesAPath(file string) bool {
	if strings.ContainsRune(file, os.PathSeparator) {
		return true
	}
	return goos == "windows" && strings.ContainsRune(file, '/')
}
