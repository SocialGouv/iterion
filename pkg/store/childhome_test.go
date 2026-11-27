package store

import "testing"

// childHomeEnv is the ITERION_HOME entry for a test binary that re-executes
// itself: a fresh directory t owns, appended after the inherited environment.
// The child's TestMain (hometest.Isolate) reads the ITERION_HOME it inherits
// as the operator's real home and fails its run when a test-key store
// appears there; inheriting the parent's isolated home, it would report the
// store a parallel test of the parent creates in that home. It is
// hometest.ChildHomeEnv, restated: hometest imports this package, so the
// package's internal tests cannot import it.
func childHomeEnv(t testing.TB) string {
	t.Helper()
	return "ITERION_HOME=" + t.TempDir()
}
