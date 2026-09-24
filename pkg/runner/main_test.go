package runner

import (
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
	code := proctest.NoProcessLeaks(m.Run)
	restore()
	os.Exit(code)
}

// The isolation TestMain installs, witnessed: a live price on the host must not
// reach this package's estimates.
func TestThisPackagePricesFromTheStaticTable(t *testing.T) { pricingtest.RequireIsolated(t) }
