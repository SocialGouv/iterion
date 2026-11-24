package cli_test

import (
	"github.com/SocialGouv/iterion/internal/detecttest"
	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/hometest"
	"github.com/SocialGouv/iterion/internal/pricingtest"
	"github.com/SocialGouv/iterion/internal/proctest"
	"os"
	"testing"
)

// `iterion run` takes its workspace from the process cwd, so a run with
// `worktree: auto` (the IR default) registers its per-run worktree in that
// workspace's repository while the store holding it lives under the test's
// t.TempDir() and is deleted on return. Under `go test` the engine refuses a
// cwd inside iterion's own checkout (#2781), which leaves the workspaces a
// test named — a `t.Chdir` fixture, a `WithWorkDir` — reaching a repository.
//
// The guard fails the package when the registry the tests run in gained such
// an entry, and names the test that left it. The runs those tests launch
// resolve their stores from the iterion home, which hometest keeps off the
// operator's.
func TestMain(m *testing.M) {
	pricingtest.Isolate() // for the rest of the process: no restore before os.Exit
	restoreProviders := detecttest.IsolateProviders()
	code := hometest.Isolate(func() int {
		return proctest.NoProcessLeaks(func() int { return gittest.NoWorktreeLeaks(m) })
	})
	restoreProviders()
	os.Exit(code)
}

// The provider isolation TestMain installs, witnessed: a fixture whose answer
// falls to the last-resort extraction must not reach a host provider.
func TestThisPackageDetectsNoProvider(t *testing.T) { detecttest.RequireNoProvider(t) }
