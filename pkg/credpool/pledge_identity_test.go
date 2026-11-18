package credpool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Two keys of ONE lender for ONE provider are two distinct offers: the
// donor lends each key deliberately, and each carries its own ceilings and
// concurrency slot. The identity has to say so, or the second write lands on
// the first's row.
func TestTwoAPIKeyPledgesSameLenderRefCoexist(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryPledgeStore()
	upsert := func(keyID string) {
		t.Helper()
		// An empty id: the store computes it, exactly as both write paths do.
		if err := store.Upsert(ctx, Pledge{
			PoolID: "pool-1", UserID: "alice",
			Credential: Credential{Source: SourceAPIKey, Ref: "anthropic", KeyID: keyID},
			Enabled:    true, Health: HealthOK,
		}); err != nil {
			t.Fatalf("upsert key %s: %v", keyID, err)
		}
	}
	upsert("key-1")
	upsert("key-2")

	pledges, err := store.ListByUser(ctx, "alice")
	if err != nil {
		t.Fatalf("list alice's pledges: %v", err)
	}
	if len(pledges) != 2 {
		t.Fatalf("two keys of one lender produced %d pledge(s), want 2 — the second write overwrote the first", len(pledges))
	}
	byKey := map[string]string{}
	for _, p := range pledges {
		byKey[p.KeyID] = p.ID
	}
	for _, keyID := range []string{"key-1", "key-2"} {
		want := "alice|api_key|anthropic|" + keyID
		if got := byKey[keyID]; got != want {
			t.Errorf("pledge of %s has id %q, want %q — the identity does not carry the key", keyID, got, want)
			continue
		}
		got, err := store.Get(ctx, want)
		if err != nil {
			t.Errorf("get pledge %q: %v", want, err)
			continue
		}
		if got.KeyID != keyID {
			t.Errorf("pledge %q names key %q, want %q", want, got.KeyID, keyID)
		}
	}
}

// TestAcquire_failsOverBetweenTwoKeysOfOneLender: when the first of the
// lender's two keys is out (cooling, out of its allowance, parked as
// unhealthy), the broker serves the second — and an abstention names the
// first key's own pledge, never a merged row.
func TestAcquire_failsOverBetweenTwoKeysOfOneLender(t *testing.T) {
	shapes := []struct {
		name string
		// unavailable takes one of the lender's keys out of the pool.
		unavailable func(t *testing.T, h *harness, p Pledge)
		want        Status
	}{
		{"cooldown", func(t *testing.T, h *harness, p Pledge) {
			t.Helper()
			until := h.now.Add(time.Hour)
			p.CooldownUntil = &until
			if err := h.pledges.Upsert(context.Background(), p); err != nil {
				t.Fatalf("park key %s on cooldown: %v", p.KeyID, err)
			}
		}, StatusCooling},
		{"exhausted", func(t *testing.T, h *harness, p Pledge) {
			t.Helper()
			// Consume the single run unit the pledge allows. A second call
			// on an already-consumed pledge merely refuses — either way the
			// entry has nothing left to give.
			if _, _, err := h.ledger.Reserve(context.Background(), p.ID, h.now, p.Limits, LiveCommitment{}); err != nil {
				t.Fatalf("consume key %s's run unit: %v", p.KeyID, err)
			}
		}, StatusExhausted},
		{"unhealthy", func(t *testing.T, h *harness, p Pledge) {
			t.Helper()
			p.Health = HealthAuthFailed
			if err := h.pledges.Upsert(context.Background(), p); err != nil {
				t.Fatalf("park key %s as unhealthy: %v", p.KeyID, err)
			}
		}, StatusUnhealthy},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			keyA, keyB := "key-"+shape.name+"-a", "key-"+shape.name+"-b"
			a := h.donorKeyNamed(t, "alice", "anthropic", keyA, Limits{MaxRunsPerDay: 1})
			b := h.donorKeyNamed(t, "alice", "anthropic", keyB, Limits{MaxRunsPerDay: 1})
			if a.ID == b.ID {
				t.Fatalf("the lender's two keys share the pledge id %q — the second key overwrote the first", a.ID)
			}

			// The first key is out; the second must serve.
			shape.unavailable(t, h, a)
			grant, err := h.broker.Acquire(ctx, h.wantKey("run-1", "anthropic"))
			if err != nil {
				t.Fatalf("the first key is %s and the second is available, yet Acquire = %v", shape.name, err)
			}
			if grant.PledgeID != b.ID {
				t.Fatalf("run-1 served pledge %q, want the second key's %q", grant.PledgeID, b.ID)
			}
			if string(grant.Payload) != "sk-real-"+keyB {
				t.Errorf("run-1 payload = %q, want the second key's plaintext", grant.Payload)
			}

			// With both keys out, the abstention names each key's own pledge
			// and the state that held it out.
			shape.unavailable(t, h, b)
			_, err = h.broker.Acquire(ctx, h.wantKey("run-2", "anthropic"))
			var nde *NoDonorError
			if !errors.As(err, &nde) {
				t.Fatalf("both keys %s: Acquire = %v, want a typed ErrNoDonor carrying the skips", shape.name, err)
			}
			skip, ok := skipOf(nde.Skips, a.ID)
			if !ok {
				t.Fatalf("the abstention names no skip for the first key's pledge %q (skips: %+v)", a.ID, nde.Skips)
			}
			if got := LaunchVerdict(skip.Status, skip.Refuses); got != shape.want {
				t.Errorf("skip of the first key = %q, want %q", got, shape.want)
			}
		})
	}
}

