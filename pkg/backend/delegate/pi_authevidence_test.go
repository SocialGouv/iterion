package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/sandbox"
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

// blankPiAnthropicEnv states the pod a pi Anthropic session reads: no
// credential in any channel pi or the claude CLI reads, no subscription
// switch, no cloud switch, and an agent dir holding no store — pi's own
// auth.json answers before every variable, so a host carrying a pi login
// would otherwise decide these rows. It returns that agent dir, for the rows
// that plant a store in it.
func blankPiAnthropicEnv(t *testing.T) string {
	t.Helper()
	resetClaudeCredEnv(t)
	for _, k := range []string{
		"ANTHROPIC_OAUTH_TOKEN", "ITERION_FORBID_SUBSCRIPTION_OAUTH", "PI_CODING_AGENT_DIR",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	t.Setenv("ITERION_PI_AGENT_DIR", dir)
	return dir
}

// claude_code records a rejected credential as meter evidence so the next
// resolution routes around it (#624). pi minted the same typed error and
// recorded nothing, so a pi-backed fleet kept re-picking the dead
// credential on every resolution, gating the healthy tiers off behind it.
// The pod env is blanked: the shape pinned here is a spend no credential
// names, and a host that carries one would (correctly) relabel the reading.
func TestPiExecute_RecordsAuthRefusalAsMeterEvidence(t *testing.T) {
	blankPiAnthropicEnv(t)
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
	// precedence picks. With nothing held and nothing inherited that is the
	// bare ambient label — no credential names the spend — never the slot
	// label of a key the run does not hold.
	if got[0].Source != "anthropic-env" {
		t.Fatalf("Source = %q, want the bare ambient label", got[0].Source)
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
		// The slug names no credential on pi's Anthropic provider: which of
		// its channels answered is piAnthropicSpend's question.
		"anthropic": "",
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

// piRecordedRefusals drives one auth refusal through Execute and returns the
// readings it recorded.
func piRecordedRefusals(t *testing.T, ctx context.Context, model, hint string, creds *secrets.Credentials) []usagecap.Reading {
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
	return got
}

// piRecordedRefusalSource is piRecordedRefusals for a row that expects its one
// reading: the source label that reading carries.
func piRecordedRefusalSource(t *testing.T, ctx context.Context, model, hint string, creds *secrets.Credentials) string {
	t.Helper()
	got := piRecordedRefusals(t, ctx, model, hint, creds)
	if len(got) != 1 {
		t.Fatalf("recorded %d readings, want 1 auth refusal", len(got))
	}
	return got[0].Source
}

// piAmbientLabel is the fingerprinted ambient label naming one credential
// value, spelled from the wire shape the runner parses.
func piAmbientLabel(value string) string {
	return "anthropic-env:" + secrets.FingerprintSHA256(value)
}

// A session spending a credential the pod hands it names THAT credential's
// account: the runner keys the fingerprinted ambient label on the platform
// meter, and its pre-flight reads the same meter for the route (the same
// resolution, PiAnthropicRouteSource), so a wall one pi run measures reaches
// every other borrower's pre-flight — claude_code's too, on a pod that carries
// that one key alone. A run with an anthropic key of its own (default or
// pinned) spends THAT key: the slot label. A pod with nothing inherited names
// no credential: the bare label. A z.ai refusal keeps its facade label. A
// stamped run's pi node spends nothing: until pi's arm lands, the interim
// guard refuses its leg before pi starts (ADR-161 decision 9), so no reading
// indicts any account.
func TestPiExecute_TheAmbientAccountNamesItselfInTheRefusal(t *testing.T) {
	blankPiAnthropicEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	ambientLabel := piAmbientLabel("sk-pod")
	heldKey := &secrets.Credentials{
		APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"},
		Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-run"},
	}
	pinnedKey := &secrets.Credentials{
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-pinned"},
		Fingerprints:  map[string]string{string(secrets.ProviderAnthropic): "fp-pinned"},
	}
	stamped := &secrets.Credentials{AmbientRefused: true}
	ctx := context.Background()

	if got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", nil); got != ambientLabel {
		t.Errorf("nothing held: Source = %q, want the pod account's label %q", got, ambientLabel)
	}
	if got := piRecordedRefusalSource(t, ctx, "anthropic/claude-opus-5", "", nil); got != ambientLabel {
		t.Errorf("spec prefix: Source = %q, want the pod account's label %q — the hint is not the only way anthropic resolves", got, ambientLabel)
	}
	if got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", heldKey); got != PiUsageSourceDirect {
		t.Errorf("a run with its own anthropic key: Source = %q, want %q — piResolveEnv binds that key, the slot label is the right one", got, PiUsageSourceDirect)
	}
	if got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", pinnedKey); got != PiUsageSourceDirect {
		t.Errorf("a pinned anthropic key: Source = %q, want %q — piRouteEnv binds the pin", got, PiUsageSourceDirect)
	}
	if got := piRecordedRefusals(t, ctx, "claude-opus-5-5", "anthropic", stamped); len(got) != 0 {
		t.Errorf("a stamped run recorded %+v — the guard refuses the leg before pi spends the pod's key", got)
	}

	// The facade providers keep their slug's label even beside a live pod key.
	if got := piRecordedRefusalSource(t, ctx, "glm-5.3", "zai", nil); got != PiUsageSourceZAI {
		t.Errorf("a zai refusal beside a live pod key: Source = %q, want %q", got, PiUsageSourceZAI)
	}

	t.Run("a pod with nothing inherited names no credential", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		if got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", nil); got != "anthropic-env" {
			t.Errorf("Source = %q, want the bare ambient label — no credential names the spend", got)
		}
	})
}

