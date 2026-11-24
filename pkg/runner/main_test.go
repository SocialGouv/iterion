package runner

import (
	"github.com/SocialGouv/iterion/internal/detecttest"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/pricingtest"
	"github.com/SocialGouv/iterion/internal/proctest"
	"github.com/SocialGouv/iterion/pkg/backend/model"
)

// The fake shared sandbox here runs subbot children as real host commands.
// A fake whose lifetime contract diverges from the real driver's leaves them
// running; the guard fails the package instead of exporting the leak to
// whatever runs next. Live model prices are isolated (pricingtest.Isolate):
// a spend assertion reads the static price table, not what the host last
// fetched. The run credentials lookups are the ones the runner installs at
// boot (cmd/iterion/runner.go): the resolver's claw arm, which the
// pre-flight asks, reads a run's keys and forfaits through them.
func TestMain(m *testing.M) {
	restore := pricingtest.Isolate()
	restoreProviders := detecttest.IsolateProviders()
	restoreCreds := model.SetCredentialsLookup(model.RunCredentialsLookup)
	restoreDirs := model.SetOAuthDirLookup(model.RunOAuthDirLookup)
	code := proctest.NoProcessLeaks(m.Run)
	restoreDirs()
	restoreCreds()
	restoreProviders()
	restore()
	os.Exit(code)
}

// The provider isolation TestMain installs, witnessed: a fixture whose answer
// falls to the last-resort extraction must not reach a host provider.
func TestThisPackageDetectsNoProvider(t *testing.T) { detecttest.RequireNoProvider(t) }

// The isolation TestMain installs, witnessed: a live price on the host must not
// reach this package's estimates.
func TestThisPackagePricesFromTheStaticTable(t *testing.T) { pricingtest.RequireIsolated(t) }
