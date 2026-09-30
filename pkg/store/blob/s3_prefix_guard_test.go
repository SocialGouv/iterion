package blob

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Nothing but the gateway's listing bounds what a prefix sweep deletes and
// what a prefix listing returns. These tests drive every sweep and listing
// through a fake gateway that pages for real, then misbehaves: it ignores
// the prefix, refuses to list, or cannot move past a page.

// prefixSweep is one of the S3Client sweeps, with a key it owns for run-a.
type prefixSweep struct {
	name  string
	own   string
	sweep func(ctx context.Context, c *S3Client) error
}

func prefixSweeps() []prefixSweep {
	return []prefixSweep{
		{"DeleteRun", "artifacts/run-a/plan/0.json", func(ctx context.Context, c *S3Client) error { return c.DeleteRun(ctx, "run-a") }},
		{"DeleteRunAttachments", "attachments/run-a/spec/spec.md", func(ctx context.Context, c *S3Client) error { return c.DeleteRunAttachments(ctx, "run-a") }},
		{"DeleteRunBackendSessions", "sessions/run-a/ref-1", func(ctx context.Context, c *S3Client) error { return c.DeleteRunBackendSessions(ctx, "run-a") }},
		{"DeleteRunToolBlobs", "tools/run-a/tu-1/output", func(ctx context.Context, c *S3Client) error { return c.DeleteRunToolBlobs(ctx, "run-a") }},
		{"DeleteRunFiles", "runfiles/run-a/out/report.txt", func(ctx context.Context, c *S3Client) error { return c.DeleteRunFiles(ctx, "run-a") }},
	}
}

// ownKeys are five keys of the swept run's family: several pages at a page
// size of 2.
func (sw prefixSweep) ownKeys() []string {
	keys := []string{sw.own}
	for i := 1; i < 5; i++ {
		keys = append(keys, fmt.Sprintf("%s.%d", sw.own, i))
	}
	return keys
}

// foreignKeys belong to other runs — including run-a-b, whose keys start
// with every run-a prefix minus its trailing slash — or to nobody.
var foreignKeys = []string{
	"artifacts/run-a-b/plan/0.json",
	"attachments/run-a-b/spec/spec.md",
	"sessions/run-a-b/ref-1",
	"tools/run-a-b/tu-1/output",
	"runfiles/run-a-b/out/report.txt",
	"artifacts/run-b/plan/0.json",
	"README-real.txt",
}

func seed(f *fakeS3, keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		f.objects[k] = []byte("x")
	}
}

func (f *fakeS3) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakeS3) setPageSize(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageSize = n
}

func (f *fakeS3) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

// within fails the test when fn has not returned after d: the sweeps and
// listings under test must stop on their own, whatever the gateway does.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: it loops", what, d)
	}
}

// requireNoForeignKeyInError: the error of a sweep or listing reaches HTTP
// clients (the run-delete and artifact routes print it), so it names the
// prefix and a count, never a key that belongs to someone else.
func requireNoForeignKeyInError(t *testing.T, err error) {
	t.Helper()
	for _, k := range foreignKeys {
		if strings.Contains(err.Error(), k) {
			t.Fatalf("the error quotes %s, a key outside the run: %v", k, err)
		}
	}
}

// On a conforming gateway each sweep deletes every key of its run, page
// after page, and nothing of run-a-b's: the trailing slash of every prefix
// is load-bearing.
func TestS3SweepsDeleteTheirRunAndNotTheNeighbourSharingItsIDPrefix(t *testing.T) {
	for _, pageSize := range []int{0, 2} {
		for _, sw := range prefixSweeps() {
			t.Run(fmt.Sprintf("%s/page=%d", sw.name, pageSize), func(t *testing.T) {
				f, srv := newFakeS3(t, "b")
				c := newTestS3Client(t, srv.URL, "b")
				f.setPageSize(pageSize)
				seed(f, sw.ownKeys()...)
				seed(f, foreignKeys...)
				if err := sw.sweep(context.Background(), c); err != nil {
					t.Fatalf("%s on a conforming gateway: %v", sw.name, err)
				}
				for _, k := range sw.ownKeys() {
					if f.has(k) {
						t.Fatalf("%s left its own key %s", sw.name, k)
					}
				}
				for _, k := range foreignKeys {
					if !f.has(k) {
						t.Fatalf("%s deleted %s, which is not run-a's", sw.name, k)
					}
				}
			})
		}
	}
}