// pi reads its OWN channel set, in its own order — ANTHROPIC_AUTH_TOKEN, then
// ANTHROPIC_OAUTH_TOKEN, then ANTHROPIC_API_KEY, the first live one alone on
// the request — and none of claude_code's CLAUDE_CODE_OAUTH_TOKEN or
// ANTHROPIC_CUSTOM_HEADERS. Its refusal names the credential pi resolved, not
// the set the claude CLI reads: on the forfait pod (CLAUDE_CODE_OAUTH_TOKEN on
// the runner) a pi refusal stamped with claude_code's reading lands on the
// forfait's PLATFORM meter and parks every tenant's claude_code runs on an
// account pi never spent.
func TestPiExecute_TheRefusalNamesTheCredentialPiResolves(t *testing.T) {
	ctx := context.Background()

	t.Run("the forfait pod carries nothing pi reads", func(t *testing.T) {
		blankPiAnthropicEnv(t)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-forfait")
		t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization: Bearer hdr-pod")
		forfait := AmbientCredentialFingerprint()
		if forfait == "" {
			t.Fatal("inert bench: claude_code's reading names no account on this pod")
		}
		got := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", nil)
		if got == ambientSourcePrefix+forfait {
			t.Fatalf("Source = %q is the forfait's meter — the refusal would park claude_code's runs on an account pi never spent", got)
		}
		if got != "anthropic-env" {
			t.Errorf("Source = %q, want the bare ambient label — pi reads neither CLAUDE_CODE_OAUTH_TOKEN nor the header block", got)
		}
	})

	t.Run("the OAuth token rides ahead of the API key", func(t *testing.T) {
		blankPiAnthropicEnv(t)
		t.Setenv("ANTHROPIC_OAUTH_TOKEN", "oat-expired")
		t.Setenv("ANTHROPIC_API_KEY", "sk-good")
		if got, want := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", nil), piAmbientLabel("oat-expired"); got != want {
			t.Errorf("Source = %q, want %q — pi spent the OAuth token, and the key's meter would bench a healthy key", got, want)
		}
	})

	t.Run("the bearer rides ahead of the API key, alone", func(t *testing.T) {
		blankPiAnthropicEnv(t)
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-pod")
		t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
		if got, want := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", nil), piAmbientLabel("tok-pod"); got != want {
			t.Errorf("Source = %q, want %q — pi sends the bearer alone, not the set the claude CLI sends", got, want)
		}
	})

	t.Run("an explicit Anthropic base URL is pi's own destination", func(t *testing.T) {
		blankPiAnthropicEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com")
		t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
		if got, want := piRecordedRefusalSource(t, ctx, "claude-opus-5-5", "anthropic", nil), piAmbientLabel("sk-pod"); got != want {
			t.Errorf("Source = %q, want %q", got, want)
		}
	})

	t.Run("a credential the pod wires elsewhere is withheld", func(t *testing.T) {
		blankPiAnthropicEnv(t)
		t.Setenv("ANTHROPIC_BASE_URL", "https://llm-gw.example.test/anthropic")
		t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-gateway")
		if got := piRecordedRefusals(t, ctx, "claude-opus-5-5", "anthropic", nil); len(got) != 0 {
			t.Errorf("recorded %+v — pi sent the gateway's token to api.anthropic.com, and that refusal says nothing of the gateway account claude_code's sessions share", got)
		}
	})
}

