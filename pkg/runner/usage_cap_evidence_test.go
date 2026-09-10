package runner

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// Recording is not enforcing. A deployment that configured no cap still
// needs the provider's refusals on the ledger: the credential-tier skips
// (#610 frequency, #624 auth) route around a credential the provider will
// not serve, and their only input is a fresh rejected reading. Gating the
// guard on the cap policy made every one of them inert on exactly the
// deployments that never asked for a ceiling.
func TestUsageGuardFor_RecordsEvidenceWithoutACapPolicy(t *testing.T) {
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-tenant"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-anthropic"},
	})
	caps := usagecap.NewMemStore()
	// No policy source at all — the shape of a deployment with no
	// usage-cap settings store wired.
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), UsageCaps: caps}}

	g := r.usageGuardFor(ctx, &queue.RunMessage{TenantID: "team-7"}, iterlog.Nop())
	if g == nil {
		t.Fatal("want a guard: a deployment with a ledger must still record what it measures")
	}
	d := g.Observe(usagecap.Reading{
		Window:     usagecap.WindowAuth,
		Status:     usagecap.StatusRejected,
		ObservedAt: time.Now().UTC(),
	})
	if d.Blocked {
		t.Fatal("an unconfigured cap must not block anything — recording is not enforcing")
	}
	key := usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-7"), "fp-anthropic")
	got, err := caps.Latest(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Window != usagecap.WindowAuth || got[0].Status != usagecap.StatusRejected {
		t.Fatalf("ledger under %s = %+v, want the auth refusal", key, got)
	}
}

// The one shape that still carries no guard: nothing to enforce AND
// nothing to publish to.
func TestUsageGuardFor_NilWithNeitherPolicyNorLedger(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	if g := r.usageGuardFor(context.Background(), &queue.RunMessage{}, iterlog.Nop()); g != nil {
		t.Fatal("no policy and no ledger: a guard would observe for nobody")
	}
}
