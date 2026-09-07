package server

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
)

// newTestRunviewService is the single construction point for a
// runview.Service in this package's tests: it builds the service and
// registers its teardown.
//
// A live service owns background goroutines — the orphan reconciler above
// all — that write run statuses on a ticker. Left running, they write into
// the store (almost always a t.TempDir()) after the test has returned,
// which surfaces as "TempDir RemoveAll cleanup: directory not empty" on
// whichever test is cleaning up at that moment. Stop cancels and AWAITS
// them, so the store belongs to nobody once the cleanup returns.
//
// Cleanups run LIFO, so this one fires before the t.TempDir() removal
// registered by the caller when it made the store.
func newTestRunviewService(t *testing.T, storeDir string, opts ...runview.ServiceOption) *runview.Service {
	t.Helper()
	svc, err := runview.NewService(storeDir, opts...)
	if err != nil {
		t.Fatalf("runview.NewService(%q): %v", storeDir, err)
	}
	stopRunviewOnCleanup(t, svc)
	return svc
}

// stopRunviewOnCleanup registers the teardown for a service the caller
// built itself (a test asserting on NewService's own error, say). Same
// contract as newTestRunviewService.
func stopRunviewOnCleanup(t *testing.T, svc *runview.Service) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Stop(ctx)
	})
}

// TestRunviewFixtureSweep_NoUnstoppedServices keeps the class closed as the
// package grows: a test that constructs a runview.Service by hand owns
// goroutines nobody joins, and the next TempDir cleanup is the one that
// pays for it. Every construction goes through the fixture above, or names
// itself here with the reason it stops the service another way.
func TestRunviewFixtureSweep_NoUnstoppedServices(t *testing.T) {
	allowed := map[string]string{
		"runview_fixture_test.go": "the fixture itself",
	}
	raw := regexp.MustCompile(`runview\.NewService\(`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	lineComment := regexp.MustCompile(`//[^\n]*`)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Clean(name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if !raw.MatchString(lineComment.ReplaceAllString(string(b), "")) {
			continue
		}
		if _, ok := allowed[name]; ok {
			continue
		}
		t.Errorf("%s builds a runview.Service directly — use newTestRunviewService (or stopRunviewOnCleanup) so its goroutines are joined before the TempDir is removed, or add an allowlist entry with its reason", name)
	}
}
