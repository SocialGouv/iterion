package platformcfg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The CAS twin conformance for the tenant routing-policy records: a
// write must land only when the caller's stamp still matches, and a
// lost race must be a loud (false, nil) — the handlers surface it as
// 409 instead of silently dropping the concurrent editor's policy. THE
// MONGO TWIN IS THE ONE THAT MUST REDDEN under a dropped
// stampUpdatedAt arm (the unstamped doc still marshals updated_at as
// zero time — not omitempty — so the first-write `$exists:false`
// filter never matches); the Memory twin cannot redden there (a
// zero-expected always matches the zero updatedAtOf returns).
func TestRoutingPolicyRecord_CASConformance(t *testing.T) {
	policy := llmroute.Policy{PairOrder: []string{"claw+zai_key"}}
	newRec := func() RoutingPolicyRecord {
		return RoutingPolicyRecord{Policy: &policy, UpdatedBy: "tester"}
	}

	// ---- memory twin ----
	mem := NewMemoryStore[RoutingPolicyRecord]()
	// First write: the zero stamp means "no document existed" and the
	// write must land.
	wrote, err := mem.PutIfUnchanged(context.Background(), newRec(), time.Time{})
	if err != nil || !wrote {
		t.Fatalf("first write: wrote=%v err=%v", wrote, err)
	}
	// Matching stamp lands; a stale one loses loudly.
	cur, _ := mem.Get(context.Background())
	if wrote, err = mem.PutIfUnchanged(context.Background(), newRec(), cur.UpdatedAt); err != nil || !wrote {
		t.Fatalf("matching write: wrote=%v err=%v", wrote, err)
	}
	if wrote, err = mem.PutIfUnchanged(context.Background(), newRec(), time.Time{}); err != nil || wrote {
		t.Fatalf("stale zero stamp must lose: wrote=%v err=%v", wrote, err)
	}

	// ---- mongo twin (the conformance harness's own gating) ----
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the Mongo CAS suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_route_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})

	mg := NewMongoScoped[RoutingPolicyRecord](db, ColRoutingPolicies, TeamRoutingPolicyID("t1"))
	wrote, err = mg.PutIfUnchanged(ctx, newRec(), time.Time{})
	if err != nil || !wrote {
		t.Fatalf("mongo first write: wrote=%v err=%v", wrote, err)
	}
	cur, err = mg.Get(ctx)
	if err != nil || cur == nil {
		t.Fatalf("mongo get after first write: %v", err)
	}
	if wrote, err = mg.PutIfUnchanged(ctx, newRec(), cur.UpdatedAt); err != nil || !wrote {
		t.Fatalf("mongo matching write: wrote=%v err=%v", wrote, err)
	}
	if wrote, err = mg.PutIfUnchanged(ctx, newRec(), time.Time{}); err != nil || wrote {
		t.Fatalf("mongo stale zero stamp must lose: wrote=%v err=%v", wrote, err)
	}

	// Clearing an ABSENT document is idempotent, not a lost race: zero
	// stamp + absent → (true, nil) on BOTH twins (the mongo arm reds on
	// the pre-fix code, whose cold path answered false → a 409 forever
	// after the first clear). A document APPEARING in the window stays a
	// lost race — the route test's armed seam guards that side.
	if err := mg.Delete(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if wrote, err = mg.DeleteIfUnchanged(ctx, time.Time{}); err != nil || !wrote {
		t.Fatalf("clear of an absent doc must be idempotent: wrote=%v err=%v", wrote, err)
	}
	if err := mem.Delete(context.Background()); err != nil {
		t.Fatalf("memory reset: %v", err)
	}
	if wrote, err = mem.DeleteIfUnchanged(context.Background(), time.Time{}); err != nil || !wrote {
		t.Fatalf("memory twin: clear of an absent doc must be idempotent: wrote=%v err=%v", wrote, err)
	}

	// The CAS clear: DeleteIfUnchanged mirrors the replace's discipline —
	// a stale stamp loses loudly, the matching one clears. The doc is
	// gone from the idempotent-clear block above; write it again.
	if _, err := mg.PutIfUnchanged(ctx, newRec(), time.Time{}); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	cur, err = mg.Get(ctx)
	if err != nil || cur == nil {
		t.Fatalf("get before CAS clear: %v", err)
	}
	if wrote, err = mg.DeleteIfUnchanged(ctx, time.Time{}); err != nil || wrote {
		t.Fatalf("stale-zero CAS clear must lose: wrote=%v err=%v", wrote, err)
	}
	if wrote, err = mg.DeleteIfUnchanged(ctx, cur.UpdatedAt); err != nil || !wrote {
		t.Fatalf("matching CAS clear: wrote=%v err=%v", wrote, err)
	}
	gone2, err := mg.Get(ctx)
	if err != nil || gone2 != nil {
		t.Fatalf("after CAS clear: rec=%v err=%v, want nil/nil", gone2, err)
	}

	// The clear-outright write: Delete removes the document, and the
	// next read answers absence (nil, nil) — the fold's "level unset".
	var st Store[RoutingPolicyRecord] = mg
	del, ok := st.(Deleter)
	if !ok {
		t.Fatal("the mongo store must implement Deleter")
	}
	if err := del.Delete(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	gone, err := mg.Get(ctx)
	if err != nil || gone != nil {
		t.Fatalf("after delete: rec=%v err=%v, want nil/nil", gone, err)
	}
}
