package binding

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/pricingtest"
)

func TestMain(m *testing.M) { os.Exit(pricingtest.Run(m.Run)) }
