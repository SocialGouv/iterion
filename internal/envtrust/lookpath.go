package envtrust

import (
	"os"
	"os/exec"
	"strings"
)

// LookPath is exec.LookPath that never resolves against a PATH a project
// `.env` planted. Which executable a launcher-started process runs is an
// authority question (#1945 closed it one step earlier, at the config), and
// the live PATH answers it with the repository's voice whenever the PATH is
// planted — the value speaks for the file, never for the operator, on BOTH
// planting lanes:
//
//   - CLI lane (cmd/iterion applyDotEnv): the file fills only names that
//     were UNSET, so a planted PATH means the operator's process had none —
//     refusing loses no answer the operator gave.
//   - Studio lane (pkg/projectenv.Snapshot): the file's lines REPLACE the
//     inherited values in the child's environment, and PATH is the one
//     execution-grade name the operator's opt-in
//     (ITERION_DOTENV_ENDPOINTS=allow) re-opens — the legitimate
//     `PATH=$PATH:…` extension. A planted PATH there can sit on top of an
//     operator who HAD one; it is still the file's answer, expanded by the
//     loader, and no reader can separate the operator's share from the
//     repository's — so the refusal stands there too, and the operator's
//     channel is the same on both lanes: their own environment (a PATH the
//     operator exported and no file touched is not planted).
//
// The refusal is loud (exec.ErrNotFound, as a miss on an empty PATH would
// be): the caller reports a missing binary, never runs the repository's. A
// file that already carries a path separator consults no PATH and resolves
// exactly as exec.LookPath.
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
