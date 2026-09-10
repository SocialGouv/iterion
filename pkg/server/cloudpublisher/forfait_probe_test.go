package cloudpublisher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// #690 point 2 — a stale-but-suggestive reading is re-measured at the
// provider before the walk decides. The ledger says the tenant's forfait
// was refused on the weekly window six hours ago (reset three days out):
// past the trust window that is a suggestion, so the walk asks the
// provider, records what it says under the same key the runner meters,
// and decides on THAT — skipping when the wall is real, granting when the
// window reset early, and trusting the credential when the provider cannot
// be asked.
func TestForfaitWindowClosed_RefreshesAStaleReadingFromTheProvider(t *testing.T) {
	now := time.Now().UTC()
	stale := usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: now.Add(72 * time.Hour), ObservedAt: now.Add(-6 * time.Hour),
	}
	setup := func(t *testing.T) (*poolFixture, string, *usagecap.MemStore) {
		t.Helper()
		f := newPoolFixture(t, credpool.Limits{MaxUSDPerDay: 5})
		fp := withTenantForfait(t, f, "sk-ant-tenant-own")
		st := meterSays(t, f, fp, stale)
		f.pub.trust = usagecap.Trust{MaxAge: time.Hour, Window: 2 * time.Hour}
		return f, fp, st
	}
	key := func(fp string) string {
		return usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope(poolTeam), fp)
	}

	t.Run("the wall is real: refreshed reading skips the forfait", func(t *testing.T) {
		f, fp, st := setup(t)
		probed := 0
		f.pub.usageProbe = func(ctx context.Context, payload []byte) ([]usagecap.Reading, error) {
			probed++
			if !contains(string(payload), "sk-ant-tenant-own") {
				t.Errorf("probe got a payload that is not the forfait's own: %q", payload)
			}
			return []usagecap.Reading{{Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
				ResetsAt: now.Add(72 * time.Hour), ObservedAt: now, Source: "anthropic-oauth"}}, nil
		}
		got := bundleToken(t, f, "run-wall")
		if probed != 1 {
			t.Fatalf("probe called %d time(s), want exactly once", probed)
		}
		if contains(got, "sk-ant-tenant-own") || !contains(got, "sk-ant-donated") {
			t.Fatalf("the provider still refuses the forfait, yet the walk handed it over: %q", got)
		}
		latest, _ := st.Latest(context.Background(), key(fp))
		if len(latest) != 1 || latest[0].ObservedAt.Before(now) {
			t.Fatalf("the refreshed reading was not recorded under the credential's key: %+v", latest)
		}
	})

	t.Run("the window reset early: refreshed reading grants the forfait", func(t *testing.T) {
		f, fp, st := setup(t)
		f.pub.usageProbe = func(context.Context, []byte) ([]usagecap.Reading, error) {
			return []usagecap.Reading{{Window: usagecap.WindowSevenDay, Status: usagecap.StatusAllowed, Utilization: 0.02,
				ResetsAt: now.Add(72 * time.Hour), ObservedAt: now, Source: "anthropic-oauth"}}, nil
		}
		if got := bundleToken(t, f, "run-reset"); !contains(got, "sk-ant-tenant-own") {
			t.Fatalf("the provider reports 2%%, yet the walk skipped the forfait: %q", got)
		}
		latest, _ := st.Latest(context.Background(), key(fp))
		if len(latest) != 1 || latest[0].Utilization != 0.02 {
			t.Fatalf("the 2%% reading did not replace the stale refusal: %+v", latest)
		}
	})

	t.Run("the provider cannot be asked: the credential is trusted", func(t *testing.T) {
		f, fp, st := setup(t)
		f.pub.usageProbe = func(context.Context, []byte) ([]usagecap.Reading, error) {
			return nil, errors.New("usage endpoint returned HTTP 403")
		}
		if got := bundleToken(t, f, "run-403"); !contains(got, "sk-ant-tenant-own") {
			t.Fatalf("a failed probe must leave the trust-window verdict (usable), got %q", got)
		}
		latest, _ := st.Latest(context.Background(), key(fp))
		if len(latest) != 1 || !latest[0].ObservedAt.Equal(stale.ObservedAt) {
			t.Fatalf("a failed probe must record nothing: %+v", latest)
		}
	})

	t.Run("a fresh refusal never probes", func(t *testing.T) {
		f, fp, _ := setup(t)
		fresh := stale
		fresh.ObservedAt = now.Add(-10 * time.Minute)
		meterSays(t, f, fp, fresh)
		f.pub.usageProbe = func(context.Context, []byte) ([]usagecap.Reading, error) {
			t.Fatal("probed on a FRESH refusal — the ledger already decides")
			return nil, nil
		}
		if got := bundleToken(t, f, "run-fresh"); contains(got, "sk-ant-tenant-own") {
			t.Fatalf("a fresh refusal must skip the forfait: %q", got)
		}
	})

	t.Run("a stale LOW reading never probes", func(t *testing.T) {
		f, fp, _ := setup(t)
		low := usagecap.Reading{Window: usagecap.WindowSevenDay, Status: usagecap.StatusAllowed, Utilization: 0.1,
			ResetsAt: now.Add(72 * time.Hour), ObservedAt: now.Add(-6 * time.Hour)}
		meterSays(t, f, fp, low)
		f.pub.usageProbe = func(context.Context, []byte) ([]usagecap.Reading, error) {
			t.Fatal("probed on a reading that suggested nothing")
			return nil, nil
		}
		if got := bundleToken(t, f, "run-low"); !contains(got, "sk-ant-tenant-own") {
			t.Fatalf("a low reading must leave the forfait usable: %q", got)
		}
	})
}
