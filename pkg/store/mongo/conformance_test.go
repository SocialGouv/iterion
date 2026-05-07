package mongo

import (
	"bytes"
	"context"
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
func bsonNonce(t *testing.T) string {
	t.Helper()
	id := bson.NewObjectID().Hex()
	if len(id) < 8 {
		return id
	}
	return id[:8]
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
	b.data[blob.ArtifactKey(runID, nodeID, version)] = append([]byte{}, body...)
	return nil
}

func (b *inMemoryBlob) GetArtifact(_ context.Context, runID, nodeID string, version int) ([]byte, error) {
	key := blob.ArtifactKey(runID, nodeID, version)
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
	b.data[blob.AttachmentKey(runID, name, filename)] = append([]byte{}, body...)
	return nil
}

func (b *inMemoryBlob) GetAttachment(_ context.Context, runID, name, filename string) (io.ReadCloser, blob.AttachmentMeta, error) {
	key := blob.AttachmentKey(runID, name, filename)
	body, ok := b.data[key]
	if !ok {
		return nil, blob.AttachmentMeta{}, blob.ErrArtifactNotFound
	}
	rc := io.NopCloser(bytes.NewReader(body))
	return rc, blob.AttachmentMeta{Size: int64(len(body))}, nil
}

func (b *inMemoryBlob) PresignAttachment(_ context.Context, runID, name, filename string, _ time.Duration) (string, error) {
	return "memory://" + blob.AttachmentKey(runID, name, filename), nil
}

func (b *inMemoryBlob) DeleteRunAttachments(_ context.Context, runID string) error {
	prefix := blob.AttachmentRunPrefix(runID)
	for k := range b.data {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(b.data, k)
		}
	}
	return nil
}
