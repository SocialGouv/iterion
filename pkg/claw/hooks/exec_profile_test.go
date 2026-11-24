//go:build !windows

package hooks

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
	"os"
	"path/filepath"
	"testing"
)

func TestShellCommandReadsControlledLoginProfile(t *testing.T) {
	homeDir := testutil.IsolateShellHome(t)
	if err := os.WriteFile(filepath.Join(homeDir, ".profile"), []byte("printf 'profile-out\n'\nprintf 'profile-err\n' >&2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := runShellCommand(context.Background(), "printf 'body-out\n'; printf 'body-err\n' >&2; exit 7", nil, nil, nil)
	t.Logf("stdout=%q stderr=%q exit=%d", got.Stdout, got.Stderr, got.ExitCode)
	if got.Stdout != "profile-out\nbody-out" || got.Stderr != "profile-err\nbody-err" || got.ExitCode != 7 {
		t.Fatalf("real login shell or its output was altered: %+v", got)
	}
}
