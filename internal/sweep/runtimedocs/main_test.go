package runtimedocs

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/detecttest"
	"github.com/SocialGouv/iterion/internal/hometest"
	"github.com/SocialGouv/iterion/internal/pricingtest"
	"github.com/SocialGouv/iterion/internal/proctest"
)

// This package imports pkg/runtime, whose engines run real tool nodes: shells
// must die with the test that started them, plugins and scratch resolve from
// the iterion home, which hometest keeps off the operator's; and no fixture
// may reach a host provider. This is pkg/runtime's TestMain without its
// throw-away-workdir ledger, which is state internal to that package.
func TestMain(m *testing.M) {
	pricingtest.Isolate() // for the rest of the process: no restore before os.Exit
	restoreProviders := detecttest.IsolateProviders()
	code := hometest.Isolate(func() int { return proctest.NoProcessLeaks(m.Run) })
	restoreProviders()
	os.Exit(code)
}
