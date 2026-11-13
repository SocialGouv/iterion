package testutil

import "testing"

// IsolateShellHome keeps shell-output tests independent of the operator's
// startup files. It does not change the production shell or filter its output.
// Like Setenv, it must not be called from parallel tests.
func IsolateShellHome(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("ENV", "")
	t.Setenv("BASH_ENV", "")
	return dir
}
