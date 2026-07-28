package mongo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
)

// TestConformance_Mongo plugs the Mongo store into pkg/store's shared
// conformance harness. Skipped unless ITERION_TEST_MONGO_URI is set —
// the suite needs a real replica set (change streams + transactions),
// so we don't try to spin up testcontainers from a unit test. Local
// runs use the docker-compose.cloud.yml stack:
//
//	devbox run -- task cloud:up:deps
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27017/?replicaSet=rs0' \
//	    devbox run -- go test ./pkg/store/mongo/...
//
// The blob backend is stubbed via inMemoryBlob so a real S3/MinIO
// isn't required just to exercise the run/event/lock paths.
func TestConformance_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo conformance")
	}
	storetest.RunWithOpts(t, func(t *testing.T) store.RunStore {
		t.Helper()
		dbName := "iterion_conformance_" + bsonNonce(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s, err := New(ctx, Config{
			URI:      uri,
			Database: dbName,
			Blob:     newInMemoryBlob(),
		})
		if err != nil {
			t.Fatalf("mongo New: %v", err)
		}
		t.Cleanup(func() {
			drop, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer dropCancel()
			_ = s.db.Drop(drop)
			_ = s.Close(drop)
		})
		return s
	}, storetest.Opts{InitialStatus: store.RunStatusQueued})
}

// bsonNonce returns a short suffix that makes parallel conformance
// runs land on disjoint databases (Go test packages run sequentially
// per package, but the same package's t.Parallel() subtests can
// otherwise collide).
// bsonNonce returns a per-call unique suffix for a test database name.
//
// It returns the FULL ObjectID hex, not a prefix: the leading 8 hex chars of
// an ObjectID are its timestamp in SECONDS, so a prefix makes every test
// starting within the same second share one database — and therefore each
// other's data. That went unnoticed while no test asserted on a
// platform-wide scan; the retry sweeper's due-list does, and saw its
// neighbours' runs.
func bsonNonce(t *testing.T) string {
	t.Helper()
	return bson.NewObjectID().Hex()
}

// inMemoryBlob is a hash-map blob.Client implementation suitable for
// the conformance suite. Not a public package — only the conformance
// test uses it because the artifacts assertions stress the (run,
// node, version) layout, not S3 semantics.
type inMemoryBlob struct {
	data map[string][]byte
}

func newInMemoryBlob() *inMemoryBlob {
	return &inMemoryBlob{data: make(map[string][]byte)}
}

func (b *inMemoryBlob) PutArtifact(_ context.Context, runID, nodeID string, version int, body []byte) error {
	key, err := blob.ArtifactKey(runID, nodeID, version)
	if err != nil {
		return err
	}
	b.data[key] = append([]byte{}, body...)
	return nil
}

func (b *inMemoryBlob) GetArtifact(_ context.Context, runID, nodeID string, version int) ([]byte, error) {
	key, err := blob.ArtifactKey(runID, nodeID, version)
	if err != nil {
		return nil, err
	}
	body, ok := b.data[key]
	if !ok {
		return nil, blob.ErrArtifactNotFound
	}
	out := make([]byte, len(body))
	copy(out, body)
	return out, nil
}

func (b *inMemoryBlob) ListArtifactVersions(_ context.Context, runID, nodeID string) ([]int, error) {
	prefix := "artifacts/" + runID + "/" + nodeID + "/"
	versions := []int{}
	for k := range b.data {
		if len(k) <= len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		var v int
		// strip ".json" suffix manually to avoid pulling strconv into
		// the test helper's import list.
		tail := k[len(prefix):]
		if len(tail) <= len(".json") {
			continue
		}
		tail = tail[:len(tail)-len(".json")]
		for _, c := range tail {
			if c < '0' || c > '9' {
				v = -1
				break
			}
			v = v*10 + int(c-'0')
		}
		if v >= 0 {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return nil, blob.ErrArtifactNotFound
	}
	return versions, nil
}

func (b *inMemoryBlob) DeleteRun(_ context.Context, runID string) error {
	prefix := "artifacts/" + runID + "/"
	for k := range b.data {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(b.data, k)
		}
	}
	return nil
}

func (b *inMemoryBlob) Ping(_ context.Context) error { return nil }

func (b *inMemoryBlob) Close() error { return nil }

func (b *inMemoryBlob) PutAttachment(_ context.Context, runID, name, filename, contentType string, body []byte) error {
	key, err := blob.AttachmentKey(runID, name, filename)
	if err != nil {
		return err
	}
	b.data[key] = append([]byte{}, body...)
	return nil
}

