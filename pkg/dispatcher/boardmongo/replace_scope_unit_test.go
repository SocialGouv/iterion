package boardmongo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// In-package on purpose: the refusals below fire BEFORE any Mongo call,
// and no exported caller names these keys (that is the point) — the only
// way to pin the guard is to call replace directly.

// A caller naming a claim-owned key must be refused LOUDLY: the skip in
// the $set builder would otherwise drop the caller's own write in
// silence — a fenced-family write smuggled through the unfenced path,
// lost with every suite green.
func TestReplace_ClaimOwnedKeyIsRefusedLoudly(t *testing.T) {
	s := &Store{}
	iss := &native.Issue{ID: "native:1", Title: "x"}
	for _, k := range []string{"claim", "claimepoch", "claimedat", "claimleaseuntil", "claim_epoch", "claim_lease_until"} {
		err := s.replace(context.Background(), iss, k)
		if err == nil || !strings.Contains(err.Error(), "claim-owned") {
			t.Fatalf("replace(%q) = %v, want a loud claim-owned refusal — a silent skip loses the caller's write", k, err)
		}
	}
}

// A typo'd key would silently write nothing — the caller's own mutation
// lost with no error, worse than the clobber the scoping replaced.
func TestReplace_UnknownKeyIsRefusedLoudly(t *testing.T) {
	s := &Store{}
	err := s.replace(context.Background(), &native.Issue{ID: "native:1"}, "labelz")
	if err == nil || !strings.Contains(err.Error(), "unknown issue field") {
		t.Fatalf("replace with a typo'd key = %v, want a loud unknown-field refusal", err)
	}
}

// The admin label sweeps transform a listAll SNAPSHOT that ages for the
// whole walk — an unguarded write re-applied it, resurrecting a one-shot
// label the trigger spine had atomically consumed in the window (a lost
// update: the sweep has NO intent about that label). The guarded retry
// re-reads and re-transforms, so the consume survives the rename.
// Deterministic: the consume fires inside the transform's first call —
// exactly between the sweep's read and its write.
func TestLabelSweep_DoesNotResurrectAConsumedOneShot(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("sweepguard_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	})
	if err := EnsureSchema(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := New(db, "t1")
	iss, err := s.Create(native.Issue{Title: "x", State: native.StateReady,
		Labels: []string{"old", "triage:auto"}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	rename := func(labels []string) ([]string, bool) {
		calls++
		if calls == 1 {
			// The atomic one-shot consume lands between the sweep's read
			// and its write.
			if ok, cerr := s.ConsumeLabels(iss.ID, []string{"triage:auto"}); cerr != nil || !ok {
				t.Fatalf("consume: ok=%v err=%v", ok, cerr)
			}
		}
		out := make([]string, 0, len(labels))
		changed := false
		for _, l := range labels {
			if l == "old" {
				out, changed = append(out, "new"), true
				continue
			}
			out = append(out, l)
		}
		return out, changed
	}
	if _, err := s.applyLabelRewrite(ctx, rename, native.EvtIssueUpdated, map[string]any{"op": "rename"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range got.Labels {
		if l == "triage:auto" {
			t.Fatalf("the sweep resurrected the consumed one-shot: labels=%v — the subscription it had spent re-fires", got.Labels)
		}
	}
	found := false
	for _, l := range got.Labels {
		if l == "new" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the rename itself must still land: labels=%v", got.Labels)
	}
	if calls < 2 {
		t.Fatalf("transform called %d time(s) — the guard miss must re-read and re-transform, not re-apply the stale slice", calls)
	}
}
