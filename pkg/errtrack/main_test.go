package errtrack_test

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/pricingtest"
)

// The test binary links a live model-pricing tier: isolate it from the
// host's cache and the network (#3257). The external package carries it:
// pricingtest reaches pkg/store, which imports errtrack.
func TestMain(m *testing.M) { os.Exit(pricingtest.Run(m.Run)) }