func (b *inMemoryBlob) GetAttachment(_ context.Context, runID, name, filename string) (io.ReadCloser, blob.AttachmentMeta, error) {
	key, err := blob.AttachmentKey(runID, name, filename)
	if err != nil {
		return nil, blob.AttachmentMeta{}, err
	}
	body, ok := b.data[key]
	if !ok {
		return nil, blob.AttachmentMeta{}, blob.ErrArtifactNotFound
	}
	rc := io.NopCloser(bytes.NewReader(body))
	return rc, blob.AttachmentMeta{Size: int64(len(body))}, nil
}

func (b *inMemoryBlob) PresignAttachment(_ context.Context, runID, name, filename string, _ time.Duration) (string, error) {
	key, err := blob.AttachmentKey(runID, name, filename)
	if err != nil {
		return "", err
	}
	return "memory://" + key, nil
}

func (b *inMemoryBlob) DeleteAttachment(_ context.Context, runID, name, filename string) error {
	key, err := blob.AttachmentKey(runID, name, filename)
	if err != nil {
		return err
	}
	delete(b.data, key)
	return nil
}

func (b *inMemoryBlob) DeleteRunAttachments(_ context.Context, runID string) error {
	prefix, err := blob.AttachmentRunPrefix(runID)
	if err != nil {
		return err
	}
	for k := range b.data {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(b.data, k)
		}
	}
	return nil
}

func (b *inMemoryBlob) PutToolBlob(_ context.Context, runID, toolUseID, kind string, body []byte) error {
	key, err := blob.ToolBlobKey(runID, toolUseID, kind)
	if err != nil {
		return err
	}
	b.data[key] = append([]byte{}, body...)
	return nil
}

func (b *inMemoryBlob) GetToolBlobRange(_ context.Context, runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	key, err := blob.ToolBlobKey(runID, toolUseID, kind)
	if err != nil {
		return nil, 0, false, err
	}
	body, ok := b.data[key]
	if !ok {
		return nil, 0, false, blob.ErrArtifactNotFound
	}
	total := int64(len(body))
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return nil, total, true, nil
	}
	readLen := total - offset
	if limit > 0 && limit < readLen {
		readLen = limit
	}
	out := make([]byte, readLen)
	copy(out, body[offset:offset+readLen])
	eof := offset+readLen >= total
	return out, total, eof, nil
}

func (b *inMemoryBlob) DeleteRunToolBlobs(_ context.Context, runID string) error {
	prefix, err := blob.ToolBlobRunPrefix(runID)
	if err != nil {
		return err
	}
	for k := range b.data {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(b.data, k)
		}
	}
	return nil
}

func (b *inMemoryBlob) PutRunFile(_ context.Context, runID, relPath, _ string, body io.Reader, size int64) error {
	key, err := blob.RunFileKey(runID, relPath)
	if err != nil {
		return err
	}
	payload, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(payload)) != size {
		return fmt.Errorf("run file size = %d, want %d", len(payload), size)
	}
	b.data[key] = append([]byte{}, payload...)
	return nil
}

func (b *inMemoryBlob) ListRunFiles(_ context.Context, runID string) ([]blob.RunFileObject, error) {
	prefix, err := blob.RunFileRunPrefix(runID)
	if err != nil {
		return nil, err
	}
	var out []blob.RunFileObject
	for k, v := range b.data {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, blob.RunFileObject{Path: k[len(prefix):], Size: int64(len(v))})
		}
	}
	return out, nil
}

func (b *inMemoryBlob) GetRunFile(_ context.Context, runID, relPath string) (io.ReadCloser, blob.RunFileObject, error) {
	key, err := blob.RunFileKey(runID, relPath)
	if err != nil {
		return nil, blob.RunFileObject{}, err
	}
	body, ok := b.data[key]
	if !ok {
		return nil, blob.RunFileObject{}, blob.ErrArtifactNotFound
	}
	prefix, _ := blob.RunFileRunPrefix(runID)
	rc := io.NopCloser(bytes.NewReader(body))
	return rc, blob.RunFileObject{Path: key[len(prefix):], Size: int64(len(body))}, nil
}

func (b *inMemoryBlob) DeleteRunFiles(_ context.Context, runID string) error {
	prefix, err := blob.RunFileRunPrefix(runID)
	if err != nil {
		return err
	}
	for k := range b.data {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(b.data, k)
		}
	}
	return nil
}

func (b *inMemoryBlob) PutIRBlob(_ context.Context, runID string, body []byte) error {
	key, err := blob.IRBlobKey(runID)
	if err != nil {
		return err
	}
	b.data[key] = append([]byte{}, body...)
	return nil
}

func (b *inMemoryBlob) GetIRBlob(_ context.Context, storageKey string) ([]byte, error) {
	body, ok := b.data[storageKey]
	if !ok {
		return nil, blob.ErrArtifactNotFound
	}
	return append([]byte{}, body...), nil
}

func (b *inMemoryBlob) DeleteRunIR(_ context.Context, runID string) error {
	key, err := blob.IRBlobKey(runID)
	if err != nil {
		return err
	}
	delete(b.data, key)
	return nil
}
