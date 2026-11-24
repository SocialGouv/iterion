package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The class #2458 names: a NON-crossing producer's error — a delegation that
// failed in this process, a dropped session, a fall-through's cause — was
// printed to the operator's run log RAW by every store-hook sink, each of
// which would have needed its own pass. The fix is one redactor on the run's
// logger (iterlog.WithRedactor), wired by runview.BuildExecutor from the
// run's guard; these hooks hand the logger the error unredacted and the
// chokepoint scrubs the record — for every present and future sink at once.
//
// Each subtest drives one hook that prints info.Err raw, through the
// production wiring (NewStoreEventHooks + a logger carrying the guard), and
// asserts on both readers of the line: the log text and the persisted event
// (the latter was already Layer-0's; the two must agree).
func TestStoreHooks_NonCrossingErrorsAreRedactedAtTheRunLogger(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-NONCROSSING0123456789abcdefghijklm"
	g := secretguard.New([]secretguard.Secret{{Name: "OPENAI_API_KEY", Value: secret}}, secretguard.DefaultConfig())
	placeholder := g.Redact(secret)
	if placeholder == secret {
		t.Fatal("scenario broken: the guard does not know the witness value")
	}

	for _, tc := range []struct {
		name string
		fire func(h EventHooks)
		want string // the line fragment that proves the sink was reached
	}{
		{
			name: "delegate_error",
			fire: func(h EventHooks) {
				h.OnDelegateError("n", DelegateInfo{BackendName: "claude_code",
					Error: errors.New("anthropic: 401 (x-api-key: " + secret + ")")})
			},
			want: "Delegation failed",
		},
		{
			name: "delegate_retry",
			fire: func(h EventHooks) {
				h.OnDelegateRetry("n", DelegateInfo{BackendName: "claude_code", Attempt: 2, Delay: time.Second,
					Error: errors.New("rate limited, key " + secret)})
			},
			want: "Delegation retry",
		},
		{
			name: "session_degraded",
			fire: func(h EventHooks) {
				h.OnSessionDegraded("n", SessionDegradedInfo{BackendName: "claude_code", SessionID: "s-1",
					Reason: "resume refused", Err: errors.New("handshake carried " + secret)})
			},
			want: "Session degraded",
		},
		{
			name: "provider_fallback",
			fire: func(h EventHooks) {
				h.OnProviderFallback("n", ProviderFallbackInfo{FromBackend: "claw", ToBackend: "claude_code",
					Reason: "primary refused", Err: errors.New("provider echoed " + secret)})
			},
			want: "Model fallback",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var runLog strings.Builder
			em := &captureEmitter{}
			logger := iterlog.New(iterlog.LevelInfo, &runLog).WithRedactor(g.Redact)
			hooks := NewStoreEventHooks(context.Background(), em, "run-noncrossing", logger, g, nil)

			tc.fire(hooks)

			if !strings.Contains(runLog.String(), tc.want) {
				t.Fatalf("scenario broken: no %q line in %q", tc.want, runLog.String())
			}
			if strings.Contains(runLog.String(), secret) {
				t.Errorf("the operator log carries the run's secret in clear: %q", runLog.String())
			}
			if !strings.Contains(runLog.String(), placeholder) {
				t.Errorf("the line is reported outside the run's redaction: %q", runLog.String())
			}
			evs, _ := json.Marshal(em.events)
			if strings.Contains(string(evs), secret) {
				t.Errorf("an event carries the secret in clear: %s", evs)
			}
		})
	}
}

// The ADR's measurement (docs/adr/149-the-run-logger-redacts-what-it-keeps.md): what the chokepoint costs per kept
// line on a chatty debug run — a realistic guard (a run's provider keys,
// declared secrets and minted grants) over a typical tool-loop line, and
// over a 4 KiB block body, against the same logger with no redactor.
func BenchmarkRunLoggerRedactedLine(b *testing.B) {
	secrets := make([]secretguard.Secret, 0, 32)
	for i := 0; i < 32; i++ {
		secrets = append(secrets, secretguard.Secret{
			Name:  "key_" + strings.Repeat("x", 2) + string(rune('a'+i%26)),
			Value: "sk-bench-" + strings.Repeat(string(rune('A'+i%26)), 24),
		})
	}
	g := secretguard.New(secrets, secretguard.DefaultConfig())
	line := "[%s#%d/claw] %s %s"
	args := func() []any {
		return []any{"campaign", 3, "Bash", `{"command":"git status && git diff --stat"}`}
	}
	body := strings.Repeat("compiler warning: unused import at line 41 of pkg/x/y.go\n", 80)

	for _, bc := range []struct {
		name   string
		redact func(string) string
		block  bool
	}{
		{"line/no-redactor", nil, false},
		{"line/guard", g.Redact, false},
		{"block-4KiB/no-redactor", nil, true},
		{"block-4KiB/guard", g.Redact, true},
	} {
		b.Run(bc.name, func(b *testing.B) {
			l := iterlog.New(iterlog.LevelDebug, io.Discard).WithRedactor(bc.redact)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if bc.block {
					l.LogBlock(iterlog.LevelDebug, "🔧", "[campaign#3/claw] tool input Bash:", body)
				} else {
					l.Logf(iterlog.LevelDebug, "🔧", line, args()...)
				}
			}
		})
	}
}

// The kill switch is the guard's own: ITERION_SECRETS_REDACT=off builds a
// guard whose Redact passes text through, and BuildExecutor wires no
// redactor for a nil guard — so the chokepoint adds no knob of its own.
// Pinned here so a future "smaller" redactor cannot silently re-arm it.
func TestRunLoggerChokepointHonoursTheKillSwitch(t *testing.T) {
	t.Parallel()
	const secret = "sk-proj-KILLSWITCH0123456789abcdefghijkl"
	g := secretguard.New([]secretguard.Secret{{Name: "OPENAI_API_KEY", Value: secret}},
		secretguard.Config{RedactKnown: false, Heuristic: false, MinLen: 5, Marker: "[redacted]", DecodeDepth: 2})
	var runLog strings.Builder
	em := &captureEmitter{}
	logger := iterlog.New(iterlog.LevelInfo, &runLog).WithRedactor(g.Redact)
	hooks := NewStoreEventHooks(context.Background(), em, "run-killswitch", logger, g, nil)

	hooks.OnDelegateError("n", DelegateInfo{BackendName: "claw",
		Error: errors.New("boom: " + secret)})

	if !strings.Contains(runLog.String(), secret) {
		t.Errorf("ITERION_SECRETS_REDACT=off must pass log text through, got: %q", runLog.String())
	}
}
