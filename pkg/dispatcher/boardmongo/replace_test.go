package boardmongo_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// TestReplacePreservesUnknownIssueFields pins the mixed-fleet contract of
// the store's full-issue write path: a binary must never erase issue
// subdocument fields it does not know about. Under the historical
// ReplaceOne this test is RED — a newer binary's field (here the stand-in
// zz_future_field, tomorrow a claim lease) vanished on the older binary's
// first SetLastRun/SetState/Update, silently disarming whatever feature
// owned it.
func TestReplacePreservesUnknownIssueFields(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo board suite")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_board_replace_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dc := context.WithTimeout(context.Background(), 10*time.Second)
		defer dc()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	if err := boardmongo.EnsureSchema(ctx, db); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	st := boardmongo.New(db, "tenant-mixed-fleet")

	iss, err := st.Create(native.Issue{Title: "mixed-fleet probe", Body: "b"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A NEWER binary wrote a field this one's struct does not carry.
	issues := db.Collection(boardmongo.IssuesCollection)
	if _, err := issues.UpdateOne(ctx,
		bson.M{"_id": iss.ID},
		bson.M{"$set": bson.M{"issue.zz_future_field": "must-survive"}}); err != nil {
		t.Fatalf("inject future field: %v", err)
	}

	// This binary performs ordinary full-issue mutations through the
	// write path under test.
	if err := st.SetLastRun(iss.ID, "run-123", "/tmp/wd"); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}
	if _, err := st.SetState(iss.ID, "ready"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	var doc bson.Raw
	if err := issues.FindOne(ctx, bson.M{"_id": iss.ID}).Decode(&doc); err != nil {
		t.Fatalf("read raw doc: %v", err)
	}
	if got, ok := doc.Lookup("issue", "zz_future_field").StringValueOK(); !ok || got != "must-survive" {
		t.Fatalf("newer binary's field erased by this binary's write (got %q, ok=%t) — the ReplaceOne regression", got, ok)
	}

	// And the mutations themselves landed with full semantics preserved.
	got, err := st.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRunID != "run-123" || got.State != "ready" {
		t.Fatalf("mutations lost: %+v", got)
	}
}
