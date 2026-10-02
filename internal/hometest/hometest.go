// Package hometest keeps a test binary off the operator's iterion home.
package hometest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/store"
)

const homePrefix = "iterion-test-home-"

// removeAttempts and removeBackoff bound how long Isolate waits for a
// goroutine a test left running to stop writing into the home it removes.
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
	parent, err := filepath.Abs(os.TempDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "hometest: resolve the temp dir: %v\n", err)
		return 1
	}
	dir, err := os.MkdirTemp(parent, homePrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hometest: create the test iterion home: %v\n", err)
		return 1
	}
	code := 1
	if err := os.Setenv("ITERION_HOME", dir); err != nil {
		fmt.Fprintf(os.Stderr, "hometest: point ITERION_HOME at %s: %v\n", dir, err)
	} else if err := os.Unsetenv(envtrust.EnvPlantedNames); err != nil {
		fmt.Fprintf(os.Stderr, "hometest: drop %s: %v\n", envtrust.EnvPlantedNames, err)
	} else {
		code = run()
	}
	if err := removeHome(parent, dir); err != nil {
		fmt.Fprintf(os.Stderr, "hometest: %v\n", err)
		code = failed(code)
	}
	if err := store.ReleaseTestIterionHomeForTests(); err != nil {
		fmt.Fprintf(os.Stderr, "hometest: %v\n", err)
		code = failed(code)
	}
	return code
}

// isTestHome reports whether dir is a home Isolate creates: a direct child
// of parent carrying homePrefix.
func isTestHome(parent, dir string) bool {
	return filepath.Dir(dir) == filepath.Clean(parent) && strings.HasPrefix(filepath.Base(dir), homePrefix)
}

// removeHome removes the home Isolate created — only that one — retrying
// while a goroutine a test left running may still be writing into it.
func removeHome(parent, dir string) error {
	if !isTestHome(parent, dir) {
		return fmt.Errorf("refusing to remove %q: not a test iterion home under %s", dir, parent)
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
	return fmt.Errorf("the test iterion home %s is left behind: %w", dir, err)
}

func failed(code int) int {
	if code == 0 {
		return 1
	}
	return code
}
