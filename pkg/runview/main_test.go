package runview

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/proctest"
)

// A Service launched without WithWorkDir hands the engine os.Getwd() — this
// package's own directory, inside the developer's checkout — so a run with
// `worktree: auto` (the IR default) registers its per-run worktree in the REAL
// repository while the checkout itself lives under t.TempDir() and is deleted
// on return. Nothing ever comes back for the registration.
//
// The guard makes that visible: it fails the package when the repository the
// tests run in gained a dead worktree entry, and names the test that left it.
func TestMain(m *testing.M) {
	os.Exit(proctest.NoProcessLeaks(func() int { return gittest.NoWorktreeLeaks(m) }))
}
