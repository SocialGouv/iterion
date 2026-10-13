package server

import (
	"context"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
)

// The servers these tests start resolve their run stores, installed plugins
// and the global runs view from the iterion home; hometest keeps them off
// the operator's. HOME is the binary's own too: a schedule guard runs
// `sh -lc` and a run shell `$SHELL -l`, login shells that read their profile
// from $HOME (#2231).
func TestMain(m *testing.M) {
	os.Exit(hometest.IsolateUserHome(func() int { return hometest.Isolate(m.Run) }))
}

// shutdownOnCleanup stops every background loop srv started — the assistant
// watch and mission sweep included — before the test's temporary directories
// are removed: a loop left running re-creates the per-project store under a
// removed ITERION_HOME, and the directory outlives the test.
func shutdownOnCleanup(t *testing.T, srv *Server) {
	t.Helper()
	t.Cleanup(func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			t.Errorf("shut the test server down: %v", err)
		}
	})
}
