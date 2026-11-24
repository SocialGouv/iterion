package connectors_test

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/pricingtest"
)

// The package witness judges subscriptions through pkg/connector/binding,
// whose import graph links a live pricing tier: the tier is isolated so no
// verdict reads a price or a context window from the network.
func TestMain(m *testing.M) { os.Exit(pricingtest.Run(m.Run)) }
