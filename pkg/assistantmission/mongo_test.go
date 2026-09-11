package assistantmission

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestMongoStoreMissionUniquenessAndClaimFence(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo assistant mission suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_assistantmission_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, stop := mongotest.TeardownCtx()
		defer stop()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	st := NewMongoStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	first := testMission(now)
	if _, fresh, err := st.CreateOrGet(ctx, first); err != nil || !fresh {
		t.Fatalf("create fresh=%v err=%v", fresh, err)
	}
	other := testMission(now)
	other.ID, other.InvocationKey, other.OperatorID = "m2", "other", "other-operator"
	if _, _, err := st.CreateOrGet(ctx, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("second active target = %v, want conflict", err)
	}
	claimed, won, err := st.Claim(ctx, first.ID, "worker", now, time.Minute)
	if err != nil || !won {
		t.Fatalf("claim won=%v err=%v", won, err)
	}
	stale := claimed
	stale.Revision--
	if _, err := st.UpdateClaimed(ctx, stale, "worker"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("stale update = %v, want claim lost", err)
	}
	if _, err := st.RequestStop(ctx, Scope{TenantID: "t", OperatorID: "o", TargetRunID: "r1"}, first.ID, "done", now); err != nil {
		t.Fatal(err)
	}
	reattach := first
	reattach.ID = "new-id"
	reattach.Policy.ExpiresAt = now.Add(time.Hour)
	got, fresh, err := st.CreateOrGet(ctx, reattach)
	if err != nil || fresh || got.ID != first.ID || got.State != StateStopped {
		t.Fatalf("terminal reattach = %#v fresh=%v err=%v", got, fresh, err)
	}
}
