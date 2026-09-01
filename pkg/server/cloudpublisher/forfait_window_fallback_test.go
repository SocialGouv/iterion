package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credpool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
// key the runner meters with. No policy is involved: the skip's only
// evidence is the provider's own refusal.
func meterSays(t *testing.T, f *poolFixture, fp string, r usagecap.Reading) *usagecap.MemStore {
	t.Helper()
	st := usagecap.NewMemStore()
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope(poolTeam), fp)
	if err := st.Record(context.Background(), key, r); err != nil {
		t.Fatalf("record reading: %v", err)
	}
	f.pub.usageCaps = st
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
// CHAIN: the run falls through to the pool. No operator policy is needed:
// a provider refusal is objective evidence on its own, so the skip also
// protects the shipped default deployment where no cap was ever set.
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

// Conservative by construction: every uncertain answer means "usable", and
// only the provider's own refusal is evidence. A wrong skip spends a
// donor's quota (or drops to env) for a subscription that would have
// worked. The high-utilization cases carry a RESET INSTANT on purpose —
// the production reading always does (the CLI's rate_limit_event names
// resets_at on every window), so a guard that only defends the
// no-reset-instant shape defends almost nothing.
func TestForfaitWindowClosed_staysConservative(t *testing.T) {
	cases := []struct {
		name    string
		reading usagecap.Reading
	}{
		{
			name: "refusal is stale (its window already reopened)",
			reading: usagecap.Reading{
				Window: usagecap.WindowFiveHour, Status: usagecap.StatusRejected, Utilization: 1,
				ResetsAt: time.Now().Add(-time.Minute), ObservedAt: time.Now().Add(-2 * time.Hour),
			},
		},
		{
			name: "plainly allowed",
			reading: usagecap.Reading{
				Window: usagecap.WindowFiveHour, Status: usagecap.StatusAllowed, Utilization: 0.5,
				ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now(),
			},
		},
		{
			// The provider is still serving this credential; a deployment
			// whose OPERATOR set a lower ceiling must cap its own runs, not
			// push work onto a donor to keep a tenant under its own budget.
			name: "nearly full but the provider never refused — with a reset instant, the production shape",
			reading: usagecap.Reading{
				Window: usagecap.WindowFiveHour, Status: usagecap.StatusAllowed, Utilization: 0.99,
				ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now(),
			},
		},
		{
			name: "provider warning is not a refusal",
			reading: usagecap.Reading{
				Window: usagecap.WindowSevenDay, Status: usagecap.StatusWarning, Utilization: 0.97,
				ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
			fp := withTenantForfait(t, f, "sk-ant-tenant-own")
			meterSays(t, f, fp, c.reading)
			if got := bundleToken(t, f, "run-conservative"); !contains(got, "sk-ant-tenant-own") {
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
	if got := bundleToken(t, f, "run-meter-down"); !contains(got, "sk-ant-tenant-own") {
		t.Fatalf("a meter failure must leave the tenant's forfait in place, got %q", got)
	}
}

type failingUsageStore struct{}

func (failingUsageStore) Record(context.Context, string, usagecap.Reading) error { return nil }
func (failingUsageStore) Latest(context.Context, string) ([]usagecap.Reading, error) {
	return nil, context.DeadlineExceeded
}

// The deployment's own forfait (tier 5, fillFromPlatform) obeys the same
// rule. It meters under ScopePlatform and the reserved owner key, and it
// is only reached when no tenant tier and no pool served — so the fixture
// here deliberately has NO pool: a vacuous path would leave the platform
// skip untested (measured: the previous version of this test never
// reached fillFromPlatform at all, and passed with the skip deleted).
func TestForfaitWindowClosed_platformScope(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	newPlatformPub := func(t *testing.T) (*poolFixture, string) {
		t.Helper()
		oauth := secrets.NewMemoryOAuthStore()
		seedOAuth(t, oauth, sealer, secrets.PlatformOwnerKey, "sk-ant-platform")
		recs, err := oauth.ListByUser(context.Background(), secrets.PlatformOwnerKey)
		if err != nil || len(recs) != 1 {
			t.Fatalf("seeded platform forfait unreadable: %v (%d records)", err, len(recs))
		}
		rs := secrets.NewMemoryRunSecretsStore()
		f := &poolFixture{
			pub: &Publisher{
				runSecrets:   rs,
				sealer:       sealer,
				oauthForfait: oauth,
				logger:       iterlog.New(iterlog.LevelError, nil),
			},
			rs: rs, sealer: sealer,
		}
		return f, recs[0].Fingerprint
	}

	// Baseline: the platform forfait serves a credential-less run.
	f, _ := newPlatformPub(t)
	if got := bundleToken(t, f, "run-platform-baseline"); !contains(got, "sk-ant-platform") {
		t.Fatalf("baseline: the platform forfait must serve, got %q", got)
	}

	// Refused: the same run must NOT get the platform forfait — the slot
	// stays empty for the runner's env backstop.
	f, fp := newPlatformPub(t)
	st := usagecap.NewMemStore()
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, fp)
	if err := st.Record(context.Background(), key, usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	f.pub.usageCaps = st
	if got := bundleToken(t, f, "run-platform-window"); contains(got, "sk-ant-platform") {
		t.Fatalf("the refused platform forfait was handed over: %q", got)
	}

	// The scope must be the platform's: the same refusal recorded under a
	// tenant scope describes a DIFFERENT meter and must not skip.
	f, fp = newPlatformPub(t)
	st = usagecap.NewMemStore()
	wrongKey := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope(poolTeam), fp)
	if err := st.Record(context.Background(), wrongKey, usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	f.pub.usageCaps = st
	if got := bundleToken(t, f, "run-platform-wrong-scope"); !contains(got, "sk-ant-platform") {
		t.Fatalf("a tenant-scoped refusal skipped the platform forfait, got %q", got)
	}
}
