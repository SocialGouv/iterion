package credpool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// All unimplemented interface methods panic through their nil embedding.
// Thus a preview that starts reserving, touching fairness, writing leases,
// refreshing records, or opening secrets fails before the live oracle runs.
type previewPools struct {
	PoolStore
	inner PoolStore
}

func (s previewPools) ListEnabled(c context.Context) ([]Pool, error) { return s.inner.ListEnabled(c) }

type previewPledges struct {
	PledgeStore
	inner PledgeStore
}

func (s previewPledges) ListByPool(c context.Context, id string) ([]Pledge, error) {
	return s.inner.ListByPool(c, id)
}

type previewLeases struct {
	LeaseStore
	inner LeaseStore
}

func (s previewLeases) LiveCommitment(c context.Context, id, run string, t time.Time) (int, float64, error) {
	return s.inner.LiveCommitment(c, id, run, t)
}

type previewLedger struct {
	Ledger
	inner Ledger
}

func (s previewLedger) Usage(c context.Context, id string, t time.Time) (Usage, Usage, error) {
	return s.inner.Usage(c, id, t)
}
func (s previewLedger) UsageMany(c context.Context, ids []string, t time.Time) (map[string]Usage, error) {
	return s.inner.UsageMany(c, ids, t)
}

type previewOAuth struct {
	secrets.OAuthStore
	inner secrets.OAuthStore
}

func (s previewOAuth) Get(c context.Context, id string, k secrets.OAuthKind) (secrets.OAuthRecord, error) {
	return s.inner.Get(c, id, k)
}

type previewKeys struct {
	secrets.ApiKeyStore
	inner secrets.ApiKeyStore
}

func (s previewKeys) GetOwned(c context.Context, id, u string) (secrets.ApiKey, error) {
	return s.inner.GetOwned(c, id, u)
}

type previewSealer struct{ secrets.Sealer }

func TestPreviewMatchesActualAdmissionWithoutOpeningOrWriting(t *testing.T) {
	for _, scenario := range []string{"fairness", "full_concurrency", "exhausted_runs", "uncapped", "expired_key", "audience"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t)
			ctx := t.Context()
			lim := Limits{MaxUSDPerDay: 12, MaxConcurrentRuns: 3}
			if scenario == "uncapped" {
				lim = Limits{}
			}
			a := h.donor(t, "private-alice@example.invalid", lim)
			h.donor(t, "private-bob@example.invalid", lim)
			req := h.request("new-run")
			switch scenario {
			case "fairness":
				_, _, err := h.ledger.Reserve(ctx, a.ID, h.now, lim, LiveCommitment{})
				if err != nil {
					t.Fatal(err)
				}
				when := h.now.Add(-time.Minute)
				a.LastServedAt = &when
				if err := h.pledges.Upsert(ctx, a); err != nil {
					t.Fatal(err)
				}
			case "full_concurrency":
				for _, id := range []string{"first", "second", "third", "fourth", "fifth", "sixth"} {
					if _, err := h.broker.Acquire(ctx, h.request(id)); err != nil {
						t.Fatal(err)
					}
				}
			case "exhausted_runs":
				a.Limits.MaxRunsPerDay = 1
				if err := h.pledges.Upsert(ctx, a); err != nil {
					t.Fatal(err)
				}
				if _, _, err := h.ledger.Reserve(ctx, a.ID, h.now, a.Limits, LiveCommitment{}); err != nil {
					t.Fatal(err)
				}
			case "expired_key":
				_, keyID := h.donorKey(t, "private-carol@example.invalid", "anthropic", lim)
				k, err := h.apiKeys.GetOwned(ctx, keyID, "private-carol@example.invalid")
				if err != nil {
					t.Fatal(err)
				}
				expired := h.now
				k.ExpiresAt = &expired
				if err := h.apiKeys.Update(ctx, k); err != nil {
					t.Fatal(err)
				}
				req.Wants = []Credential{{Source: SourceAPIKey, Ref: "anthropic"}}
			case "audience":
				req.OrgID = "outside"
			}
			b := *h.broker
			b.pools = previewPools{inner: h.pools}
			b.pledges = previewPledges{inner: h.pledges}
			b.leases = previewLeases{inner: h.leases}
			b.ledger = previewLedger{inner: h.ledger}
			b.oauth = previewOAuth{inner: h.oauth}
			b.apiKeys = previewKeys{inner: h.apiKeys}
			b.sealer = previewSealer{}
			view, err := b.Preview(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			var selected *PreviewCandidate
			for i := range view.Candidates {
				if view.Candidates[i].Selected {
					selected = &view.Candidates[i]
				}
			}
			data, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"private-alice", "private-bob", "private-carol", "sk-ant", "pledge_id", "key_id"} {
				if strings.Contains(string(data), private) {
					t.Fatalf("preview exposed %s", private)
				}
			}
			actual, err := h.broker.Acquire(ctx, req)
			if selected == nil {
				if actual != nil || err == nil {
					t.Fatalf("preview abstained but live admission returned %+v, %v", actual, err)
				}
				return
			}
			if err != nil || actual == nil {
				t.Fatalf("preview selected %s but live admission failed: %v", selected.PledgeID, err)
			}
			if selected.PledgeID != actual.PledgeID {
				t.Fatalf("preview=%s live=%s", selected.PledgeID, actual.PledgeID)
			}
			if selected.RemainingUSD != nil && *selected.RemainingUSD != actual.RemainingUSD {
				t.Fatalf("preview allowance=%v live=%v", *selected.RemainingUSD, actual.RemainingUSD)
			}
			if scenario == "uncapped" && selected.RemainingUSD != nil {
				t.Fatal("uncapped is unknown/unbounded, not $0 remaining")
			}
		})
	}
}
