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

// A fair-usage/frequency refusal is account-level: it arrives as a relayed
// error, never as window telemetry, so it carries NO reset instant — the
// reading's own staleness bound is all there is. It must still skip the
// forfait: four lanes measured on 2026-09-02 kept resolving a credential
// the provider refused on every single request.
func TestForfaitWindowClosed_frequencyRefusalSkips(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	fp := withTenantForfait(t, f, "sk-ant-tenant-own")
	meterSays(t, f, fp, usagecap.Reading{
		Window: usagecap.WindowFrequency, Status: usagecap.StatusRejected,
		ObservedAt: time.Now(),
	})
	got := bundleToken(t, f, "run-frequency-refused")
	if contains(got, "sk-ant-tenant-own") {
		t.Fatalf("the frequency-refused forfait was handed over anyway: %q", got)
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
		{
			// usagecap itself never blocks on overage (it is the
			// pay-as-you-go MONEY channel, bounded by budget flags, not
			// quota) — a rejected overage reading is no refusal evidence.
			name: "rejected overage window is not quota evidence",
			reading: usagecap.Reading{
				Window: usagecap.WindowOverage, Status: usagecap.StatusRejected, Utilization: 1,
				ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now(),
			},
		},
		{
			// FamilyOf's contract: an unknown window is not silently
			// folded into a rule that was never meant to govern it.
			name: "rejected unknown window is not quota evidence",
			reading: usagecap.Reading{
				Window: usagecap.Window("five_minute_burst"), Status: usagecap.StatusRejected, Utilization: 1,
				ResetsAt: time.Now().Add(time.Hour), ObservedAt: time.Now(),
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

type failingUsageStore struct{ usagecap.Store }

func (failingUsageStore) Record(context.Context, string, usagecap.Reading) error { return nil }
func (failingUsageStore) Latest(context.Context, string) ([]usagecap.Reading, error) {
	return nil, context.DeadlineExceeded
}

// The last-tier rule is DELIBERATELY the opposite of the tenant tiers:
// the platform forfait is handed over even when the provider refused it.
// The platform tier is the last DB-backed tier and the runner's env
// backstop is invisible from the publisher — skipping here could only
// trade a self-healing park (one refused call, durable usage-window
// retry) for a possibly-stuck run with no credential at all.
func TestForfaitWindowClosed_platformTierHandsOverRegardless(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
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
	st := usagecap.NewMemStore()
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, recs[0].Fingerprint)
	if err := st.Record(context.Background(), key, usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	f.pub.usageCaps = st
	if got := bundleToken(t, f, "run-platform-window"); !contains(got, "sk-ant-platform") {
		t.Fatalf("the platform forfait must be handed over even refused (park beats stuck), got %q", got)
	}
}

// A skipped forfait is only an improvement when another tier can serve.
// With NO pool and NO platform credential, the refused tenant forfait is
// RESTORED at the end of the resolution: the run then parks on the
// provider refusal with a durable usage-window retry, instead of failing
// on a no-credential auth error nothing retries.
func TestForfaitWindowClosed_restoredWhenNoTierCanServe(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, secrets.OrgOwnerKey(poolTeam), "sk-ant-tenant-own")
	recs, err := oauth.ListByUser(context.Background(), secrets.OrgOwnerKey(poolTeam))
	if err != nil || len(recs) != 1 {
		t.Fatalf("seeded forfait unreadable: %v (%d records)", err, len(recs))
	}
	rs := secrets.NewMemoryRunSecretsStore()
	f := &poolFixture{
		pub: &Publisher{
			// NO credPool, NO platform records: the skip has nowhere to
			// fall through to.
			runSecrets:   rs,
			sealer:       sealer,
			oauthForfait: oauth,
			logger:       iterlog.New(iterlog.LevelError, nil),
		},
		rs: rs, sealer: sealer,
	}
	st := usagecap.NewMemStore()
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope(poolTeam), recs[0].Fingerprint)
	if err := st.Record(context.Background(), key, usagecap.Reading{
		Window: usagecap.WindowFiveHour, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(3 * time.Hour), ObservedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	f.pub.usageCaps = st
	if got := bundleToken(t, f, "run-restore"); !contains(got, "sk-ant-tenant-own") {
		t.Fatalf("the refused forfait must be RESTORED when no tier can serve, got %q", got)
	}
}

// An OPERATOR cap closes the window exactly like a provider refusal: the
// runner's pre-flight parks on Decision.Blocked before any node runs, so
// handing the capped forfait over just replays the park on every retry.
// Measured on 2026-09-04: a weekly forfait at 97% (provider still ALLOWING)
// was re-granted on four consecutive attempts while the next tier sat idle —
// the park writes no refusal, so no signal ever broke the loop.
func TestForfaitWindowClosed_operatorCapFallsThrough(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	fp := withTenantForfait(t, f, "sk-ant-tenant-own")
	meterSays(t, f, fp, usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusWarning, Utilization: 0.97,
		ResetsAt: time.Now().Add(4 * 24 * time.Hour), ObservedAt: time.Now(),
	})

	// Without a policy the walk stays refusal-evidence-only: utilization
	// alone must not bench a credential (the shipped no-cap deployment).
	if got := bundleToken(t, f, "run-no-policy"); !contains(got, "sk-ant-tenant-own") {
		t.Fatalf("no policy: run must keep the tenant's forfait, got %q", got)
	}

	// Under the cap: nothing to skip.
	f.pub.capPolicy = usagecap.StaticPolicy(usagecap.Policy{
		Week: usagecap.WindowPolicy{MaxPercent: 99, Mode: usagecap.ModeHard}})
	if got := bundleToken(t, f, "run-under-cap"); !contains(got, "sk-ant-tenant-own") {
		t.Fatalf("under cap: run must keep the tenant's forfait, got %q", got)
	}

	// Hard cap reached: the pre-flight would park this run — fall through.
	f.pub.capPolicy = usagecap.StaticPolicy(usagecap.Policy{
		Week: usagecap.WindowPolicy{MaxPercent: 95, Mode: usagecap.ModeHard}})
	got := bundleToken(t, f, "run-hard-cap")
	if got == "" {
		t.Fatal("the run got NO credential: the cap skip must fall through, not empty the bundle")
	}
	if contains(got, "sk-ant-tenant-own") {
		t.Fatalf("the hard-capped forfait was handed over anyway: %q", got)
	}
	if !contains(got, "sk-ant-donated") {
		t.Fatalf("expected the donor's credential from the pool, got %q", got)
	}
}

// SOFT counts too, and this is the shipped default posture: five-hour caps
// default to soft, and soft means "never interrupts work in flight" — it
// still lets no NEW run start (docs/usage-caps.md). The pre-flight parks on
// Decision.Blocked whatever the mode, so a walk that skipped only on hard
// would reproduce the measured starvation loop on the DEFAULT deployment,
// bounded by the five-hour reset instead of four days.
func TestForfaitWindowClosed_operatorSoftCapAlsoFallsThrough(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	fp := withTenantForfait(t, f, "sk-ant-tenant-own")
	f.pub.capPolicy = usagecap.StaticPolicy(usagecap.Policy{
		FiveHour: usagecap.WindowPolicy{MaxPercent: 85, Mode: usagecap.ModeSoft}})
	meterSays(t, f, fp, usagecap.Reading{
		Window: usagecap.WindowFiveHour, Status: usagecap.StatusWarning, Utilization: 0.9,
		ResetsAt: time.Now().Add(2 * time.Hour), ObservedAt: time.Now(),
	})
	got := bundleToken(t, f, "run-soft-cap")
	if contains(got, "sk-ant-tenant-own") {
		t.Fatalf("the soft-capped forfait was handed over: the pre-flight parks on Blocked, got %q", got)
	}
	if !contains(got, "sk-ant-donated") {
		t.Fatalf("expected the donor's credential from the pool, got %q", got)
	}
}

// A capped reading with NO reset instant is trusted for its own staleness
// bound — the same synthesis the refusal branch applies. Without it the
// walk re-grants a credential the pre-flight parks on, once an hour, for as
// long as the operator's cap stays reached.
func TestForfaitWindowClosed_operatorCapWithoutResetIsBounded(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	fp := withTenantForfait(t, f, "sk-ant-tenant-own")
	f.pub.capPolicy = usagecap.StaticPolicy(usagecap.Policy{
		Week: usagecap.WindowPolicy{MaxPercent: 95, Mode: usagecap.ModeHard}})
	meterSays(t, f, fp, usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusWarning, Utilization: 0.97,
		ObservedAt: time.Now(),
	})
	if got := bundleToken(t, f, "run-capped-no-reset"); contains(got, "sk-ant-tenant-own") {
		t.Fatalf("a capped reading with no reset must still skip (bounded by staleness), got %q", got)
	}

	// Past the staleness bound the reading is ignored by Fresh, and the
	// credential comes back on its own — the skip is self-healing.
	meterSays(t, f, fp, usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusWarning, Utilization: 0.97,
		ObservedAt: time.Now().Add(-2 * usagecap.DefaultMaxAge),
	})
	if got := bundleToken(t, f, "run-capped-stale"); !contains(got, "sk-ant-tenant-own") {
		t.Fatalf("a STALE capped reading must not bench the forfait, got %q", got)
	}
}
