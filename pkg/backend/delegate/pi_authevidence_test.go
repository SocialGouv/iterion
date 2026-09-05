package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// piStubBackend stands in for a transport so the wiring — not the helper —
// is what the test exercises.
type piStubBackend struct{ err error }

func (s piStubBackend) Execute(context.Context, Task) (Result, error) {
	return Result{BackendName: BackendPi, ExitCode: 1}, s.err
}

func piAuthTask(t *testing.T, model string, sink *[]usagecap.Reading) Task {
	t.Helper()
	dir := t.TempDir()
	return Task{
		NodeID:  "think",
		WorkDir: dir,
		BaseDir: dir,
		Model:   model,
		Hooks: TaskHooks{OnUsageWindow: func(r usagecap.Reading) error {
			*sink = append(*sink, r)
			return nil
		}},
	}
}

// claude_code records a rejected credential as meter evidence so the next
// resolution routes around it (#624). pi minted the same typed error and
// recorded nothing, so a pi-backed fleet kept re-picking the dead
// credential on every resolution, gating the healthy tiers off behind it.
func TestPiExecute_RecordsAuthRefusalAsMeterEvidence(t *testing.T) {
	var got []usagecap.Reading
	b := NewPiBackend(testLogger(), "pi")
	b.rpc = piStubBackend{err: &ErrAuthFailed{Provider: BackendPi, Detail: "upstream 401"}}

	_, err := b.Execute(context.Background(), piAuthTask(t, "anthropic/claude-opus-5", &got))
	var auth *ErrAuthFailed
	if !errors.As(err, &auth) {
		t.Fatalf("Execute err = %v, want the typed auth failure to pass through unchanged", err)
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d readings, want 1 auth refusal", len(got))
	}
	if got[0].Window != usagecap.WindowAuth || got[0].Status != usagecap.StatusRejected {
		t.Fatalf("reading = %+v, want a rejected auth window", got[0])
	}
	if got[0].ObservedAt.IsZero() {
		t.Fatal("an undated reading is never fresh, so it would be evidence of nothing")
	}
	// The source names the credential the node actually ran on, or the
	// runner's meter charges the refusal to whichever key its default
	// precedence picks.
	if got[0].Source != "anthropic-direct" {
		t.Fatalf("Source = %q, want the anthropic-wire label", got[0].Source)
	}
}

// A refusal iterion cannot attribute must be recorded NOWHERE. pi routes
// ~36 providers behind one process; charging an openai rejection to the
// meter's anthropic-wire default would bench a credential that is fine.
func TestPiExecute_DoesNotChargeAnUnattributableRefusal(t *testing.T) {
	for _, model := range []string{"openai/gpt-5.5", "gpt-5.5"} {
		var got []usagecap.Reading
		b := NewPiBackend(testLogger(), "pi")
		b.rpc = piStubBackend{err: &ErrAuthFailed{Provider: BackendPi, Detail: "upstream 403"}}
		if _, err := b.Execute(context.Background(), piAuthTask(t, model, &got)); err == nil {
			t.Fatalf("%s: want the auth failure", model)
		}
		if len(got) != 0 {
			t.Fatalf("%s: recorded %+v — no evidence beats wrong evidence", model, got)
		}
	}
}

// Only a rejected CREDENTIAL is evidence. A workflow failure, a rate limit
// or a transient blip say nothing about the credential and must not bench it.
func TestPiExecute_RecordsNothingForANonAuthFailure(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("pi: the model produced no output"),
		&ErrTransient{Provider: BackendPi, Reason: "network"},
		&ErrRateLimited{Provider: BackendPi, Kind: RateLimitKindUsageWindow},
	} {
		var got []usagecap.Reading
		b := NewPiBackend(testLogger(), "pi")
		b.rpc = piStubBackend{err: err}
		_, _ = b.Execute(context.Background(), piAuthTask(t, "anthropic/claude-opus-5", &got))
		if len(got) != 0 {
			t.Fatalf("%v recorded %+v, want nothing", err, got)
		}
	}
}

func TestPiUsageSource(t *testing.T) {
	cases := map[string]string{
		"anthropic": "anthropic-direct",
		"zai":       "facade:pi-zai",
		"openai":    "",
		"":          "",
		"google":    "",
	}
	for provider, want := range cases {
		if got := piUsageSource(provider); got != want {
			t.Errorf("piUsageSource(%q) = %q, want %q", provider, got, want)
		}
	}
}
