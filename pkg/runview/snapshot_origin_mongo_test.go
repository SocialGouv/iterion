package runview

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// TestSnapshotOriginNativeMongo runs the snapshot origin matrix against the
// native Mongo store; CI's mongo-conformance job supplies the URI.
func TestSnapshotOriginNativeMongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	for _, f := range []store.ValueFormat{1, 2} {
		for _, mode := range snapshotOriginModes {
			t.Run(fmt.Sprintf("f%d/%s", f, mode), func(t *testing.T) {
				ctx := store.WithIdentity(t.Context(), "snapshot-tenant", "owner")
				s, err := mongostore.New(ctx, mongostore.Config{URI: uri, Database: fmt.Sprintf("snapshot_%d", time.Now().UnixNano()), Blob: snapshotExactBlob{}, RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
					defer cancel()
					_ = s.DB().Drop(c)
					_ = s.Close(c)
				})
				snapshotOriginMatrix(t, s, ctx, f, mode)
			})
		}
	}
}

type snapshotBlob struct{}

func (snapshotBlob) PutArtifact(context.Context, string, string, int, []byte) error { return nil }
func (snapshotBlob) GetArtifact(context.Context, string, string, int) ([]byte, error) {
	return nil, fmt.Errorf("snapshotBlob: not found")
}
func (snapshotBlob) ListArtifactVersions(context.Context, string, string) ([]int, error) {
	return nil, nil
}
func (snapshotBlob) DeleteRun(context.Context, string) error { return nil }
func (snapshotBlob) Ping(context.Context) error              { return nil }
func (snapshotBlob) Close() error                            { return nil }
func (snapshotBlob) PutAttachment(context.Context, string, string, string, string, []byte) error {
	return nil
}
func (snapshotBlob) GetAttachment(context.Context, string, string, string) (io.ReadCloser, blob.AttachmentMeta, error) {
	return nil, blob.AttachmentMeta{}, fmt.Errorf("snapshotBlob: not found")
}
func (snapshotBlob) PresignAttachment(context.Context, string, string, string, time.Duration) (string, error) {
	return "", fmt.Errorf("snapshotBlob: unsupported")
}
func (snapshotBlob) DeleteAttachment(context.Context, string, string, string) error { return nil }
func (snapshotBlob) DeleteRunAttachments(context.Context, string) error             { return nil }
func (snapshotBlob) PutToolBlob(context.Context, string, string, string, []byte) error {
	return nil
}
func (snapshotBlob) GetToolBlobRange(context.Context, string, string, string, int64, int64) ([]byte, int64, bool, error) {
	return nil, 0, false, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteRunToolBlobs(context.Context, string) error { return nil }
func (snapshotBlob) PutRunFile(context.Context, string, string, string, io.Reader, int64) error {
	return nil
}
func (snapshotBlob) ListRunFiles(context.Context, string) ([]blob.RunFileObject, error) {
	return nil, nil
}
func (snapshotBlob) GetRunFile(context.Context, string, string) (io.ReadCloser, blob.RunFileObject, error) {
	return nil, blob.RunFileObject{}, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteRunFiles(context.Context, string) error { return nil }
func (snapshotBlob) PutIRBlob(context.Context, string, []byte) error {
	return nil
}
func (snapshotBlob) GetIRBlob(context.Context, string) ([]byte, error) {
	return nil, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteRunIR(context.Context, string) error { return nil }
func (snapshotBlob) PutBackendSession(context.Context, string, string, []byte) error {
	return nil
}
func (snapshotBlob) GetBackendSession(context.Context, string, string) ([]byte, error) {
	return nil, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteBackendSession(context.Context, string, string) error { return nil }
func (snapshotBlob) PutUpload(context.Context, string, string, string, io.ReadSeeker, int64) error {
	return nil
}
func (snapshotBlob) GetUpload(context.Context, string, string) (io.ReadCloser, blob.AttachmentMeta, error) {
	return nil, blob.AttachmentMeta{}, blob.ErrArtifactNotFound
}
func (snapshotBlob) DeleteUpload(context.Context, string) error             { return nil }
func (snapshotBlob) DeleteRunBackendSessions(context.Context, string) error { return nil }

var _ blob.Client = snapshotBlob{}

type snapshotExactBlob struct{ snapshotBlob }

func (b snapshotExactBlob) PutArtifactForFormat(c context.Context, _ int, r, n string, v int, d []byte) error {
	return b.PutArtifact(c, r, n, v, d)
}
func (b snapshotExactBlob) GetArtifactForFormat(c context.Context, _ int, r, n string, v int) ([]byte, error) {
	return b.GetArtifact(c, r, n, v)
}
func (b snapshotExactBlob) ListArtifactVersionsForFormat(c context.Context, _ int, r, n string) ([]int, error) {
	return b.ListArtifactVersions(c, r, n)
}
func (b snapshotExactBlob) DeleteRunArtifactsForFormat(c context.Context, _ int, r string) error {
	return b.DeleteRun(c, r)
}
func (b snapshotExactBlob) DeleteRunArtifacts(c context.Context, r string) error {
	return b.DeleteRun(c, r)
}
func (b snapshotExactBlob) RunObjectsForFormat(int) (blob.RunObjects, error) { return b, nil }
func (b snapshotExactBlob) RunObjectsForScope(store.IncarnationDataScope) (blob.RunObjects, error) {
	return b, nil
}
func (b snapshotExactBlob) ArtifactsForScope(store.IncarnationDataScope) (blob.ArtifactObjects, error) {
	return b, nil
}
