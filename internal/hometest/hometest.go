// Package hometest keeps a test binary off the operator's homes: the iterion
// home (ITERION_HOME) and the user home (HOME).
package hometest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	homePrefix     = "iterion-test-home-"
	userHomePrefix = "iterion-test-user-home-"
)

// removeAttempts and removeBackoff bound how long a home is waited on for a
// goroutine a test left running to stop writing into it.
const (
	removeAttempts = 5
	removeBackoff  = 100 * time.Millisecond
)

// Isolate points ITERION_HOME at a directory it creates for this test
// process, runs run, then removes that directory and the one pkg/store
// creates for a test that clears ITERION_HOME. It returns run's exit code,
// or 1 when a home cannot be created or is left behind. Wrap the TestMain of
// a package whose tests reach the iterion home — run stores, installed
// plugins, the global runs view:
//
//	func TestMain(m *testing.M) { os.Exit(hometest.Isolate(m.Run)) }
//
// An ITERION_HOME already set in the environment is replaced for the
// process: it names the operator's real data, which no test may touch. A
// test that needs its own home still t.Setenv's one over this. The
// planted-names marker an iterion process above the test run may have exported
// (internal/envtrust) is dropped too: it would make the home set here read as
// one a project `.env` planted.
func Isolate(run func() int) int {
	code := isolateHome("ITERION_HOME", homePrefix, "test iterion home", func() int {
		if err := os.Unsetenv(envtrust.EnvPlantedNames); err != nil {
			fmt.Fprintf(os.Stderr, "hometest: drop %s: %v\n", envtrust.EnvPlantedNames, err)
			return 1
		}
		return run()
	})
	if err := store.ReleaseTestIterionHomeForTests(); err != nil {
		fmt.Fprintf(os.Stderr, "hometest: %v\n", err)
		code = failed(code)
	}
	return code
}

// IsolateUserHome points HOME at an empty directory it creates for this test
// process, runs run, then removes that directory. It returns run's exit code,
// or 1 when the home cannot be created or is left behind. Wrap the TestMain of
// a package whose tests spawn a login shell — `sh -lc` (dispatcher hooks,
// schedule guards), `$SHELL -l` (run shells) — which, as sh or bash, reads its
// profile from $HOME: an operator's profile runs pyenv init, direnv hooks and
// the like, it writes into the operator's home, and a spawn killed mid-profile
// leaves a host lock held — pyenv's rehash lock then stalls the login shells
// started in the next two minutes, each for up to 60s (#2231). Compose it
// with Isolate:
//
//	os.Exit(hometest.IsolateUserHome(func() int { return hometest.Isolate(m.Run) }))
//
// A test that needs a home of its own still t.Setenv's one over this. On
// Windows os.UserHomeDir reads USERPROFILE, which this leaves alone.
// RequireNoOperatorProfile is the check a package pins it with.
func IsolateUserHome(run func() int) int {
	return isolateHome("HOME", userHomePrefix, "test user home", run)
}

// inheritedUserHome is HOME as the test binary inherited it — the operator's —
// read when this package initialises, before any TestMain can replace it.
var inheritedUserHome = os.Getenv("HOME")

// RequireNoOperatorProfile fails t unless home — the HOME a login shell spawned
// by the test reported — is not the one the test binary inherited and holds no
// login profile for the shell to source. A home that is not absolute fails
// too: a shell whose environment lacks HOME reports it empty, and bash then
// reads the profile of the account's passwd home.
func RequireNoOperatorProfile(t testing.TB, home string) {
	t.Helper()
	if !filepath.IsAbs(home) {
		t.Fatalf("the login shell ran under HOME=%q, want the absolute directory IsolateUserHome made", home)
	}
	if inheritedUserHome != "" && filepath.Clean(home) == filepath.Clean(inheritedUserHome) {
		t.Fatalf("the login shell ran under the inherited HOME %s, and sources the profile there", home)
	}
	for _, rc := range []string{".profile", ".bash_profile", ".bash_login"} {
		_, err := os.Stat(filepath.Join(home, rc))
		if err == nil {
			t.Fatalf("the login shell's HOME %s holds a %s it sources", home, rc)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stat %s in the login shell's HOME %s: %v", rc, home, err)
		}
	}
}

// isolateHome points the environment variable name at a directory it creates
// under the temp dir, runs run, then removes that directory — only that one.
// what names the directory in its errors.
func isolateHome(name, prefix, what string, run func() int) int {
	parent, err := filepath.Abs(os.TempDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "hometest: resolve the temp dir: %v\n", err)
		return 1
	}
	dir, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hometest: create the %s: %v\n", what, err)
		return 1
	}
	code := 1
	if err := os.Setenv(name, dir); err != nil {
		fmt.Fprintf(os.Stderr, "hometest: point %s at %s: %v\n", name, dir, err)
	} else {
		code = run()
	}
	if err := removeHome(parent, dir, prefix, what); err != nil {
		fmt.Fprintf(os.Stderr, "hometest: %v\n", err)
		code = failed(code)
	}
	return code
}

// isTestHome reports whether dir is one isolateHome creates for prefix: a
// direct child of parent carrying that prefix.
func isTestHome(parent, dir, prefix string) bool {
	return filepath.Dir(dir) == filepath.Clean(parent) && strings.HasPrefix(filepath.Base(dir), prefix)
}

// removeHome removes the directory isolateHome created — only that one —
// retrying while a goroutine a test left running may still be writing into it.
func removeHome(parent, dir, prefix, what string) error {
	if !isTestHome(parent, dir, prefix) {
		return fmt.Errorf("refusing to remove %q: not a %s under %s", dir, what, parent)
	}
	var err error
	for attempt := 0; attempt < removeAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(removeBackoff)
		}
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
	}
	return fmt.Errorf("the %s %s is left behind: %w", what, dir, err)
}

func failed(code int) int {
	if code == 0 {
		return 1
	}
	return code
}
