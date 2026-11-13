package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// liveRegistryCachePrefix names the directories IsolateLiveRegistry creates,
// and is what it checks before removing one.
const liveRegistryCachePrefix = "claw-test-live-registry-"

// IsolateLiveRegistry runs a test binary off the live model registry. Claw's
// registry reads $XDG_CACHE_HOME/claw-code-go/models-cache.json — the cache
// the host last fetched — and refreshes it from openrouter.ai and models.dev
// in the background, so a verdict that reads a context window, an output cap
// or a price would otherwise depend on that file and on the network
// (iterion's #1686; iterion's own price-reading tests isolate through
// internal/pricingtest).
//
// It points XDG_CACHE_HOME at an empty directory of its own, switches the
// refresh off (CLAW_DISABLE_LIVE_REGISTRY=1), runs run, and fails the run if
// a live cache appeared in that directory anyway. That witness sees a fetch
// that landed while the switch was cleared and XDG_CACHE_HOME was this
// directory — not a fetch that failed, nor one saved under a test's own
// XDG_CACHE_HOME; the switch itself, re-read before the network, is what
// keeps a refresh from fetching. A test that exercises the cache gives
// itself an XDG_CACHE_HOME and clears the switch with t.Setenv, after
// seeding a fresh cache.
// Every package whose test binary links internal/apikit calls it from
// TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testutil.IsolateLiveRegistry(m.Run)) }
func IsolateLiveRegistry(run func() int) int {
	dir, held, err := createOwnTempDir(liveRegistryCachePrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testutil: create the test cache home: %v\n", err)
		return 1
	}
	defer held.Close()
	for name, value := range map[string]string{"XDG_CACHE_HOME": dir, "CLAW_DISABLE_LIVE_REGISTRY": "1"} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintf(os.Stderr, "testutil: set %s: %v\n", name, err)
			return 1
		}
	}
	code := run()
	cache := filepath.Join(dir, "claw-code-go", "models-cache.json")
	if _, err := os.Stat(cache); err == nil {
		fmt.Fprintf(os.Stderr, "testutil: the run wrote claw's live model cache %s: a test reached the live registry\n", cache)
		code = failed(code)
	}
	if err := removeOwnTempDir(dir, held, liveRegistryCachePrefix); err != nil {
		fmt.Fprintf(os.Stderr, "testutil: %v\n", err)
		code = failed(code)
	}
	return code
}

// createOwnTempDir creates a directory named with prefix under the
// process's temp directory and returns it held open: while the handle is
// open the directory's inode cannot be given to anything else, so the
// handle is what identifies it at removal.
func createOwnTempDir(prefix string) (string, *os.File, error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", nil, err
	}
	held, err := os.Open(dir)
	if err != nil {
		return "", nil, fmt.Errorf("hold %s open: %w", dir, err)
	}
	return dir, held, nil
}

// removeOwnTempDir removes dir only when it is still the directory held
// open since createOwnTempDir, directly under the process's temp directory
// and named with prefix. Anything else — a symbolic link put in its place,
// another directory moved or made there, a temp directory that changed — is
// refused, never removed.
func removeOwnTempDir(dir string, held *os.File, prefix string) error {
	created, err := held.Stat()
	if err != nil {
		return fmt.Errorf("stat the held %s: %w", dir, err)
	}
	now, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if !os.SameFile(created, now) {
		return fmt.Errorf("refusing to remove %s: it is no longer the directory this run created", dir)
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return fmt.Errorf("resolve the temp directory: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return fmt.Errorf("resolve %s: %w", filepath.Dir(dir), err)
	}
	if parent != tmp || !strings.HasPrefix(filepath.Base(dir), prefix) {
		return fmt.Errorf("refusing to remove %s: not a %s* directory directly under %s", dir, prefix, tmp)
	}
	return os.RemoveAll(dir)
}

func failed(code int) int {
	if code == 0 {
		return 1
	}
	return code
}