// skipOf finds the skip naming one pledge.
func skipOf(skips []PledgeSkip, pledgeID string) (PledgeSkip, bool) {
	for _, s := range skips {
		if s.PledgeID == pledgeID {
			return s, true
		}
	}
	return PledgeSkip{}, false
}

// donorKeyNamed seeds one of a lender's keys under an explicit key id and
// pledges it with an EMPTY pledge id, so the store's own identity rule is
// what the test judges. donorKey derives one key id per (user, provider) and
// cannot express a lender holding two.
func (h *harness) donorKeyNamed(t *testing.T, userID, provider, keyID string, lim Limits, mutate ...func(*Pledge)) Pledge {
	t.Helper()
	ctx := context.Background()
	plaintext := "sk-real-" + keyID
	sealed, err := secrets.SealAPIKey(h.sealer, keyID, []byte(plaintext))
	if err != nil {
		t.Fatalf("seal key %s: %v", keyID, err)
	}
	if err := h.apiKeys.Create(ctx, secrets.ApiKey{
		ID: keyID, TenantID: "team-1", ScopeTeamID: "team-1", ScopeUserID: userID,
		Provider: secrets.Provider(provider), Name: keyID, SealedSecret: sealed,
		Fingerprint: secrets.FingerprintSHA256(plaintext),
	}); err != nil {
		t.Fatalf("seed key %s: %v", keyID, err)
	}
	p := Pledge{
		PoolID: "pool-1", UserID: userID,
		Credential: Credential{Source: SourceAPIKey, Ref: provider, KeyID: keyID},
		Enabled:    true, Health: HealthOK, Limits: lim,
	}
	for _, m := range mutate {
		m(&p)
	}
	if err := h.pledges.Upsert(ctx, p); err != nil {
		t.Fatalf("seed pledge of key %s: %v", keyID, err)
	}
	all, err := h.pledges.ListByUser(ctx, userID)
	if err != nil {
		t.Fatalf("list %s's pledges: %v", userID, err)
	}
	for _, got := range all {
		if got.KeyID == keyID {
			return got
		}
	}
	t.Fatalf("the pledge of key %s is not among %s's pledges — the store dropped it", keyID, userID)
	return Pledge{}
}

// TestPledgeID_oauthIDsAreUnchanged bounds the identity change's blast
// radius: an oauth pledge keeps the three-segment id it has always had
// (no OAuth-side migration), while an api_key pledge is identified by the
// key it lends.
func TestPledgeID_oauthIDsAreUnchanged(t *testing.T) {
	if got, want := PledgeID("jo", SourceOAuth, "claude_code"), "jo|oauth|claude_code"; got != want {
		t.Errorf("oauth pledge id = %q, want %q — every oauth pledge would need a migration", got, want)
	}
	if got, want := PledgeID("jo", SourceAPIKey, "anthropic"), "jo|api_key|anthropic|key-1"; got != want {
		t.Errorf("api_key pledge id = %q, want %q — two keys of one lender collide", got, want)
	}
}
