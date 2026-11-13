//go:build !windows

package hooks

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
	"os"
	"path/filepath"
	"testing"
)

func TestShellHomeIsolatedAndRestored(t *testing.T) {
	outside := t.TempDir()
	hostile := filepath.Join(outside, "startup")
	if err := os.WriteFile(hostile, []byte("printf 'BAD-env-out\\n'\nprintf 'BAD-env-err\\n' >&2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".profile"), []byte("printf 'BAD-profile-out\\n'\nprintf 'BAD-profile-err\\n' >&2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", outside)
	t.Setenv("ENV", hostile)
	t.Setenv("BASH_ENV", hostile)
	t.Run("isolated", func(t *testing.T) {
		home := testutil.IsolateShellHome(t)
		if home == outside || os.Getenv("HOME") != home || os.Getenv("ENV") != "" || os.Getenv("BASH_ENV") != "" {
			t.Fatal("isolation incomplete")
		}
		got := runShellCommand(context.Background(), "printf 'body-out\\n'; printf 'body-err\\n' >&2; exit 7", nil, nil, nil)
		t.Logf("isolated stdout=%q stderr=%q exit=%d", got.Stdout, got.Stderr, got.ExitCode)
		if got.Stdout != "body-out" || got.Stderr != "body-err" || got.ExitCode != 7 {
			t.Fatalf("fixture leaked or output filtered: %+v", got)
		}
	})
	if os.Getenv("HOME") != outside || os.Getenv("ENV") != hostile || os.Getenv("BASH_ENV") != hostile {
		t.Fatal("environment not restored after child")
	}
	got := runShellCommand(context.Background(), "printf 'body-out\\n'; printf 'body-err\\n' >&2; exit 7", nil, nil, nil)
	t.Logf("restored stdout=%q stderr=%q exit=%d", got.Stdout, got.Stderr, got.ExitCode)
	if got.Stdout != "BAD-profile-out\nbody-out" || got.Stderr != "BAD-profile-err\nbody-err" || got.ExitCode != 7 {
		t.Fatalf("hostile-profile control did not execute: %+v", got)
	}
}

func TestShellHomeCanContainSpaces(t *testing.T) {
	isolated := testutil.IsolateShellHome(t)
	home := filepath.Join(isolated, "space in home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("printf 'space-profile\\n'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := runShellCommand(context.Background(), "printf 'body\\n'", nil, nil, nil)
	t.Logf("space HOME stdout=%q stderr=%q exit=%d", got.Stdout, got.Stderr, got.ExitCode)
	if got.Stdout != "space-profile\nbody" || got.Stderr != "" || got.ExitCode != 0 {
		t.Fatal(got)
	}
}

func TestNestedBashIgnoresAmbientStartup(t *testing.T) {
	outside := t.TempDir()
	startup := filepath.Join(outside, "ambient.bash")
	if err := os.WriteFile(startup, []byte("printf 'BAD-bash-env\\n'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", outside)
	t.Setenv("BASH_ENV", startup)
	t.Run("isolated", func(t *testing.T) {
		testutil.IsolateShellHome(t)
		got := runShellCommand(context.Background(), "bash -c \"printf 'child-out\\\\n'\"", nil, nil, nil)
		t.Logf("nested isolated stdout=%q stderr=%q exit=%d", got.Stdout, got.Stderr, got.ExitCode)
		if got.Stdout != "child-out" || got.Stderr != "" || got.ExitCode != 0 {
			t.Fatalf("nested child imported ambient BASH_ENV: %+v", got)
		}
	})
	got := runShellCommand(context.Background(), "bash -c \"printf 'child-out\\\\n'\"", nil, nil, nil)
	t.Logf("nested restored stdout=%q stderr=%q exit=%d", got.Stdout, got.Stderr, got.ExitCode)
	if got.Stdout != "BAD-bash-env\nchild-out" || got.ExitCode != 0 {
		t.Fatalf("startup control did not fire: %+v", got)
	}
}