// A gateway that ignores the prefix lists other runs' keys: the sweep stops
// on that page, deletes nothing of it, keeps every other object, and says
// why without quoting the foreign keys.
func TestS3SweepsNeverDeleteAListedKeyOutsideTheirPrefix(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			f.setPageSize(2)
			seed(f, sw.own)
			seed(f, foreignKeys...)
			f.set(&f.ignorePrefix, true)
			var err error
			within(t, 10*time.Second, sw.name, func() { err = sw.sweep(context.Background(), c) })
			if !errors.Is(err, ErrListingOutsidePrefix) {
				t.Fatalf("%s behind a gateway ignoring the prefix returned %v, want ErrListingOutsidePrefix", sw.name, err)
			}
			if n := f.listCount(); n != 1 {
				t.Fatalf("%s listed %d pages of a gateway that ignored the prefix on the first, want 1: it does not stop there", sw.name, n)
			}
			requireNoForeignKeyInError(t, err)
			for _, k := range foreignKeys {
				if !f.has(k) {
					t.Fatalf("%s deleted %s, listed by a gateway that ignored the prefix", sw.name, k)
				}
			}
			if !f.has(sw.own) {
				t.Fatalf("%s deleted %s from a page it could not trust", sw.name, sw.own)
			}
		})
	}
}

// DeleteObjects answers 200 with a per-object error list: a sweep that
// deleted nothing must not report success, and names every key it kept.
func TestS3SweepsReportPerObjectDeleteFailures(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			own := sw.ownKeys()[:2]
			seed(f, own...)
			f.set(&f.refuseDeletes, true)
			err := sw.sweep(context.Background(), c)
			for _, k := range own {
				if err == nil || !strings.Contains(err.Error(), k) {
					t.Fatalf("%s with every delete refused returned %v, want an error naming %s", sw.name, err, k)
				}
				if !f.has(k) {
					t.Fatalf("the fake deleted %s although it refused the delete", k)
				}
			}
		})
	}
}

// A listing that fails does not get better by asking again: the SDK's
// paginator does not move past a page it failed to fetch, so a sweep that
// retried it would spin — and pile up errors — until its context ends.
func TestS3SweepsStopOnAListingThatFails(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			seed(f, sw.own)
			f.set(&f.refuseLists, true)
			var err error
			within(t, 10*time.Second, sw.name, func() { err = sw.sweep(context.Background(), c) })
			if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
				t.Fatalf("%s with the listing refused returned %v, want the AccessDenied", sw.name, err)
			}
			if n := f.listCount(); n > 1 {
				t.Fatalf("%s sent %d listing requests for a listing refused outright, want 1", sw.name, n)
			}
		})
	}
}

// A gateway that ignores the continuation token serves the first page over
// and over: with the deletes refused too nothing ever changes, and only the
// repeated token shows the sweep it cannot finish.
func TestS3SweepsStopOnAListingThatCannotAdvance(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			f.setPageSize(2)
			seed(f, sw.ownKeys()...)
			f.set(&f.stuckToken, true)
			f.set(&f.refuseDeletes, true)
			var err error
			within(t, 10*time.Second, sw.name, func() { err = sw.sweep(context.Background(), c) })
			if err == nil || !strings.Contains(err.Error(), "repeated the continuation token") {
				t.Fatalf("%s behind a gateway repeating its continuation token returned %v, want the repeated-token error", sw.name, err)
			}
			if n := f.listCount(); n > 3 {
				t.Fatalf("%s sent %d listing requests to a gateway stuck on one page", sw.name, n)
			}
		})
	}
}

// A truncated page without a continuation token would end the SDK's
// paginator as if the listing were complete: the sweep deletes that page,
// then stops with the error rather than report the rest as swept.
func TestS3SweepsStopOnATruncatedPageWithoutAToken(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			f.setPageSize(2)
			seed(f, sw.ownKeys()...)
			f.set(&f.dropContinuationToken, true)
			var err error
			within(t, 10*time.Second, sw.name, func() { err = sw.sweep(context.Background(), c) })
			if err == nil || !strings.Contains(err.Error(), "without a continuation token") {
				t.Fatalf("%s behind a gateway dropping its continuation token returned %v, want the missing-token error", sw.name, err)
			}
			left := 0
			for _, k := range sw.ownKeys() {
				if f.has(k) {
					left++
				}
			}
			if left != 1 || f.listCount() != 2 {
				t.Fatalf("%s left %d of its keys after %d listings, want 1 after 2 (the two pages listed swept, then a stop)", sw.name, left, f.listCount())
			}
		})
	}
}

// A sweep that fails midway reports every failure: the objects it could
// not delete on the pages it got through, and the listing that stopped it.
func TestS3SweepsReportEveryFailureOfAPartialSweep(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			f.setPageSize(2)
			seed(f, sw.ownKeys()...)
			f.set(&f.refuseDeletes, true)
			f.set(&f.refuseContinuations, true)
			err := sw.sweep(context.Background(), c)
			if err == nil {
				t.Fatalf("%s with deletes and the second page refused returned nil", sw.name)
			}
			for _, want := range append(sw.ownKeys()[:2], " page: ") {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s: the error does not report %q: %v", sw.name, want, err)
				}
			}
		})
	}
}

// Cancellation stops a sweep before it deletes anything, and callers can
// tell it from a backend failure.
func TestS3SweepsStopOnACancelledContext(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			seed(f, sw.own)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var err error
			within(t, 10*time.Second, sw.name, func() { err = sw.sweep(ctx, c) })
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s on a cancelled context returned %v, want context.Canceled", sw.name, err)
			}
			if !f.has(sw.own) {
				t.Fatalf("%s deleted %s on a cancelled context", sw.name, sw.own)
			}
		})
	}
}

