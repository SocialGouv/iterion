package blob

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
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

// fakeS3 is a minimal in-memory S3 gateway: PUT/GET object,
// HEAD bucket, ListObjectsV2 and the batch DeleteObjects POST.
type fakeS3 struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
	// putContentTypes records the Content-Type seen per key.
	putContentTypes map[string]string
}

func newFakeS3(t *testing.T, bucket string) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{bucket: bucket, objects: map[string][]byte{}, putContentTypes: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// splitPath maps a path-style request onto (bucket, key).
func (f *fakeS3) splitPath(p string) (string, string) {
	trimmed := strings.TrimPrefix(p, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	return bucket, key
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key := f.splitPath(r.URL.Path)
	if bucket != f.bucket {
		writeS3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	switch {
	case r.Method == http.MethodHead && key == "":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
		f.handleBatchDelete(w, r)
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		f.handleList(w, r)
	case r.Method == http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeS3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		f.mu.Lock()
		f.objects[key] = body
		f.putContentTypes[key] = r.Header.Get("Content-Type")
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		f.mu.Lock()
		body, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		_, _ = w.Write(body)
	case r.Method == http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (f *fakeS3) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	type object struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	}
	type result struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		Name        string   `xml:"Name"`
		Prefix      string   `xml:"Prefix"`
		KeyCount    int      `xml:"KeyCount"`
		IsTruncated bool     `xml:"IsTruncated"`
		Contents    []object `xml:"Contents"`
	}
	res := result{Name: f.bucket, Prefix: prefix}
	for _, k := range f.keys() {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		f.mu.Lock()
		size := int64(len(f.objects[k]))
		f.mu.Unlock()
		res.Contents = append(res.Contents, object{Key: k, Size: size})
	}
	res.KeyCount = len(res.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func (f *fakeS3) handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		XMLName xml.Name `xml:"Delete"`
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := xml.Unmarshal(body, &req); err != nil {
		writeS3Error(w, http.StatusBadRequest, "MalformedXML")
		return
	}
	f.mu.Lock()
	for _, o := range req.Objects {
		delete(f.objects, o.Key)
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></DeleteResult>`))
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

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
	got := fake.keys()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("objects in the bucket = %v, want %v", got, want)
	}
	if ct := fake.putContentTypes["artifacts/run-001/review/1.json"]; ct != "application/json" {
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
	if len(fake.keys()) != 4 {
		t.Fatalf("a re-upload created a new object: %v", fake.keys())
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
	if got, want := fake.keys(), []string{"artifacts/run-b/plan/0.json"}; strings.Join(got, ",") != strings.Join(want, ",") {
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
