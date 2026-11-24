package delegate

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
	"github.com/SocialGouv/iterion/internal/pricingtest"
)

// hometest isolates per-user state. pricingtest prevents live price lookups
// from reading host caches or starting refreshes that outlive a test's HOME.
//
// The package is also hermetic to the host's anthropic-wire FACADE keys: an
// ambient ZAI_API_KEY/MOONSHOT_API_KEY reroutes a stand-in spawn onto the
// vendor facade by default precedence, where a claude id is refused as a
// substitution (#2772) before any argv/env is recorded. A test that exercises
// a facade route sets the key itself, per case.
func TestMain(m *testing.M) {
	os.Unsetenv("ZAI_API_KEY")
	os.Unsetenv("MOONSHOT_API_KEY")
	restore := pricingtest.Isolate()
	code := hometest.Isolate(m.Run)
	restore()
	os.Exit(code)
}
