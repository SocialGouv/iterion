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

// #684 — a resolution that passes a credential over reports WHEN it reopens,
// so the run stamps it and its usage-window retry can arm on the earlier of
// that and the failed credential's own reset. The forfait tier: the tenant's
// forfait is refused on the five-hour window (reopens in three hours), the
// run falls through to the pool donor, and the resolution carries 16:40Z.
func TestResolve_ReportsWhenTheSkippedForfaitReopens(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	fp := withTenantForfait(t, f, "sk-ant-tenant-own")
	reopens := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Second)
	meterSays(t, f, fp, usagecap.Reading{
		Window: usagecap.WindowFiveHour, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: reopens, ObservedAt: time.Now(),
	})

	bundle, creds := f.resolve(t, "run-684", nil)
	if got := string(bundle.OAuthCredentials["claude_code"]); !contains(got, "sk-ant-donated") {
		t.Fatalf("expected the donor's credential after the skip, got %q", got)
	}
	if !creds.skippedReopensAt.Equal(reopens) {
		t.Fatalf("skippedReopensAt = %v, want the refused forfait's reopening %v", creds.skippedReopensAt, reopens)
	}
	stamp := creds.stamp()
	if stamp.SkippedReopensAt == nil || !stamp.SkippedReopensAt.Equal(reopens) {
		t.Fatalf("stamp.SkippedReopensAt = %v, want %v", stamp.SkippedReopensAt, reopens)
	}

	// Nothing skipped → nothing stamped: a stale instant would arm a retry
	// on a credential the run was never refused.
	f2 := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
	withTenantForfait(t, f2, "sk-ant-tenant-own")
	if _, creds := f2.resolve(t, "run-684-clean", nil); !creds.skippedReopensAt.IsZero() || creds.stamp().SkippedReopensAt != nil {
		t.Fatalf("a resolution that skipped nothing reported %v", creds.skippedReopensAt)
	}
}

// The BYOK walk reports through the same tracker, and the EARLIEST
// reopening wins when several credentials are passed over.
func TestApiKeyUsable_ReportsTheEarliestReopening(t *testing.T) {
	st := usagecap.NewMemStore()
	p := &Publisher{usageCaps: st, logger: iterlog.New(iterlog.LevelError, nil)}
	scope := usagecap.TenantScope("team")
	now := time.Now().UTC()
	late := now.Add(4 * 24 * time.Hour)
	soon := now.Add(3 * time.Hour)
	for fp, at := range map[string]time.Time{"fp-late": late, "fp-soon": soon} {
		if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, scope, fp),
			usagecap.Reading{Window: usagecap.WindowFiveHour, Status: usagecap.StatusRejected, Utilization: 1,
				ResetsAt: at, ObservedAt: now}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	skips := &skipTracker{}
	usable := p.apiKeyUsable(context.Background(), scope, "run-x", skips)
	if usable(secrets.ApiKey{Provider: secrets.ProviderAnthropic, Name: "late", Fingerprint: "fp-late"}) {
		t.Fatal("a refused key was handed over")
	}
	if usable(secrets.ApiKey{Provider: secrets.ProviderAnthropic, Name: "soon", Fingerprint: "fp-soon"}) {
		t.Fatal("a refused key was handed over")
	}
	if !usable(secrets.ApiKey{Provider: secrets.ProviderAnthropic, Name: "fresh", Fingerprint: "fp-fresh"}) {
		t.Fatal("an unrefused key was skipped")
	}
	if !skips.earliest.Equal(soon) {
		t.Fatalf("earliest reopening = %v, want %v (the sooner of the two refused keys)", skips.earliest, soon)
	}
}
