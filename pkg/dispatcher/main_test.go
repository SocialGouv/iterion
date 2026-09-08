package dispatcher

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/proctest"
)

// The subbot fixtures here park a child on a sentinel file, so a dispatch
// that is not joined leaves a shell polling forever — issue #956, which the
// cancel-and-join cleanup in engine_runner_subbot_test.go fixes. The guard is
// what keeps that fix from silently rotting.
func TestMain(m *testing.M) { os.Exit(proctest.NoProcessLeaks(m.Run)) }
