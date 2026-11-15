package store_test

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
	"github.com/SocialGouv/iterion/internal/pricingtest"
)

// The record-codec profiles test reaches runview and the model executor, which
// link a live model-pricing tier: isolate it as well as the home (#3257).
func TestMain(m *testing.M) {
	os.Exit(hometest.Isolate(func() int { return pricingtest.Run(m.Run) }))
}
