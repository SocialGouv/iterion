package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
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
// The pod env is blanked: the shape pinned here is a spend no inherited
// credential names, and a host that carries one would (correctly) relabel
// the reading.
func TestPiExecute_RecordsAuthRefusalAsMeterEvidence(t *testing.T) {
	resetClaudeCredEnv(t)
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
		"anthropic": PiUsageSourceDirect,
		"zai":       PiUsageSourceZAI,
		// pi's OWN id for Moonshot — what piResolveModel returns after
		// piProviderPrefixes. Matching iterion's "moonshot" here would
		// never fire, and the refusal would land on the default credential.
		"moonshotai": PiUsageSourceMoonshot,
		"moonshot":   "",
		"openai":     "",
		"":           "",
		"google":     "",
	}
	for provider, want := range cases {
		if got := piUsageSource(provider); got != want {
			t.Errorf("piUsageSource(%q) = %q, want %q", provider, got, want)
		}
	}
}

// piRecordedRefusalSource drives one auth refusal through Execute and returns
// the source label the reading carries.
func piRecordedRefusalSource(t *testing.T, ctx context.Context, model, hint string, creds *secrets.Credentials) string {
	t.Helper()
	var got []usagecap.Reading
	b := NewPiBackend(testLogger(), "pi")
	b.rpc = piStubBackend{err: &ErrAuthFailed{Provider: BackendPi, Detail: "upstream 401"}}
	if creds != nil {
		ctx = secrets.WithCredentials(ctx, *creds)
	}
	task := piAuthTask(t, model, &got)
	task.ProviderHint = hint
	if _, err := b.Execute(ctx, task); err == nil {
		t.Fatal("want the typed auth failure to pass through")
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d readings, want 1 auth refusal", len(got))
	}
	return got[0].Source
}

// A session spending the pod's inherited account names THAT account: the
// runner keys the fingerprinted ambient label on the platform account's
// meter — the meter its nothing-held pre-flight already reads for a pi route
// with an anthropic hint — so a wall one pi run measures reaches every other
// borrower's pre-flight. The clamp is exact: a run with an anthropic
// credential of its own (default or pinned — either one rides the env
// piResolveEnv/piRouteEnv compose), a run that refused the pod env, a pod
// with nothing inherited, and every non-anthropic provider keep the slot
// label piUsageSource mints.
func TestPiExecute_TheAmbientAccountNamesItselfInTheRefusal(t *testing.T) {
	resetClaudeCredEnv(t)
	for _, k := range []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ambientLabel := "anthropic-env:" + secrets.FingerprintSHA256("sk-pod")
	heldKey := &secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-run"},
	}
	pinnedKey := &secrets.Credentials{
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-pinned"},
		Fingerprints:  map[string]string{string(secrets.ProviderAnthropic): "fp-pinned"},
	}
	refusedPod := &secrets.Credentials{AmbientRefused: true}
	ctx := context.Background()

	if got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", nil); got != ambientLabel {
		t.Errorf("nothing held: Source = %q, want the pod account's label %q — the bare slot label books on a meter no pre-flight of this shape reads", got, ambientLabel)
	}
	if got := piRecordedRefusalSource(t, ctx, "anthropic/claude-opus-5", "", nil); got != ambientLabel {
		t.Errorf("spec prefix: Source = %q, want the pod account's label %q — the hint is not the only way anthropic resolves", got, ambientLabel)
	}
	if got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", heldKey); got != PiUsageSourceDirect {
		t.Errorf("a run with its own anthropic key: Source = %q, want %q — pi's env carries that key, the slot label is the right one", got, PiUsageSourceDirect)
	}
	if got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", pinnedKey); got != PiUsageSourceDirect {
		t.Errorf("a pinned anthropic key: Source = %q, want %q — piRouteEnv puts the pin in the env", got, PiUsageSourceDirect)
	}
	if got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", refusedPod); got != PiUsageSourceDirect {
		t.Errorf("a run that refused the pod env: Source = %q, want %q — its nothing-held default spends nothing of the pod's", got, PiUsageSourceDirect)
	}

	// The clamp is label-specific: a z.ai refusal keeps its facade label even
	// beside a live pod key.
	if got := piRecordedRefusalSource(t, ctx, "glm-5.3", "zai", nil); got != PiUsageSourceZAI {
		t.Errorf("a zai refusal beside a live pod key: Source = %q, want %q", got, PiUsageSourceZAI)
	}

	t.Run("a pod with nothing inherited keeps the slot label", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		if got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", nil); got != PiUsageSourceDirect {
			t.Errorf("Source = %q, want %q — no inherited account names the spend", got, PiUsageSourceDirect)
		}
	})
}
