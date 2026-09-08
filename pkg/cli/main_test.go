package cli_test

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/proctest"
)

// `iterion run` takes its workspace from the process cwd — this package's own
// directory when a test does not say otherwise — so a run with
// `worktree: auto` (the IR default) registers its per-run worktree in the
// developer's checkout, while the checkout itself lives under the test's
// t.TempDir() store and is deleted on return.
//
// The guard fails the package when the registry the tests run in gained such
// an entry, and names the test that left it.
func TestMain(m *testing.M) {
	os.Exit(proctest.NoProcessLeaks(func() int { return gittest.NoWorktreeLeaks(m) }))
}