// The prefix listings page through everything under their prefix, and fail
// rather than return someone else's key — a stray version counted as this
// node's, another run's object served as one of this run's files. The
// foreign keys here share the listing's family (and sort after its own), so
// a guard loosened to the family's first segment is caught too.
func TestS3ListingsRefuseAKeyOutsideTheirPrefix(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		own, intruder []string
		list          func(c *S3Client) (int, error)
	}{
		{"ListArtifactVersions",
			[]string{"artifacts/run-a/plan/0.json", "artifacts/run-a/plan/1.json", "artifacts/run-a/plan/2.json"},
			[]string{"artifacts/run-a/plan2/7.json"},
			func(c *S3Client) (int, error) {
				v, err := c.ListArtifactVersions(ctx, "run-a", "plan")
				return len(v), err
			}},
		{"ListRunFiles",
			[]string{"runfiles/run-a/out/a.txt", "runfiles/run-a/out/b.txt", "runfiles/run-a/out/c.txt"},
			[]string{"runfiles/run-b/secret.txt"},
			func(c *S3Client) (int, error) { f, err := c.ListRunFiles(ctx, "run-a"); return len(f), err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			f.setPageSize(2)
			seed(f, tc.own...)
			seed(f, tc.intruder...)
			if n, err := tc.list(c); err != nil || n != len(tc.own) {
				t.Fatalf("%s over two pages = %d entries, %v; want %d", tc.name, n, err, len(tc.own))
			}
			f.set(&f.ignorePrefix, true)
			n, err := tc.list(c)
			if !errors.Is(err, ErrListingOutsidePrefix) {
				t.Fatalf("%s behind a gateway ignoring the prefix = %d entries, %v; want ErrListingOutsidePrefix", tc.name, n, err)
			}
			for _, k := range tc.intruder {
				if strings.Contains(err.Error(), k) {
					t.Fatalf("the error quotes %s, a key outside the listing: %v", k, err)
				}
			}
		})
	}
}

// A listing whose truncated page lost its continuation token fails rather
// than return the first pages as the whole listing.
func TestS3ListingsStopOnATruncatedPageWithoutAToken(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeS3(t, "b")
	c := newTestS3Client(t, srv.URL, "b")
	f.setPageSize(2)
	for i := 0; i < 5; i++ {
		seed(f, fmt.Sprintf("artifacts/run-a/plan/%d.json", i), fmt.Sprintf("runfiles/run-a/out/%d.txt", i))
	}
	f.set(&f.dropContinuationToken, true)
	if v, err := c.ListArtifactVersions(ctx, "run-a", "plan"); err == nil || !strings.Contains(err.Error(), "without a continuation token") {
		t.Fatalf("ListArtifactVersions behind a gateway dropping its token = %v, %v; want the missing-token error", v, err)
	}
	if files, err := c.ListRunFiles(ctx, "run-a"); err == nil || !strings.Contains(err.Error(), "without a continuation token") {
		t.Fatalf("ListRunFiles behind a gateway dropping its token = %v, %v; want the missing-token error", files, err)
	}
}

// A listing stuck on one page ends in an error, not in a loop.
func TestS3ListingsStopOnAListingThatCannotAdvance(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeS3(t, "b")
	c := newTestS3Client(t, srv.URL, "b")
	f.setPageSize(1)
	seed(f, "artifacts/run-a/plan/0.json", "artifacts/run-a/plan/1.json", "runfiles/run-a/a.txt", "runfiles/run-a/b.txt")
	f.set(&f.stuckToken, true)
	var errV, errF error
	within(t, 10*time.Second, "ListArtifactVersions", func() { _, errV = c.ListArtifactVersions(ctx, "run-a", "plan") })
	within(t, 10*time.Second, "ListRunFiles", func() { _, errF = c.ListRunFiles(ctx, "run-a") })
	for name, err := range map[string]error{"ListArtifactVersions": errV, "ListRunFiles": errF} {
		if err == nil || !strings.Contains(err.Error(), "repeated the continuation token") {
			t.Fatalf("%s behind a gateway repeating its continuation token returned %v, want the repeated-token error", name, err)
		}
	}
}

// DeleteRun bounds its sweep with a validated run id, like the other four.
func TestS3DeleteRunRefusesAnInvalidRunID(t *testing.T) {
	f, srv := newFakeS3(t, "b")
	c := newTestS3Client(t, srv.URL, "b")
	seed(f, "artifacts/run-a/plan/0.json")
	for _, id := range []string{"", "..", "run-a/plan"} {
		if err := c.DeleteRun(context.Background(), id); err == nil {
			t.Fatalf("DeleteRun(%q) = nil, want an invalid run_id error", id)
		}
	}
	if !f.has("artifacts/run-a/plan/0.json") {
		t.Fatal("a refused DeleteRun deleted run-a's artifact")
	}
}
