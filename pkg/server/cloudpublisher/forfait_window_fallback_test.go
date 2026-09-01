package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// withTenantForfait gives the fixture's tenant a forfait of its OWN, so
// the pool is out of reach under the ordinary rule ("only a run with no
// credential at all"). Returns the fingerprint the meter keys on.
func withTenantForfait(t *testing.T, f *poolFixture, token string) string {
	t.Helper()
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, f.sealer, secrets.OrgOwnerKey(poolTeam), token)
	f.pub.oauthForfait = oauth
	recs, err := oauth.ListByUser(context.Background(), secrets.OrgOwnerKey(poolTeam))
	if err != nil || len(recs) != 1 {
		t.Fatalf("seeded forfait unreadable: %v (%d records)", err, len(recs))
	}
	return recs[0].Fingerprint
}

// meterSays records one reading for the tenant's forfait, under the SAME
// key the runner meters with.
func meterSays(t *testing.T, f *poolFixture, fp string, r usagecap.Reading) *usagecap.MemStore {
	t.Helper()
	st := usagecap.NewMemStore()
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope(poolTeam), fp)
	if err := st.Record(context.Background(), key, r); err != nil {
		t.Fatalf("record reading: %v", err)
	}
	f.pub.usageCaps = st
	f.pub.usageCapPolicy = usagecap.StaticPolicy(usagecap.Policy{
		FiveHour: usagecap.WindowPolicy{MaxPercent: 90, Mode: usagecap.ModeHard},
		Week:     usagecap.WindowPolicy{MaxPercent: 90, Mode: usagecap.ModeHard},
	})
	return st
}

func bundleToken(t *testing.T, f *poolFixture, runID string) string {
	t.Helper()
	bundle, _ := f.resolve(t, runID, nil)
	blob := bundle.OAuthCredentials["claude_code"]
	if len(blob) == 0 {
		return ""
	}
	return string(blob)
}

// THE fix: a tenant holding a forfait whose provider window is CLOSED must
// not be handed that forfait — one LLM call, a refusal, and a park until
// the window resets (up to a week on the weekly one) while another tier
// could have served immediately. Skipping it makes the tiers a fallback
// CHAIN: the run falls through to the pool.
func TestForfaitWindowClosed_fallsThroughToTheNextTier(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	fp := withTenantForfait(t, f, "sk-ant-tenant-own")

	// Without a meter reading the tenant keeps its own forfait — the
	// baseline this fix must not disturb.
	if got := bundleToken(t, f, "run-baseline"); got == "" || !contains(got, "sk-ant-tenant-own") {
		t.Fatalf("baseline: run must use the tenant's own forfait, got %q", got)
	}

	// The provider refused, and said when the window reopens.
	meterSays(t, f, fp, usagecap.Reading{
		Window: usagecap.WindowFiveHour, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(3 * time.Hour), ObservedAt: time.Now(),
	})
	got := bundleToken(t, f, "run-window-closed")
	if got == "" {
		t.Fatal("the run got NO credential: the skip must fall through to the pool, not empty the bundle")
	}
	if contains(got, "sk-ant-tenant-own") {
		t.Fatalf("the window-closed forfait was handed over anyway: %q", got)
	}
	if !contains(got, "sk-ant-donated") {
		t.Fatalf("expected the donor's credential from the pool, got %q", got)
	}
}

