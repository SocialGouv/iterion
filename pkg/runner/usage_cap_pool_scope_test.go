package runner

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// A pool-lent credential is the DONOR's, not the borrower's. Metering it
// under the borrower's tenant scope opens one meter per borrower of the
// same subscription: what borrower A measured — a refusal, a window at
// 95% — never reaches borrower B, who spends the donor's quota into the
// same wall, and never reaches the donor's own tier skip either. The
// platform tier already answers this by NOT being the tenant's own; the
// pool tier is the same shape and was honoured on only one of the two
// sites that read it (#629 pt 3).
func TestUsageCapCredKeys_ALentCredentialIsNotTheBorrowersOwn(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-borrower"}

	lentKey := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-donor"},
		PoolSourced:  map[string]bool{string(secrets.ProviderAnthropic): true},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-donor"},
	})
	want := usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-donor")
	if got := usageCapKey(lentKey, msg); got != want {
		t.Errorf("lent api key metered under %q, want the shared meter %q", got, want)
	}

	lentOAuth := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/tmp/oauth"},
		PoolSourced:          map[string]bool{delegate.BackendClaudeCode: true},
		Fingerprints:         map[string]string{delegate.BackendClaudeCode: "fp-donor-oauth"},
	})
	want = usagecap.Key(delegate.BackendClaudeCode, usagecap.ScopePlatform, "fp-donor-oauth")
	if got := usageCapKey(lentOAuth, msg); got != want {
		t.Errorf("lent forfait metered under %q, want the shared meter %q", got, want)
	}

	// The tenant's own credential is unaffected.
	own := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-own"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-own"},
	})
	want = usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team-borrower"), "fp-own")
	if got := usageCapKey(own, msg); got != want {
		t.Errorf("the tenant's own key metered under %q, want %q", got, want)
	}
}

// The labels pi stamps on its own auth-refusal evidence have to be ones
// THIS meter understands, or the reading lands on whichever credential the
// default precedence picks — which would bench a healthy key. The literals
// are delegate.piUsageSource's, pinned on both sides (see
// TestPiUsageSource): a label renamed on one side reddens the other.
func TestUsageCapCredKeys_UnderstandsThePiSourceLabels(t *testing.T) {
	msg := &queue.RunMessage{TenantID: "team-7"}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{
			secrets.ProviderZAI:       "zai-token",
			secrets.ProviderAnthropic: "sk-ant",
		},
		Fingerprints: map[string]string{
			string(secrets.ProviderZAI):       "fp-zai",
			string(secrets.ProviderAnthropic): "fp-ant",
		},
	})
	keys := usageCapCredKeys(ctx, msg)
	scope := usagecap.TenantScope("team-7")
	for _, c := range []struct{ source, wantFP string }{
		{"anthropic-direct", "fp-ant"},
		{"facade:pi-zai", "fp-zai"},
	} {
		if got := keys.forSource(c.source); got != usagecap.Key(delegate.BackendClaudeCode, scope, c.wantFP) {
			t.Errorf("forSource(%q) = %q, want fingerprint %q", c.source, got, c.wantFP)
		}
	}
}

// The predicate itself, at the class: three tiers can fill a slot and only
// one of them is the tenant's.
func TestCredentials_IsTenantOwned(t *testing.T) {
	c := secrets.Credentials{
		PlatformSourced: map[string]bool{"openai": true},
		PoolSourced:     map[string]bool{"anthropic": true},
	}
	if c.IsTenantOwned("openai") {
		t.Error("a platform-tier slot is not the tenant's own")
	}
	if c.IsTenantOwned("anthropic") {
		t.Error("a pool-lent slot is not the tenant's own")
	}
	if !c.IsTenantOwned("zai") {
		t.Error("an unmarked slot is the tenant's own")
	}
}
