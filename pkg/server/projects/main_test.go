package projects

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
)

// Under go test the registry's config dir is the test process's own
// directory; hometest removes it at exit.
func TestMain(m *testing.M) { os.Exit(hometest.Isolate(m.Run)) }
