package runview

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/proctest"
)

// A run with `worktree: auto` (the IR default) registers its per-run worktree
// in its workspace's repository, and nothing ever comes back for the
// registration once the workspace is gone. A Service launched without
// WithWorkDir does not hand the engine this package's own directory — under
// `go test` the engine refuses a cwd inside iterion's checkout (#2781) — so
// what reaches a real repository here is a workspace a test NAMED.
//
// The guard stays the backstop for those: it fails the package when the
// repository the tests run in gained a dead worktree entry, and names the
// test that left it.
func TestMain(m *testing.M) {
	os.Exit(proctest.NoProcessLeaks(func() int { return gittest.NoWorktreeLeaks(m) }))
}
