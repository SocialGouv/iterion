package mongo

import (
	"bytes"
	"os"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/knowledge"
)

// TestValidateCloudTenant locks in C1: the Mongo memory store is the cloud
// adapter, so every NON-global space must carry a TenantID. Runs on the host —
// the guard fires before any Mongo I/O. See the 2026-06-15 follow-up in
// docs/bot-runs/whole-improve-loop.md.
func TestValidateCloudTenant(t *testing.T) {
	cases := []struct {
		name    string
		ref     knowledge.SpaceRef
		wantErr bool
	}{
		{"org without tenant -> reject", knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, Name: "notes"}, true},
		{"org with tenant -> ok", knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, Name: "notes", TenantID: "acme"}, false},
		{"user without tenant -> reject", knowledge.SpaceRef{Visibility: knowledge.VisibilityUser, Name: "n", UserID: "u1"}, true},
		{"user with tenant -> ok", knowledge.SpaceRef{Visibility: knowledge.VisibilityUser, Name: "n", UserID: "u1", TenantID: "acme"}, false},
		{"project without tenant -> reject", knowledge.SpaceRef{Visibility: knowledge.VisibilityProject, Name: "n", ProjectID: "p1"}, true},
		{"global without tenant -> ok", knowledge.SpaceRef{Visibility: knowledge.VisibilityGlobal, Name: "catalogue"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCloudTenant(tc.ref)
			if tc.wantErr && err == nil {
				t.Fatalf("validateCloudTenant(%+v) = nil, want a fail-close error", tc.ref)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateCloudTenant(%+v) = %v, want nil", tc.ref, err)
			}
		})
	}
	// An entry point fails closed too (Root validates before any Mongo I/O, so a
	// zero-value store is fine here).
	if _, err := (&MongoMemoryStore{}).Root(knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, Name: "x"}); err == nil {
		t.Fatal("Root with a tenant-less org ref: expected fail-close, got nil")
	}
}

// TestWriteDocumentConcurrent_Mongo locks in C2: WriteDocument is a
// compare-and-swap on `revision`, so concurrent writers to the same
// (space, path) serialize instead of collapsing into one revision (lost
// updates) with double-counted quota bumps (counter drift).
//
// Mongo-gated, like TestConformance_Mongo — skipped unless
// ITERION_TEST_MONGO_URI is set. A standalone mongod is enough (no replica
// set): the CAS uses only ReplaceOne+upsert and the unique _id, not
// transactions/change-streams. Run with:
//
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27017' \
//	    devbox run -- go test ./pkg/store/mongo/ -run WriteDocumentConcurrent
func TestWriteDocumentConcurrent_Mongo(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo concurrency test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()

	cli, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	db := cli.Database("iterion_memtest_" + bsonNonce(t))
	t.Cleanup(func() {
		dctx, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = db.Drop(dctx)
		_ = cli.Disconnect(dctx)
	})

	s := NewMongoMemoryStore(db)
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	ref := knowledge.SpaceRef{Visibility: knowledge.VisibilityOrg, Name: "concurrent", TenantID: "acme"}
	// Identical, fixed-size payload for every writer: the surviving revision's
	// size is then deterministic (no matter which writer wins), so the usage
	// assertion is exact. The first-write race (every writer reads prevRev=0 and
	// would bump +len) is what the old unconditional ReplaceOne double-counted.
	content := bytes.Repeat([]byte("x"), 256)
	const writers = 12

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.WriteDocument(ctx, ref, knowledge.DocumentInput{
				Path:      "race.md",
				Content:   content,
				UpdatedBy: "w",
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("writer %d: %v", i, e)
		}
	}

	// Every write must have landed as a distinct revision — no collapse.
	final, err := s.ReadDocument(ctx, ref, "race.md")
	if err != nil {
		t.Fatalf("ReadDocument: %v", err)
	}
	if final.Meta.Revision != int64(writers) {
		t.Errorf("final revision = %d, want %d (concurrent writes collapsed — lost updates)", final.Meta.Revision, writers)
	}
	// The space usage counter must equal exactly the surviving doc size — not an
	// inflated multiple of it from racing attempts that each bumped the counter.
	used, _, err := s.UsageBytes(ctx, ref)
	if err != nil {
		t.Fatalf("UsageBytes: %v", err)
	}
	if used != final.Meta.Size {
		t.Errorf("space usage = %d, want %d (final doc size) — quota counter drifted under concurrency", used, final.Meta.Size)
	}
}
