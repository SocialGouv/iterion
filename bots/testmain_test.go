package bots

import (
	"os"
	"testing"
)

// The git-exec tests in this package (dep-update-guard, branch-improve
// gate, campaign gate commands, docs-refresh scope…) create throwaway
// repos and commit in them. The operator's global/system git config must
// not leak into those repos: commit.gpgsign=true hangs every test commit
// on a pinentry that has no TTY, which reads as a 600s package timeout
// with no failing test named. CI never sees it (runners sign nothing) —
// only contributors do.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Exit(m.Run())
}
