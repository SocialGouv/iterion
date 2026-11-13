package openai

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.IsolateLiveRegistry(m.Run))
}
