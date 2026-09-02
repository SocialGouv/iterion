package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The BYOK walk's evidence predicate, both faces: only a fresh quota-family
// refusal under the KEY'S OWN fingerprint skips it. Everything uncertain —
// no reading, another provider's meter, the pay-as-you-go overage channel —
// keeps the key usable, because a wrong skip silently moves spend onto a
// credential nobody chose.
func TestApiKeyUsable(t *testing.T) {
	st := usagecap.NewMemStore()
	p := &Publisher{usageCaps: st, logger: iterlog.New(iterlog.LevelError, nil)}
	scope := usagecap.TenantScope("team")
	key := secrets.ApiKey{Provider: secrets.ProviderZAI, Name: "primary", Fingerprint: "fp-zai-1"}

	usable := p.apiKeyUsable(context.Background(), scope, "run-x")
	if !usable(key) {
		t.Fatal("no evidence must mean usable")
	}

	// The provider refused this key's account rate (the frequency window
	// #610 records) — the walk must pass it over.
	if err := st.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, "fp-zai-1"),
		usagecap.Reading{Window: usagecap.WindowFrequency, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if usable(key) {
		t.Fatal("a fresh frequency refusal under the key's fingerprint must skip it")
	}

	// Hysteric guards: a rejected OVERAGE reading is money, not quota; and
	// a provider with no metered backend has no evidence to act on.
	st2 := usagecap.NewMemStore()
	p2 := &Publisher{usageCaps: st2, logger: iterlog.New(iterlog.LevelError, nil)}
	if err := st2.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, "fp-zai-1"),
		usagecap.Reading{Window: usagecap.WindowOverage, Status: usagecap.StatusRejected,
			ObservedAt: time.Now()}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if !p2.apiKeyUsable(context.Background(), scope, "run-x")(key) {
		t.Fatal("a rejected overage reading is no quota evidence — the key must stay usable")
	}
	other := secrets.ApiKey{Provider: secrets.ProviderOpenAI, Name: "m", Fingerprint: "fp-zai-1"}
	if !usable(other) {
		t.Fatal("a provider with no metered backend must never be skipped")
	}
}
