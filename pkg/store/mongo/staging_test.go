package mongo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A staged upload belongs to the tenant that staged it: another tenant's
// launch does not find it, cannot discard it, and the platform sweep sees
// it with no tenant at all. The shared contract is storetest's
// StagingStore case; this is the half only a tenant-scoped store has.
// Skipped unless ITERION_TEST_MONGO_URI is set (a real replica set).
func TestStagedUploadsAreScopedToTheTenantThatStagedThem(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo staging isolation")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{URI: uri, Database: "iterion_staging_" + bsonNonce(t), Blob: newInMemoryBlob()})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	teamA := store.WithIdentity(ctx, "team-a", "user-a")
	teamB := store.WithIdentity(ctx, "team-b", "user-b")

	body := []byte("bytes of a")
	rec := store.StagedUpload{UploadID: "up_1700000000_00000000000000aa", OriginalFilename: "a.txt", MIME: "text/plain", Size: int64(len(body)), SHA256: "a"}
	if err := s.StageUpload(teamA, rec, bytes.NewReader(body)); err != nil {
		t.Fatalf("StageUpload: %v", err)
	}
	var stored store.StagedUpload
	if err := s.stagedUploads.FindOne(ctx, bson.M{"_id": rec.UploadID}).Decode(&stored); err != nil {
		t.Fatalf("the record is not in staged_uploads: %v", err)
	}
	if stored.TenantID != "team-a" || stored.OwnerID != "user-a" {
		t.Fatalf("the record is stamped %q/%q, want team-a/user-a", stored.TenantID, stored.OwnerID)
	}
	if _, _, err := s.OpenStagedUpload(teamB, rec.UploadID); !errors.Is(err, store.ErrUploadNotFound) {
		t.Fatalf("another tenant opened the upload: %v", err)
	}
	if err := s.DiscardStagedUpload(teamB, rec.UploadID); err != nil {
		t.Fatalf("another tenant's discard errs: %v", err)
	}
	if _, rc, err := s.OpenStagedUpload(teamA, rec.UploadID); err != nil {
		t.Fatalf("another tenant's discard took the upload away: %v", err)
	} else {
		_ = rc.Close()
	}
	if n, err := s.ReapStagedUploads(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("the platform sweep reaped %d, %v; want 1", n, err)
	}
	if _, _, err := s.OpenStagedUpload(teamA, rec.UploadID); !errors.Is(err, store.ErrUploadNotFound) {
		t.Fatalf("the upload survived the sweep: %v", err)
	}
	if _, ok := s.blob.(*inMemoryBlob).data["uploads/"+rec.UploadID+"/a.txt"]; ok {
		t.Fatal("the bytes survived the sweep")
	}
}

// storeThenFailBlob stores the object and then reports the PUT as failed —
// the shape of a connection dropped after the write.
type storeThenFailBlob struct{ *inMemoryBlob }

func (b *storeThenFailBlob) PutUpload(ctx context.Context, uploadID, filename, ct string, body io.ReadSeeker, size int64) error {
	if err := b.inMemoryBlob.PutUpload(ctx, uploadID, filename, ct, body, size); err != nil {
		return err
	}
	return errors.New("connection reset after the write")
}

// A PUT that failed after storing leaves no bytes behind: the record and
// the bytes go together, or the record stays for the sweep — never bytes
// with no record, which nothing would ever reap.
func TestAFailedPutLeavesNoBytesBehind(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo staging failure")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	fake := &storeThenFailBlob{inMemoryBlob: newInMemoryBlob()}
	s, err := New(ctx, Config{URI: uri, Database: "iterion_stagingfail_" + bsonNonce(t), Blob: fake})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	team := store.WithIdentity(ctx, "team-a", "user-a")
	rec := store.StagedUpload{UploadID: "up_1700000000_00000000000000ff", OriginalFilename: "f.txt", MIME: "text/plain", Size: 1, SHA256: "f"}
	if err := s.StageUpload(team, rec, bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("a failed put was reported as staged")
	}
	if _, ok := fake.data["uploads/"+rec.UploadID+"/f.txt"]; ok {
		t.Fatal("the bytes of a failed put stayed in the bucket with no record")
	}
	if n, err := s.stagedUploads.CountDocuments(ctx, bson.M{"_id": rec.UploadID}); err != nil || n != 0 {
		t.Fatalf("records left: %d, %v", n, err)
	}
}
