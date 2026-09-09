package runner

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/credusage"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #992 — a CLI delegate reports ONE aggregated token count. Booking it under
// `input_tokens` keeps a sum correct and makes the named field a lie: every
// per-token ratio, cache-hit reading and input/output share computed from the
// public endpoint is wrong, with nothing on the row saying so.
//
// The aggregate travels in its own counter, and the two directional fields
// stay at zero — "not observed", which is the truth.
func TestDelegateAggregate_IsNotClaimedAsInput(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), CredUsage: counter}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{delegate.BackendClaudeCode: "/tmp/oauth"},
		Fingerprints:         map[string]string{delegate.BackendClaudeCode: "fp-forfait"},
	})

	usage := newMetricsEmitter(nil, nil)
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "implement",
		Data: map[string]any{"model": "anthropic/claude-opus-5"}})
	usage.observe(store.Event{Type: store.EventDelegateFinished, NodeID: "implement",
		Data: map[string]any{"backend": delegate.BackendClaudeCode, "tokens": float64(9000), "cost_usd": 4.5}})

	now := time.Now().UTC()
	r.recordCredentialSpend(ctx, &queue.RunMessage{RunID: "run-1", TenantID: "team-a"}, usage, now)

	rows, err := counter.List(ctx, now, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("recorded %d credential rows, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.AggregateTokens != 9000 {
		t.Errorf("AggregateTokens = %d, want 9000 — the delegate's only observation", row.AggregateTokens)
	}
	if row.InputTokens != 0 {
		t.Errorf("InputTokens = %d, want 0 — the delegate never reported a split, so claiming one is a lie", row.InputTokens)
	}
	if row.OutputTokens != 0 {
		t.Errorf("OutputTokens = %d, want 0", row.OutputTokens)
	}
	// The property the old convention protected must survive: a consumer
	// summing every token field still gets the true total.
	if total := row.InputTokens + row.OutputTokens + row.AggregateTokens; total != 9000 {
		t.Errorf("summed tokens = %d, want 9000 — the aggregate must stay countable", total)
	}
}

// The in-process claw path observes a real split, so it keeps reporting one:
// the fix must not flatten a genuine measurement into "unknown".
func TestClawStepSplit_StaysADirectionalSplit(t *testing.T) {
	counter := credusage.NewMemoryCounter()
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), CredUsage: counter}}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-team"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-anthropic"},
	})

	usage := newMetricsEmitter(nil, nil)
	usage.observe(store.Event{Type: store.EventLLMRequest, NodeID: "n",
		Data: map[string]any{"model": "anthropic/claude-opus-5"}})
	usage.observe(store.Event{Type: store.EventLLMStepFinished, NodeID: "n",
		Data: map[string]any{"input_tokens": float64(700), "output_tokens": float64(300)}})

	now := time.Now().UTC()
	r.recordCredentialSpend(ctx, &queue.RunMessage{RunID: "run-2", TenantID: "team-b"}, usage, now)

	rows, err := counter.List(ctx, now, "team-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("recorded %d credential rows, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.InputTokens != 700 || row.OutputTokens != 300 {
		t.Errorf("row = in %d / out %d, want 700/300 — a measured split", row.InputTokens, row.OutputTokens)
	}
	if row.AggregateTokens != 0 {
		t.Errorf("AggregateTokens = %d, want 0 — this path observed both directions", row.AggregateTokens)
	}
}
