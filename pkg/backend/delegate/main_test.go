package delegate

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
)

// The model-specs cache and the per-user state these backends resolve land in the iterion home; hometest keeps it off the operator's and removes it at exit.
func TestMain(m *testing.M) { os.Exit(hometest.Isolate(m.Run)) }
