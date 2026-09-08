package runtime

import (
	"github.com/SocialGouv/iterion/internal/proctest"
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(proctest.NoProcessLeaks(m.Run)) }
