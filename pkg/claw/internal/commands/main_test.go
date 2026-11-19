package commands

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/hometest"
	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(hometest.IsolateUserHome(func() int {
		return testutil.IsolateLiveRegistry(m.Run)
	}))
}
