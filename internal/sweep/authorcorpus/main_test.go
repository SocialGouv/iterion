package authorcorpus

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/detecttest"
	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/internal/hometest"
	"github.com/SocialGouv/iterion/internal/pricingtest"
	"github.com/SocialGouv/iterion/internal/proctest"
)

// TestMain installs the isolation pkg/cli's own TestMain installs: this test
// binary links the CLI, its model specs and its pricing tier, so it runs
// with no host provider, the iterion home off the operator's, and the guards
// against leaked processes and leaked worktrees.
func TestMain(m *testing.M) {
	pricingtest.Isolate() // for the rest of the process: no restore before os.Exit
	restoreProviders := detecttest.IsolateProviders()
	code := hometest.Isolate(func() int {
		return proctest.NoProcessLeaks(func() int { return gittest.NoWorktreeLeaks(m) })
	})
	restoreProviders()
	os.Exit(code)
}