// pi's own store answers before every variable: an entry for the provider in
// the agent dir's auth.json owns it, and pi never falls back past it. iterion
// neither handed that credential nor can name it, so it records no evidence
// rather than charging the refusal to the env credential pi did not spend. A
// store pi itself could not read counts as one: what pi spends there is not
// something iterion can rule out.
func TestPiExecute_PisOwnStoreWithholdsTheReading(t *testing.T) {
	ctx := context.Background()
	plant := func(t *testing.T, dir, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, body string
		withheld   bool
	}{
		{"an anthropic login", `{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":1}}`, true},
		{"an anthropic API key", `{"anthropic":{"type":"api_key","key":"sk-stored"}}`, true},
		{"a store pi cannot parse", `{"anthropic":`, true},
		{"another provider's login", `{"openai-codex":{"type":"oauth","access":"a","refresh":"r","expires":1}}`, false},
		{"a null entry", `{"anthropic":null}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := blankPiAnthropicEnv(t)
			t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
			plant(t, dir, tc.body)
			got := piRecordedRefusals(t, ctx, "claude-opus-5-5", "anthropic", nil)
			switch {
			case tc.withheld && len(got) != 0:
				t.Errorf("recorded %+v — the store answered, not the pod's key", got)
			case !tc.withheld && (len(got) != 1 || got[0].Source != piAmbientLabel("sk-pod")):
				t.Errorf("recorded %+v, want one reading naming the pod's key — the store holds no anthropic entry", got)
			}
		})
	}

	t.Run("the operator's own agent dir is the store pi reads", func(t *testing.T) {
		blankPiAnthropicEnv(t)
		t.Setenv("ITERION_PI_AGENT_DIR", "")
		own := t.TempDir()
		t.Setenv("PI_CODING_AGENT_DIR", own)
		t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
		plant(t, own, `{"anthropic":{"type":"api_key","key":"sk-stored"}}`)
		if got := piRecordedRefusals(t, ctx, "claude-opus-5-5", "anthropic", nil); len(got) != 0 {
			t.Errorf("recorded %+v — PI_CODING_AGENT_DIR's store answered, not the pod's key", got)
		}
	})
}

// A sandboxed pi session reads the CONTAINER's agent dir: no host_state bind
// carries this host's ~/.pi there, so a login in the host's default store
// answers nothing in the container — the session spends the channels
// piSandboxEnv forwards, and its refusal names that account; the pre-flight
// reads the same one. On the host, and through the noop driver's host passthrough, the same
// store answers first and the reading is withheld. A dir the env names is
// read at its path either way: the binds keep the path the container sees.
func TestPiExecute_TheSandboxedSessionReadsTheContainersStore(t *testing.T) {
	ctx := context.Background()
	blankPiAnthropicEnv(t)
	t.Setenv("ITERION_PI_AGENT_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostStore := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(hostStore, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostStore, "auth.json"), []byte(`{"anthropic":{"type":"api_key","key":"sk-stored"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	refusals := func(t *testing.T, sb sandbox.Run) []usagecap.Reading {
		t.Helper()
		var got []usagecap.Reading
		b := NewPiBackend(testLogger(), "pi")
		b.rpc = piStubBackend{err: &ErrAuthFailed{Provider: BackendPi, Detail: "upstream 401"}}
		task := piAuthTask(t, "claude-opus-5-5", &got)
		task.ProviderHint = "anthropic"
		task.Sandbox = sb
		_, err := b.Execute(ctx, task)
		var auth *ErrAuthFailed
		if !errors.As(err, &auth) {
			t.Fatalf("bench: Execute answered %v, not the transport's auth refusal", err)
		}
		return got
	}

	if got := refusals(t, nil); len(got) != 0 {
		t.Fatalf("bench: the host session recorded %+v — the host store answers first", got)
	}
	if got := refusals(t, stubNoopSandbox{}); len(got) != 0 {
		t.Errorf("the noop passthrough recorded %+v — it runs on the host, whose store answers first", got)
	}
	got := refusals(t, stubSandboxRun{})
	if len(got) != 1 || got[0].Source != piAmbientLabel("sk-pod") {
		t.Errorf("the sandboxed session recorded %+v, want one reading naming the forwarded key — the container never sees the host's store", got)
	}
	if source, metered, _ := PiAnthropicRouteSource(ctx, "claude-opus-5-5", "anthropic", true); !metered || source != piAmbientLabel("sk-pod") {
		t.Errorf("the sandboxed pre-flight = %q metered=%v, want the forwarded key's label", source, metered)
	}
	if source, metered, _ := PiAnthropicRouteSource(ctx, "claude-opus-5-5", "anthropic", false); metered {
		t.Errorf("the host pre-flight = %q metered — the host store answers first", source)
	}

	pinned := t.TempDir()
	if err := os.WriteFile(filepath.Join(pinned, "auth.json"), []byte(`{"anthropic":{"type":"api_key","key":"sk-stored"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_PI_AGENT_DIR", pinned)
	if got := refusals(t, stubSandboxRun{}); len(got) != 0 {
		t.Errorf("a sandboxed session on a pinned agent dir recorded %+v — the store at that path answers", got)
	}
}

// A key iterion binds for the run is the one pi spends. pi reads
// ANTHROPIC_AUTH_TOKEN and ANTHROPIC_OAUTH_TOKEN BEFORE ANTHROPIC_API_KEY, so a
// run key bound by its own variable alone left the pod's inherited bearer on
// every request: the pod's account paid for the run, and the refusal recorded
// under the run's slot label benched the run's own healthy key. Both binding
// sites — the run's default key (piResolveEnv) and a key a shared tier pinned
// for the route (piRouteEnv) — clear the outranking channels on every path the
// env takes, the host transport's and the sandboxed one.
func TestPiBindsTheRunsAnthropicKeyOverInheritedBearers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds secrets.Credentials
		key   string
	}{
		{"the run's own key", secrets.Credentials{
			APIKeys:      map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-run"},
			Fingerprints: map[string]string{string(secrets.ProviderAnthropic): "fp-run"},
		}, "sk-run"},
		{"a key pinned for the route", secrets.Credentials{
			PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-pinned"},
			Fingerprints:  map[string]string{string(secrets.ProviderAnthropic): "fp-pinned"},
		}, "sk-pinned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blankPiAnthropicEnv(t)
			// Hermetic: the sandboxed env forwards the host's credential
			// variables by name, and a developer machine carries real ones.
			for _, name := range piCredentialEnvNames {
				t.Setenv(name, "")
			}
			t.Setenv("ITERION_PI_MODE", "")
			t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-pod")
			t.Setenv("ANTHROPIC_OAUTH_TOKEN", "oat-pod")
			t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
			ctx := secrets.WithCredentials(context.Background(), tc.creds)

			rpc := &captureTransport{}
			b := &PiBackend{rpc: rpc}
			if _, err := b.Execute(ctx, Task{NodeID: "opus", Model: "claude-opus-5-5", ProviderHint: "anthropic", WorkDir: t.TempDir()}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			host := map[string]string{}
			for _, kv := range piRPCEnv(ctx, rpc.task, nil, "") {
				if k, v, ok := strings.Cut(kv, "="); ok {
					host[k] = v // the exec env keeps the last value of a duplicated key
				}
			}
			sandboxed := (&CLIAgentBackend{Protocol: piProtocol}).sandboxEnv(ctx, rpc.task)
			for path, env := range map[string]map[string]string{"host": host, "sandbox": sandboxed} {
				if env["ANTHROPIC_API_KEY"] != tc.key {
					t.Errorf("%s env: ANTHROPIC_API_KEY is not the bound key", path)
				}
				for _, name := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN"} {
					if env[name] != "" {
						t.Errorf("%s env carries the pod's %s — pi reads it before the bound key and spends the pod's account", path, name)
					}
				}
			}
			if got := piRecordedRefusalSource(t, context.Background(), "claude-opus-5-5", "anthropic", &tc.creds); got != PiUsageSourceDirect {
				t.Errorf("Source = %q, want %q — the bound key is what pi spent", got, PiUsageSourceDirect)
			}
		})
	}

	// Binding another provider's key touches no anthropic channel.
	t.Run("another provider's key leaves the anthropic channels alone", func(t *testing.T) {
		blankPiAnthropicEnv(t)
		env := piResolveEnv(secrets.WithCredentials(context.Background(), secrets.Credentials{
			APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: "sk-openai"},
		}))
		for _, name := range piAnthropicEnvChannels {
			if _, ok := env[name]; ok {
				t.Errorf("binding an openai key overrode %s", name)
			}
		}
	})
}

// The subscription switch refuses a pi session whose only Anthropic credential
// is the run's Claude forfait before it spawns (noticeSubscriptionOAuth): it
// spends nothing, so no meter names the route. Under the same switch an
// inherited subscription bearer is stripped from pi's env (piResolveEnv), and
// the API key behind it is what pi spends. A route pi resolves to another
// provider is not this reading's to name.
func TestPiAnthropicRouteSource_FollowsTheSubscriptionSwitch(t *testing.T) {
	blankPiAnthropicEnv(t)
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	forfaitOnly := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): t.TempDir()},
	})
	if _, metered, anthropic := PiAnthropicRouteSource(forfaitOnly, "claude-opus-5-5", "anthropic", false); !anthropic || metered {
		t.Errorf("a forfait-only run under the switch: anthropic=%v metered=%v, want an anthropic route no meter names — the session never spawns", anthropic, metered)
	}

	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-ant-oat-pod")
	t.Setenv("ANTHROPIC_API_KEY", "sk-pod")
	if source, metered, _ := PiAnthropicRouteSource(context.Background(), "claude-opus-5-5", "anthropic", false); !metered || source != piAmbientLabel("sk-pod") {
		t.Errorf("a stripped subscription bearer: source=%q metered=%v, want the API key behind it, %q", source, metered, piAmbientLabel("sk-pod"))
	}

	if _, _, anthropic := PiAnthropicRouteSource(context.Background(), "glm-5.3", "zai", false); anthropic {
		t.Error("a zai route answered as an anthropic one — its facade label is the slug's")
	}
}
