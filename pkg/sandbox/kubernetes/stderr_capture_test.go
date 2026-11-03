package kubernetes

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/internal/stdiotest"
)

// captureStderr redirects os.Stderr to a pipe for the caller's scope,
// returns a `read` callback that drains everything so far, and a
// `restore` function to undo the redirect. Used by the workspace-copy
// timeout tests to verify the E7 (garbage env warn) and E8 (halfway /
// overrun) stderr-side signals.
func captureStderr(t *testing.T) (read func() string, restore func()) {
	t.Helper()
	c := stdiotest.Start(t, &os.Stderr)
	return c.Read, c.Close
}
