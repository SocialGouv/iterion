package modelcatalog

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
)

// The model-specs cache this catalog fetches lands in the iterion home; hometest keeps it off the operator's and removes it at exit.
func TestMain(m *testing.M) { os.Exit(hometest.Isolate(m.Run)) }
