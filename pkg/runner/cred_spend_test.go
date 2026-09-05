package runner

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/credusage"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #641 — the question the org bucket cannot answer. One run, two
// credentials: a team forfait on claude_code and a platform openai key on
// codex. Charging RunTotals() to either would attribute one credential's
// calls to the other, which is exactly the misreading a per-credential
// counter exists to remove.
func TestRecordCredentialSpend_SplitsOneRunAcrossItsCredentials(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), CredUsage: counter}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-platform"},
		OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/tmp/oauth"},
		PlatformSourced:      map[string]bool{string(secrets.ProviderOpenAI): true},
		Fingerprints: map[string]string{
			delegate.BackendClaudeCode:        "fp-forfait",
			string(secrets.ProviderOpenAI):    "fp-openai",
			string(secrets.OAuthKindCodex):    "",
			string(secrets.ProviderZAI):       "",
			string(secrets.ProviderAnthropic): "",
		},
	})
	usage := newMetricsEmitter(nil, nil)
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "implement",
		Data: map[string]any{"model": "anthropic/claude-opus-5"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "implement",
		Data: map[string]any{"backend": delegate.BackendClaudeCode, "tokens": float64(9000), "cost_usd": 4.5}})
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "review",
		Data: map[string]any{"model": "openai/gpt-5.6-sol"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "review",
		Data: map[string]any{"backend": delegate.BackendCodex, "tokens": float64(300), "cost_usd": 2.0}})

	now := time.Now().UTC()
	r.recordCredentialSpend(ctx, &queue.RunMessage{RunID: "run-1", TenantID: "team-a"}, usage, now)

	rows, err := counter.List(ctx, now, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("recorded %d credential rows, want 2 (the forfait and the platform key)", len(rows))
	}
	byFP := map[string]credusage.MonthlyUsage{}
	for _, row := range rows {
		byFP[row.Fingerprint] = row
	}
	forfait, ok := byFP["fp-forfait"]
	if !ok {
		t.Fatalf("no row for the forfait: %+v", rows)
	}
	if forfait.CostUSD != 4.5 || forfait.InputTokens != 9000 {
		t.Fatalf("forfait row = %+v, want $4.50 / 9000 tokens — not the run total", forfait)
	}
	// A subscription bills nothing per call: its figure is what the calls
	// WOULD have cost metered. Typed in the record, not only in the docs.
	if forfait.Nature != credusage.NatureEstimate {
		t.Fatalf("forfait nature = %q, want estimate", forfait.Nature)
	}
	if forfait.Tier != credusage.TierTeam {
		t.Fatalf("forfait tier = %q, want team", forfait.Tier)
	}
	key, ok := byFP["fp-openai"]
	if !ok {
		t.Fatalf("no row for the platform key: %+v", rows)
	}
	if key.CostUSD != 2.0 {
		t.Fatalf("platform key row = %+v, want $2.00", key)
	}
	if key.Nature != credusage.NatureMetered {
		t.Fatalf("api-key nature = %q, want metered — every token is on an invoice", key.Nature)
	}
	if key.Tier != credusage.TierPlatform {
		t.Fatalf("platform key tier = %q, want platform", key.Tier)
	}
	if len(key.Backends) != 1 || key.Backends[0] != delegate.BackendCodex {
		t.Fatalf("platform key backends = %v, want [codex]", key.Backends)
	}
}

// A lent credential's spend belongs to the DONOR, and the tier is what says
// so on the row.
func TestRecordCredentialSpend_MarksALentCredential(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), CredUsage: counter}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-donor"},
		PoolSourced:  map[string]bool{string(secrets.ProviderAnthropic): true},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-donor"},
	})
	usage := newMetricsEmitter(nil, nil)
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "n",
		Data: map[string]any{"model": "anthropic/claude-opus-5"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "n",
		Data: map[string]any{"backend": delegate.BackendClaudeCode, "tokens": float64(10), "cost_usd": 0.5}})

	now := time.Now().UTC()
	r.recordCredentialSpend(ctx, &queue.RunMessage{RunID: "run-2", TenantID: "team-b"}, usage, now)

	rows, _ := counter.List(ctx, now, "team-b")
	if len(rows) != 1 || rows[0].Tier != credusage.TierPool {
		t.Fatalf("rows = %+v, want one pool-tier row", rows)
	}
	if rows[0].Nature != credusage.NatureMetered {
		t.Fatalf("nature = %q, want metered — a lent key is real money on the donor's invoice", rows[0].Nature)
	}
}

// What cannot be attributed is not recorded: a route whose provider the run
// holds no credential for would otherwise land on whichever fingerprint a
// default precedence picked.
func TestRecordCredentialSpend_SkipsAnUnattributableRoute(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), CredUsage: counter}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-a"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-a"},
	})
	usage := newMetricsEmitter(nil, nil)
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "n",
		Data: map[string]any{"model": "google/gemini-3"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "n",
		Data: map[string]any{"backend": "pi", "tokens": float64(10), "cost_usd": 0.5}})

	now := time.Now().UTC()
	r.recordCredentialSpend(ctx, &queue.RunMessage{RunID: "run-3", TenantID: "team-c"}, usage, now)
	if rows, _ := counter.List(ctx, now, "team-c"); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none: a route iterion cannot attribute must charge nobody", rows)
	}
}

// The nature line is credpool's, applied to a slot shape. Two packages
// deciding "is this real money" apart would eventually disagree, and the
// disagreement would be invisible: both answers are plausible strings.
func TestCredentialNature_AgreesWithCredpoolMetered(t *testing.T) {
	cases := []struct {
		slot   string
		source credpool.CredentialSource
	}{
		{delegate.BackendClaudeCode, credpool.SourceOAuth},
		{string(secrets.OAuthKindCodex), credpool.SourceOAuth},
		{string(secrets.ProviderAnthropic), credpool.SourceAPIKey},
		{string(secrets.ProviderOpenAI), credpool.SourceAPIKey},
		{string(secrets.ProviderZAI), credpool.SourceAPIKey},
	}
	for _, c := range cases {
		metered := credentialNature(c.slot) == credusage.NatureMetered
		if metered != c.source.Metered() {
			t.Errorf("slot %q: credusage says metered=%v, credpool says %v", c.slot, metered, c.source.Metered())
		}
	}
}

// Metering never fails a run: no counter, no credentials, nothing spent.
func TestRecordCredentialSpend_IsInertWithoutACounterOrCredentials(t *testing.T) {
	r := &Runner{cfg: Config{Logger: iterlog.Nop()}}
	usage := newMetricsEmitter(nil, nil)
	r.recordCredentialSpend(context.Background(), &queue.RunMessage{RunID: "x"}, usage, time.Now())
	r.recordCredentialSpend(context.Background(), &queue.RunMessage{RunID: "x"}, nil, time.Now())

	counter := credusage.NewMemoryCounter()
	r2 := &Runner{cfg: Config{Logger: iterlog.Nop(), CredUsage: counter}}
	r2.recordCredentialSpend(context.Background(), &queue.RunMessage{RunID: "x"}, usage, time.Now())
	if rows, _ := counter.List(context.Background(), time.Now(), ""); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}
