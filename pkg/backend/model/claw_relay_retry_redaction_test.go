package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A relayed `llm_retry` carries the container's own account of why a provider
// call was retried — a 401 body, a gateway error — and the launcher re-fires
// its own OnLLMRetry with it, so the text lands on the operator's "LLM retry"
// line. Since #2458 no store-hook sink runs a pass of its own for the log:
// the run logger's redactor (wired by BuildExecutor from the run's guard,
// mirrored here) scrubs every record it keeps.
//
// Driven through the real chain — the crossing's event handler, the relay
// decoder, the production store hooks — because the leak is in what the SINK
// does with text the relay hands it, not in either half alone.
func TestClawRelay_RedactsARelayedRetryForTheOperatorLog(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-RELAY0123456789abcdefghijklmnopqrs"
	g := secretguard.New([]secretguard.Secret{{Name: "OPENAI_API_KEY", Value: secret}}, secretguard.DefaultConfig())
	placeholder := g.Redact(secret)
	if placeholder == secret || placeholder == "" {
		t.Fatalf("scenario broken: the guard does not redact the witness value (%q)", placeholder)
	}

	var runLog strings.Builder
	logger := iterlog.New(iterlog.LevelInfo, &runLog).WithRedactor(g.Redact)
	hooks := NewStoreEventHooks(context.Background(), &captureEmitter{}, "run-relay-retry",
		logger, g, nil)
	b := NewClawBackend(NewRegistry(), hooks, RetryPolicy{},
		WithClawLogger(logger))

	b.multiplexerHandler(context.Background(), delegate.Task{NodeID: "plan", Iteration: 1}, sessionSeed{}).
		OnEvent(string(store.EventLLMRetry), map[string]any{
			"attempt":  1,
			"delay_ms": 250,
			"error":    "openai: 401 unauthorized (Authorization: Bearer " + secret + ")",
		})

	if !strings.Contains(runLog.String(), "LLM retry") {
		t.Fatalf("scenario broken: no LLM-retry line in %q", runLog.String())
	}
	if strings.Contains(runLog.String(), secret) {
		t.Errorf("the relayed retry reached the operator log with the run's secret in clear: %q", runLog.String())
	}
	if !strings.Contains(runLog.String(), placeholder) {
		t.Errorf("the relayed retry's reason is missing, or reported outside the run's redaction: %q", runLog.String())
	}
}
