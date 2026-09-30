package blob

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Nothing but the gateway's listing bounds what a prefix sweep deletes and
// what a prefix listing returns. These tests put a gateway that IGNORES the
// prefix (the fake's ignorePrefix) under every one of them, with other runs'
// objects in the bucket.

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

// On a conforming gateway each sweep deletes its run's key and nothing of
// run-a-b's: the trailing slash of every prefix is load-bearing.
func TestS3SweepsDeleteTheirRunAndNotTheNeighbourSharingItsIDPrefix(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			seed(f, sw.own)
			seed(f, foreignKeys...)
			if err := sw.sweep(context.Background(), c); err != nil {
				t.Fatalf("%s on a conforming gateway: %v", sw.name, err)
			}
			if f.has(sw.own) {
				t.Fatalf("%s left its own key %s", sw.name, sw.own)
			}
			for _, k := range foreignKeys {
				if !f.has(k) {
					t.Fatalf("%s deleted %s, which is not run-a's", sw.name, k)
				}
			}
		})
	}
}

// A gateway that ignores the prefix lists the whole bucket: the sweep must
// still delete only its run's key, keep every other object, and say why.
func TestS3SweepsNeverDeleteAListedKeyOutsideTheirPrefix(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			seed(f, sw.own)
			seed(f, foreignKeys...)
			f.set(&f.ignorePrefix, true)
			err := sw.sweep(context.Background(), c)
			if !errors.Is(err, ErrListingOutsidePrefix) {
				t.Fatalf("%s behind a gateway ignoring the prefix returned %v, want ErrListingOutsidePrefix", sw.name, err)
			}
			for _, k := range foreignKeys {
				if !f.has(k) {
					t.Fatalf("%s deleted %s, listed by a gateway that ignored the prefix", sw.name, k)
				}
			}
			if f.has(sw.own) {
				t.Fatalf("%s left its own key %s", sw.name, sw.own)
			}
		})
	}
}

// DeleteObjects answers 200 with a per-object error list: a sweep that
// deleted nothing must not report success.
func TestS3SweepsReportPerObjectDeleteFailures(t *testing.T) {
	for _, sw := range prefixSweeps() {
		t.Run(sw.name, func(t *testing.T) {
			f, srv := newFakeS3(t, "b")
			c := newTestS3Client(t, srv.URL, "b")
			seed(f, sw.own)
			f.set(&f.refuseDeletes, true)
			err := sw.sweep(context.Background(), c)
			if err == nil || !strings.Contains(err.Error(), sw.own) {
				t.Fatalf("%s with every delete refused returned %v, want an error naming %s", sw.name, err, sw.own)
			}
			if !f.has(sw.own) {
				t.Fatalf("the fake deleted %s although it refused the delete", sw.own)
			}
		})
	}
}

// The prefix listings fail rather than return someone else's keys: a
// version counted from a stray top-level `7.json`, or another run's object
// served as one of this run's files.
func TestS3ListingsRefuseAKeyOutsideTheirPrefix(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeS3(t, "b")
	c := newTestS3Client(t, srv.URL, "b")
	seed(f, "artifacts/run-a/plan/0.json", "runfiles/run-a/out/report.txt", "7.json", "runfiles/run-b/secret.txt")

	if v, err := c.ListArtifactVersions(ctx, "run-a", "plan"); err != nil || len(v) != 1 || v[0] != 0 {
		t.Fatalf("ListArtifactVersions on a conforming gateway = %v, %v; want [0]", v, err)
	}
	if files, err := c.ListRunFiles(ctx, "run-a"); err != nil || len(files) != 1 || files[0].Path != "out/report.txt" {
		t.Fatalf("ListRunFiles on a conforming gateway = %v, %v; want [out/report.txt]", files, err)
	}

	f.set(&f.ignorePrefix, true)
	if v, err := c.ListArtifactVersions(ctx, "run-a", "plan"); !errors.Is(err, ErrListingOutsidePrefix) {
		t.Fatalf("ListArtifactVersions behind a gateway ignoring the prefix = %v, %v; want ErrListingOutsidePrefix", v, err)
	}
	if files, err := c.ListRunFiles(ctx, "run-a"); !errors.Is(err, ErrListingOutsidePrefix) {
		t.Fatalf("ListRunFiles behind a gateway ignoring the prefix = %v, %v; want ErrListingOutsidePrefix", files, err)
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
