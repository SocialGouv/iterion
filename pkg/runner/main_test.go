package runner

import (
	"github.com/SocialGouv/iterion/internal/detecttest"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/pricingtest"
	"github.com/SocialGouv/iterion/internal/proctest"
)

// The fake shared sandbox here runs subbot children as real host commands.
// A fake whose lifetime contract diverges from the real driver's leaves them
// running; the guard fails the package instead of exporting the leak to
// whatever runs next. Live model prices are isolated (pricingtest.Isolate):
// a spend assertion reads the static price table, not what the host last
// fetched.
func TestMain(m *testing.M) {
	restore := pricingtest.Isolate()
	restoreProviders := detecttest.IsolateProviders()
	code := proctest.NoProcessLeaks(m.Run)
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
