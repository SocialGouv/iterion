package delegate

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
)

// The model-specs cache and the per-user state these backends resolve land in the iterion home; hometest keeps it off the operator's and removes it at exit.
//
// The package is also hermetic to the host's anthropic-wire FACADE keys: an
// ambient ZAI_API_KEY/MOONSHOT_API_KEY reroutes a stand-in spawn onto the
// vendor facade by default precedence, where a claude id is refused as a
// substitution (#2772) before any argv/env is recorded. A test that exercises
// a facade route sets the key itself, per case.
func TestMain(m *testing.M) {
	os.Unsetenv("ZAI_API_KEY")
	os.Unsetenv("MOONSHOT_API_KEY")
	os.Exit(hometest.Isolate(m.Run))
}
