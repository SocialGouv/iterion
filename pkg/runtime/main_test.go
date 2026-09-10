package runtime

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/proctest"
)

// Tool nodes and the fake shared sandboxes here start real shells, and a
// shell's own children (a `sleep` in a polling loop) only die because #955
// cancels the whole process group. The guard is the postcondition for that:
// it fails the package when a helper outlived the test that started it,
// instead of letting it run on into the next one.
func TestMain(m *testing.M) { os.Exit(proctest.NoProcessLeaks(m.Run)) }