// Conservative by construction: every uncertain answer means "usable". A
// wrong skip spends a donor's quota (or drops to env) for a subscription
// that would have worked.
func TestForfaitWindowClosed_staysConservative(t *testing.T) {
	cases := []struct {
		name    string
		reading usagecap.Reading
		policy  bool
	}{
		{
			name: "reading is stale (its window already reopened)",
			reading: usagecap.Reading{
				Window: usagecap.WindowFiveHour, Status: usagecap.StatusRejected, Utilization: 1,
				ResetsAt: time.Now().Add(-time.Minute), ObservedAt: time.Now().Add(-2 * time.Hour),
			},
			policy: true,
		},
		{
			name: "under the operator's ceiling",
			reading: usagecap.Reading{
				Window: usagecap.WindowFiveHour, Status: usagecap.StatusAllowed, Utilization: 0.5,
				ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now(),
			},
			policy: true,
		},
		{
			// An operator PERCENTAGE cap is a ceiling on this deployment's
			// own spending, not evidence the provider would refuse:
			// skipping on it would push work onto a donor to keep a tenant
			// under its own budget.
			name: "over the operator's ceiling but the provider never refused, and no reset instant",
			reading: usagecap.Reading{
				Window: usagecap.WindowFiveHour, Status: usagecap.StatusAllowed, Utilization: 0.99,
				ObservedAt: time.Now(),
			},
			policy: true,
		},
		{
			name: "no policy wired at all",
			reading: usagecap.Reading{
				Window: usagecap.WindowFiveHour, Status: usagecap.StatusRejected, Utilization: 1,
				ResetsAt: time.Now().Add(3 * time.Hour), ObservedAt: time.Now(),
			},
			policy: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
			fp := withTenantForfait(t, f, "sk-ant-tenant-own")
			meterSays(t, f, fp, c.reading)
			if !c.policy {
				f.pub.usageCapPolicy = nil
			}
			got := bundleToken(t, f, "run-conservative")
			if !contains(got, "sk-ant-tenant-own") {
				t.Fatalf("the tenant's own forfait must still be used, got %q", got)
			}
		})
	}
}

// A meter failure must not change which credential a run gets: the meter
// is an optimisation, not an authority.
func TestForfaitWindowClosed_meterFailureIsNeutral(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	withTenantForfait(t, f, "sk-ant-tenant-own")
	f.pub.usageCaps = failingUsageStore{}
	f.pub.usageCapPolicy = usagecap.StaticPolicy(usagecap.Policy{
		FiveHour: usagecap.WindowPolicy{MaxPercent: 90, Mode: usagecap.ModeHard},
	})
	if got := bundleToken(t, f, "run-meter-down"); !contains(got, "sk-ant-tenant-own") {
		t.Fatalf("a meter failure must leave the tenant's forfait in place, got %q", got)
	}
}

type failingUsageStore struct{}

func (failingUsageStore) Record(context.Context, string, usagecap.Reading) error { return nil }
func (failingUsageStore) Latest(context.Context, string) ([]usagecap.Reading, error) {
	return nil, context.DeadlineExceeded
}

// The platform's own forfait meters under ScopePlatform, not the tenant's
// scope: a closed platform window must skip too (that is the deployment-
// wide subscription every tenant without its own credential falls on).
func TestForfaitWindowClosed_platformScope(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, f.sealer, secrets.PlatformOwnerKey, "sk-ant-platform")
	f.pub.oauthForfait = oauth
	recs, _ := oauth.ListByUser(context.Background(), secrets.PlatformOwnerKey)
	if len(recs) != 1 {
		t.Fatalf("seeded platform forfait unreadable (%d records)", len(recs))
	}

	st := usagecap.NewMemStore()
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, recs[0].Fingerprint)
	if err := st.Record(context.Background(), key, usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	f.pub.usageCaps = st
	f.pub.usageCapPolicy = usagecap.StaticPolicy(usagecap.Policy{
		Week: usagecap.WindowPolicy{MaxPercent: 90, Mode: usagecap.ModeHard},
	})

	// The platform owner key is resolved with the platform scope, so this
	// only skips when the scope is right — a tenant-scoped lookup would
	// find nothing and hand the exhausted platform forfait over.
	got := bundleToken(t, f, "run-platform-window")
	if contains(got, "sk-ant-platform") {
		t.Fatalf("the exhausted platform forfait was handed over: %q", got)
	}
	if !contains(got, "sk-ant-donated") {
		t.Fatalf("expected the pool to serve instead, got %q", got)
	}
}
