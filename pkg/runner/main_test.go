package runner

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/proctest"
)

// The fake shared sandbox here runs subbot children as real host commands.
// A fake whose lifetime contract diverges from the real driver's leaves them
// running; the guard fails the package instead of exporting the leak to
// whatever runs next.
func TestMain(m *testing.M) { os.Exit(proctest.NoProcessLeaks(m.Run)) }
