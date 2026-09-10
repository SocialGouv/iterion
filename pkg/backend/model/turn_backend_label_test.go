package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// #1053 — delegateHooksFor receives the node's RESOLVED backend and used to
// throw it away, stamping delegate.BackendClaudeCode on every captured turn.
// pi fires the same OnTurnFinished hook (pi_rpc.go), so every pi turn was
// persisted under another backend's name — a lie in a durable field, and the
// one fork.go decides its claw-conversation branch on.
func TestDelegateHooks_TurnCarriesTheResolvedBackend(t *testing.T) {
	for _, backend := range []string{
		delegate.BackendClaudeCode,
		delegate.BackendPi,
		delegate.BackendClaw,
		"some-future-cli",
	} {
		t.Run(backend, func(t *testing.T) {
			var got LLMTurnCaptureInfo
			var calls int
			e := &ClawExecutor{hooks: EventHooks{
				OnLLMTurnCapture: func(_ string, info LLMTurnCaptureInfo) {
					calls++
					got = info
				},
			}}

			h := e.delegateHooksFor("n1", backend, 0)
			if h.OnTurnFinished == nil {
				t.Fatal("OnTurnFinished was not wired")
			}
			h.OnTurnFinished(delegate.TurnFinishedInfo{SessionID: "sess-1", Text: "hi"})

			if calls != 1 {
				t.Fatalf("capture fired %d times, want 1", calls)
			}
			if got.Backend != backend {
				t.Errorf("turn recorded Backend=%q, want %q — the resolved name, not a constant", got.Backend, backend)
			}
			if got.SessionID != "sess-1" {
				t.Errorf("SessionID = %q, want sess-1", got.SessionID)
			}
		})
	}
}
