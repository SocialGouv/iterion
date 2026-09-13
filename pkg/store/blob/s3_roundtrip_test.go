package blob

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/internal/s3test"
)

// `iterion migrate to-cloud` (and every cloud run afterwards) puts a
// run's artifacts in S3 under a canonical key and reads them back by
// that key alone — nothing else records where an artifact went. So the
// contract that matters is on the wire: WHICH object key the client
// PUTs, that the bytes survive the round-trip verbatim, that a version
// listing enumerates exactly what was written, that a missing key is
// reported as ErrArtifactNotFound (the migration tool branches on it),
// and that DeleteRun sweeps a run's whole prefix and nothing else.
//
// The gateway below is an in-process S3 (path-style, the MinIO posture
// the Endpoint/UsePathStyle config exists for), so the REAL AWS SDK
// client — signing, paging, delete-batching, error mapping — is what is
// exercised, without an S3 account.

var newFakeS3 = s3test.New

func newTestS3Client(t *testing.T, endpoint, bucket string) *S3Client {
	t.Helper()
	c, err := NewS3(context.Background(), Config{
		Region:          "us-east-1",
		Bucket:          bucket,
		Endpoint:        endpoint,
		UsePathStyle:    true,
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The upload half of `migrate to-cloud`: artifacts land at the canonical
// key, come back byte-identical, and the version listing is what the
// bucket actually holds.
func TestS3Client_ArtifactUploadRoundTripAndLayout(t *testing.T) {
	fake, srv := newFakeS3(t, "iterion-artifacts")
	c := newTestS3Client(t, srv.URL, "iterion-artifacts")
	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	bodies := map[int][]byte{
		0: []byte(`{"verdict":"first pass"}`),
		1: []byte(`{"verdict":"second pass","note":"héllo ✓"}`),
		2: []byte(`{"verdict":"third"}`),
	}
	for v, b := range bodies {
		if err := c.PutArtifact(ctx, "run-001", "review", v, b); err != nil {
			t.Fatalf("PutArtifact v%d: %v", v, err)
		}
	}
	// Another run + node so the prefix scoping below means something.
	if err := c.PutArtifact(ctx, "run-002", "review", 0, []byte(`{"other":"run"}`)); err != nil {
		t.Fatalf("PutArtifact other run: %v", err)
	}

	// The key layout is the only index: a drift here orphans every
	// artifact the migration uploaded.
	want := []string{
		"artifacts/run-001/review/0.json",
		"artifacts/run-001/review/1.json",
		"artifacts/run-001/review/2.json",
		"artifacts/run-002/review/0.json",
	}
	got := fake.Keys()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("objects in the bucket = %v, want %v", got, want)
	}
	if ct := fake.ContentType("artifacts/run-001/review/1.json"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	for v, b := range bodies {
		back, err := c.GetArtifact(ctx, "run-001", "review", v)
		if err != nil {
			t.Fatalf("GetArtifact v%d: %v", v, err)
		}
		if string(back) != string(b) {
			t.Fatalf("v%d round-trip: got %q want %q", v, back, b)
		}
	}

	versions, err := c.ListArtifactVersions(ctx, "run-001", "review")
	if err != nil {
		t.Fatalf("ListArtifactVersions: %v", err)
	}
	sort.Ints(versions)
	if len(versions) != 3 || versions[0] != 0 || versions[2] != 2 {
		t.Fatalf("versions = %v, want [0 1 2]", versions)
	}

	// Re-PUT is idempotent (same key overwritten, no duplicate object).
	if err := c.PutArtifact(ctx, "run-001", "review", 1, []byte(`{"verdict":"rewritten"}`)); err != nil {
		t.Fatalf("re-PutArtifact: %v", err)
	}
	if len(fake.Keys()) != 4 {
		t.Fatalf("a re-upload created a new object: %v", fake.Keys())
	}
	back, err := c.GetArtifact(ctx, "run-001", "review", 1)
	if err != nil || string(back) != `{"verdict":"rewritten"}` {
		t.Fatalf("re-upload did not overwrite: %q (%v)", back, err)
	}
}

// A missing artifact must be reported as ErrArtifactNotFound, not as a
// generic backend error: the migration tool and the retention sweeper
// both branch on it, and mapping it wrong turns "not migrated yet" into
// a hard failure.
func TestS3Client_MissingArtifactMapsToNotFound(t *testing.T) {
	_, srv := newFakeS3(t, "b")
	c := newTestS3Client(t, srv.URL, "b")
	ctx := context.Background()

	_, err := c.GetArtifact(ctx, "run-404", "node", 0)
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("GetArtifact on a missing key: %v, want ErrArtifactNotFound", err)
	}
	_, err = c.ListArtifactVersions(ctx, "run-404", "node")
	if !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("ListArtifactVersions on an empty prefix: %v, want ErrArtifactNotFound", err)
	}
}

// DeleteRun sweeps one run's whole artifact prefix (every node, every
// version) through the batch-delete API — and leaves every other run's
// objects alone.
func TestS3Client_DeleteRunSweepsOnlyThatRunsPrefix(t *testing.T) {
	fake, srv := newFakeS3(t, "b")
	c := newTestS3Client(t, srv.URL, "b")
	ctx := context.Background()

	for _, a := range []struct {
		run, node string
		ver       int
	}{
		{"run-a", "plan", 0}, {"run-a", "plan", 1}, {"run-a", "implement", 0},
		{"run-b", "plan", 0},
	} {
		if err := c.PutArtifact(ctx, a.run, a.node, a.ver, []byte(`{}`)); err != nil {
			t.Fatalf("seed %s/%s/%d: %v", a.run, a.node, a.ver, err)
		}
	}

	if err := c.DeleteRun(ctx, "run-a"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if got, want := fake.Keys(), []string{"artifacts/run-b/plan/0.json"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after DeleteRun the bucket holds %v, want %v", got, want)
	}
	if _, err := c.GetArtifact(ctx, "run-a", "plan", 1); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("swept artifact still readable: %v", err)
	}
	// Sweeping a run with nothing stored is a no-op, not an error.
	if err := c.DeleteRun(ctx, "run-never-existed"); err != nil {
		t.Fatalf("DeleteRun on an empty prefix: %v", err)
	}
}
