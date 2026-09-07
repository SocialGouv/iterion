package pat

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// TestMongoStore runs the shared suite against a real Mongo (same
// gating as the pkg/store/mongo conformance harness).
func TestMongoStore(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo pat suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_pat_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema (second): %v", err)
	}
	runStoreSuite(t, NewMongoStore(db))
}
