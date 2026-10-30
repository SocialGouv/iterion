package model

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// Inside the sandbox the claw runner rebuilds its registry from ENV alone —
// ctx does not cross a process boundary. So whatever forwardableProviderEnv
// omits, the node authenticates without.
//
// Measured on prod before this: a pool-served run (019fcc09) reached the
// pod with its donated ChatGPT credential materialised, then spent the
// PLATFORM's OPENAI_API_KEY instead, because only ambient env crossed. The
// donor's lease had already been taken. Whenever the ambient key happens to
// work, that substitution is completely invisible — which is the worst
// possible shape for a billing boundary.
func TestForwardableProviderEnv_runCredentialsBeatTheAmbientOnes(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "platform-key")
	t.Setenv("ANTHROPIC_API_KEY", "platform-anthropic")

	t.Run("no credentials in ctx leaves the ambient env alone", func(t *testing.T) {
		env := envFor(t, context.Background())
		if env["OPENAI_API_KEY"] != "platform-key" {
			t.Errorf("OPENAI_API_KEY = %q, want the ambient value passed through", env["OPENAI_API_KEY"])
		}
		if _, ok := env["CODEX_HOME"]; ok {
			t.Error("CODEX_HOME set with no resolved forfait")
		}
	})

	t.Run("a tenant's BYOK key overrides the platform's", func(t *testing.T) {
		ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
			APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: "tenant-key"},
		})
		env := envFor(t, ctx)
		if env["OPENAI_API_KEY"] != "tenant-key" {
			t.Errorf("OPENAI_API_KEY = %q, want the tenant's own key — a sandboxed node billed the platform instead", env["OPENAI_API_KEY"])
		}
		// Providers the run resolved nothing for keep the ambient value:
		// the run is not asserting anything about them.
		if env["ANTHROPIC_API_KEY"] != "platform-anthropic" {
			t.Errorf("ANTHROPIC_API_KEY = %q, want the ambient value untouched", env["ANTHROPIC_API_KEY"])
		}
	})

	t.Run("a resolved ChatGPT forfait wins over an ambient key", func(t *testing.T) {
		ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
			OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindCodex): "/host/tmp/whatever"},
		})
		env := envFor(t, ctx)
		if env["CODEX_HOME"] != secrets.CodexSandboxConfigDir {
			t.Errorf("CODEX_HOME = %q, want the in-sandbox seeded dir (the HOST path does not exist in the container)", env["CODEX_HOME"])
		}
		if env["ITERION_OPENAI_USE_OAUTH"] != "1" {
			t.Error("the forfait must be forced: an ambient OPENAI_API_KEY would otherwise win by the " +
				"\"an explicit env key is deliberate\" rule, which is true of a machine default and false of a per-run credential")
		}
	})

	t.Run("an empty resolved key never blanks the ambient one", func(t *testing.T) {
		ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
			APIKeys: map[secrets.Provider]string{secrets.ProviderOpenAI: ""},
		})
		if got := envFor(t, ctx)["OPENAI_API_KEY"]; got != "platform-key" {
			t.Errorf("OPENAI_API_KEY = %q — an absent resolution must not read as 'authenticate with nothing'", got)
		}
	})
}

// The ChatGPT-forfait wire gates model access on the codex-cli version
// header, and the sandbox image ships no codex binary — so the operator's
// ITERION_CODEX_VERSION override must cross into the in-container runner
// or newer models 400 only when sandboxed.
func TestForwardableProviderEnv_forwardsCodexVersionOverride(t *testing.T) {
	t.Setenv("ITERION_CODEX_VERSION", "0.144.6")
	env := envFor(t, context.Background())
	if env["ITERION_CODEX_VERSION"] != "0.144.6" {
		t.Errorf("ITERION_CODEX_VERSION = %q, want the ambient override forwarded", env["ITERION_CODEX_VERSION"])
	}
}

// With NO operator override, the HOST-probed `codex --version` value must
// cross the boundary instead. Measured live (run 01a04514, 2026-08-27): host
// codex-cli 0.144.6 present, env unset → the in-container runner fell back
// to claw's baked-in version and OpenAI 400'd gpt-5.6-sol ("requires a newer
// version of Codex"), failing the plan_review node the host could serve.
func TestForwardableProviderEnv_forwardsHostProbedCodexVersion(t *testing.T) {
	t.Setenv("ITERION_CODEX_VERSION", "")
	orig := hostCodexVersion
	t.Cleanup(func() { hostCodexVersion = orig })

	hostCodexVersion = func() string { return "0.144.6" }
	env := envFor(t, context.Background())
	if env["ITERION_CODEX_HOST_VERSION"] != "0.144.6" {
		t.Errorf("ITERION_CODEX_HOST_VERSION = %q, want the host-probed version forwarded as a probe when no override is set", env["ITERION_CODEX_HOST_VERSION"])
	}

	// No codex on the host either: forward nothing, never an empty pair.
	hostCodexVersion = func() string { return "" }
	env = envFor(t, context.Background())
	if v, ok := env["ITERION_CODEX_HOST_VERSION"]; ok {
		t.Errorf("ITERION_CODEX_HOST_VERSION = %q set with nothing to forward — an empty value must not cross", v)
	}
}

// #736: a sandboxed claw anthropic node for a forfait-only tenant must not
// authenticate as the pod. The in-container runner rebuilds its registry from
// env alone, so the run's forfait reaches it only as CLAUDE_CONFIG_DIR (the
// dir runtime.seedClaudeConfigDir populates) — and only if the ambient
// anthropic-wire vars, which are the PLATFORM's, stop shadowing it. Leaving
// them in place is invisible precisely because the platform key works.
func TestForwardableProviderEnv_ForfaitOnlyTenantDoesNotInheritPlatformKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-PLATFORM")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ZAI_API_KEY", "")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)},
	})
	env := envFor(t, ctx)

	if got := env["CLAUDE_CONFIG_DIR"]; got != secrets.ClaudeCodeSandboxConfigDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want the seeded in-sandbox dir %q", got, secrets.ClaudeCodeSandboxConfigDir)
	}
	for _, shadow := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ZAI_API_KEY"} {
		if v, present := env[shadow]; present {
			t.Errorf("%s=%q still forwarded — the platform credential would win over the run's forfait", shadow, v)
		}
	}
}

// The tenant's OWN key is an explicit choice and keeps precedence: holding a
// forfait as well must not silently switch the run onto the subscription.
func TestForwardableProviderEnv_TenantKeyBeatsItsOwnForfait(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-PLATFORM")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-TENANT"},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)},
	})
	env := envFor(t, ctx)

	if env["ANTHROPIC_API_KEY"] != "sk-ant-TENANT" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the tenant's own key", env["ANTHROPIC_API_KEY"])
	}
	if _, present := env["CLAUDE_CONFIG_DIR"]; present {
		t.Error("CLAUDE_CONFIG_DIR set even though the tenant's own key serves — the run would switch instrument when sandboxed")
	}
}

// A gateway base URL is a destination the operator chose; a subscription bearer
// carries the whole Claude account and must not travel there implicitly.
func TestForwardableProviderEnv_ForfaitNotPointedAtRedirectedWire(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://llm-gateway.corp.example")
	t.Setenv("ZAI_API_KEY", "")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)},
	})
	if _, present := envFor(t, ctx)["CLAUDE_CONFIG_DIR"]; present {
		t.Error("forfait pointed into the container while the wire is redirected at a gateway")
	}
}

// forfaitDirForTest writes a valid, unexpired forfait blob and returns its dir.
func forfaitDirForTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-run","expiresAt":4102444800000}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// envFor calls the sandbox env builder for a first-party Anthropic model and
// fails the test on error.
func envFor(t *testing.T, ctx context.Context) map[string]string {
	t.Helper()
	env, _, err := forwardableProviderEnv(ctx, "anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	return env
}

// R97bef9 [high]: a z.ai/GLM model rides the SAME anthropic provider — it
// arrives as "anthropic/glm-X" and the registry synthesises z.ai's base URL
// from a bare ZAI_API_KEY, so no ANTHROPIC_BASE_URL exists for a wire check to
// see. Clearing ZAI_API_KEY there removes the node's only credential channel
// and points a forfait bearer at api.anthropic.com for a model it cannot
// serve — breaking exactly the forfait-carrying tenants #736 is for.
func TestForwardableProviderEnv_GLMNodeKeepsItsZAIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "zai-platform-key")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)},
	})
	env, _, err := forwardableProviderEnv(ctx, "anthropic/glm-5.2")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if env["ZAI_API_KEY"] != "zai-platform-key" {
		t.Errorf("ZAI_API_KEY = %q — a GLM node loses its only credential channel", env["ZAI_API_KEY"])
	}
	if _, present := env["CLAUDE_CONFIG_DIR"]; present {
		t.Error("forfait pointed at a GLM node the forfait cannot serve")
	}
}

// The forfait crossing concerns a claude model on claw's anthropic provider
// and nothing else: a node on another provider has no use for the Claude
// forfait, and an EXPIRED one — which refuses an anthropic node by name —
// must not fail it.
func TestForwardableProviderEnv_OtherProviderIgnoresTheForfait(t *testing.T) {
	dir := t.TempDir()
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-stale","expiresAt":1000000000000}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): dir},
	})
	env, _, err := forwardableProviderEnv(ctx, "openrouter/anthropic/claude-sonnet-5")
	if err != nil {
		t.Fatalf("an openrouter node failed on the run's expired Claude forfait: %v", err)
	}
	if _, present := env["CLAUDE_CONFIG_DIR"]; present {
		t.Error("the Claude forfait was pointed at an openrouter node")
	}
}

// R71d7c3 [medium]: once the shadows are cleared the forfait is the node's ONLY
// credential in the container, and the in-container env factory swallows expiry
// — it would build a client with no credential and 401-loop with nothing naming
// the cause. The in-process twin already refuses rather than degrade; this seam
// must decide the same way, or the two disagree again.
func TestForwardableProviderEnv_ExpiredForfaitRefusesInsteadOfBlinding(t *testing.T) {
	dir := t.TempDir()
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-stale","expiresAt":1000000000000}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-PLATFORM")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): dir},
	})
	_, _, err := forwardableProviderEnv(ctx, "anthropic/claude-haiku-4-5")
	if !errors.Is(err, secrets.ErrAnthropicForfaitExpired) {
		t.Fatalf("expected a named expiry refusal, got %v", err)
	}
}

// The contract written above providerCredentialEnvVars, applied to the
// provider that just landed: a provider whose Resolve() reads os.Getenv must
// have its variable listed, or the in-container runner — which rebuilds its
// registry from env alone — resolves without it.
//
// Moonshot reads two. MOONSHOT_API_KEY is the loud half (the in-container
// factory refuses by name). MOONSHOT_BASE_URL is the silent one: the node
// keeps working, against the PUBLISHED endpoint, while the operator pinned
// another gateway on the host — one node, two vendors' infrastructure, and
// nothing said. So the assertion is not on the list, it is on the ANSWER the
// two sides give to the same question.
func TestForwardableProviderEnv_CarriesTheMoonshotRoute(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "moonshot-platform-key")
	t.Setenv("MOONSHOT_BASE_URL", "https://api.moonshot.cn/anthropic")
	hostBase := moonshotBaseURL()

	env, _, err := forwardableProviderEnv(context.Background(), "moonshot/kimi-k2")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}

	// The container sees ONLY what crossed: rebuild that view and ask the
	// registry's own resolvers, rather than reading the list back to itself.
	t.Setenv("MOONSHOT_API_KEY", env["MOONSHOT_API_KEY"])
	t.Setenv("MOONSHOT_BASE_URL", env["MOONSHOT_BASE_URL"])
	if got := moonshotBaseURL(); got != hostBase {
		t.Errorf("in-container Moonshot endpoint = %q, host = %q — the same node talks to two vendors' gateways", got, hostBase)
	}
	if _, err := NewRegistry().Resolve("moonshot/kimi-k2"); err != nil {
		t.Errorf("in-container resolve of a moonshot node: %v", err)
	}
}

// The stream-usage knob is the reporting twin of the base URL: the host and
// the container must read the same, or a sandboxed openai node keeps its
// stream tokens unreported while the host's accounting expects them.
func TestForwardableProviderEnv_CarriesTheStreamUsageKnob(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "https://gateway.operator.example/v1")
	t.Setenv("ITERION_OPENAI_STREAM_USAGE", "1")

	env, _, err := forwardableProviderEnv(context.Background(), "openai/gpt-5")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}

	// The container sees ONLY what crossed: rebuild that view and ask the
	// registry's own helper, rather than reading the list back to itself.
	t.Setenv("OPENAI_BASE_URL", env["OPENAI_BASE_URL"])
	t.Setenv("ITERION_OPENAI_STREAM_USAGE", env["ITERION_OPENAI_STREAM_USAGE"])
	if !openAIStreamUsage(os.Getenv("OPENAI_BASE_URL")) {
		t.Error("in-container openAIStreamUsage = false with the knob set — the sandboxed lane stays blind to its own usage")
	}
}

// #2292, the Moonshot shape on the xai provider: both xai factories spend
// XAI_API_KEY on whatever xaiBaseURL answers — an operator proxy, a regional
// host. Without the base URL across, the sandboxed node keeps working, against
// api.x.ai, while the host talks to the operator's endpoint.
func TestForwardableProviderEnv_CarriesTheXAIRoute(t *testing.T) {
	t.Setenv("XAI_API_KEY", "xai-platform-key")
	t.Setenv("XAI_BASE_URL", "https://xai-proxy.operator.example/v1")
	hostBase := xaiBaseURL()

	env, _, err := forwardableProviderEnv(context.Background(), "xai/grok-4")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}

	// The container sees ONLY what crossed: rebuild that view and ask the
	// registry's own resolver, rather than reading the list back to itself.
	t.Setenv("XAI_API_KEY", env["XAI_API_KEY"])
	t.Setenv("XAI_BASE_URL", env["XAI_BASE_URL"])
	if got := xaiBaseURL(); got != hostBase {
		t.Errorf("in-container xAI endpoint = %q, host = %q — the same node spends its key on two endpoints", got, hostBase)
	}
}

// The stream-silence watchdog runs where the provider call runs, and for a
// sandboxed node that is the runner: callAndAggregate is every in-process
// call's funnel. An operator who widened a tier for a slow endpoint, or
// turned one off with 0, must get the same watchdog on both sides.
func TestForwardableProviderEnv_CarriesTheStreamWatchdog(t *testing.T) {
	t.Setenv("ITERION_CLAW_STREAM_COLD_TIMEOUT", "12m")
	t.Setenv("ITERION_CLAW_STREAM_IDLE_TIMEOUT", "0")
	hostCold, hostIdle := resolveClawStreamColdTimeout(), resolveClawStreamIdleTimeout()

	env := envFor(t, context.Background())

	t.Setenv("ITERION_CLAW_STREAM_COLD_TIMEOUT", env["ITERION_CLAW_STREAM_COLD_TIMEOUT"])
	t.Setenv("ITERION_CLAW_STREAM_IDLE_TIMEOUT", env["ITERION_CLAW_STREAM_IDLE_TIMEOUT"])
	if got := resolveClawStreamColdTimeout(); got != hostCold {
		t.Errorf("in-container cold timeout = %s, host = %s", got, hostCold)
	}
	if got := resolveClawStreamIdleTimeout(); got != hostIdle {
		t.Errorf("in-container idle timeout = %s, host = %s — a tier the operator turned off is back on when sandboxed", got, hostIdle)
	}
}

// The workspace-agent ceiling is read where the delegation runs, so the
// sandboxed twin must resolve the SAME ceiling. The name crosses always
// (crossedByItsOwnBranch): the operator's value when one is set, an explicit
// empty when the host leaves it unset — an ABSENT name is one the runner's
// workspace .env auto-load fills, handing the repository under review the
// ceiling its own agent files are read under — and an explicit empty when
// the host's value was itself planted, which neither side obeys
// (workspaceAgentMaxBytes). Red when the crossing goes back to skipping
// empty entries: host-unset then crosses absent, and the .env road is open.
func TestForwardableProviderEnv_CarriesTheWorkspaceAgentCeiling(t *testing.T) {
	operatorEnv(t)
	t.Setenv(WorkspaceAgentMaxBytesEnv, "")

	env := envFor(t, context.Background())
	v, ok := env[WorkspaceAgentMaxBytesEnv]
	if !ok {
		t.Fatalf("host-unset %s crossed ABSENT — the runner's workspace .env auto-load fills it and picks the ceiling its own agent files are read under", WorkspaceAgentMaxBytesEnv)
	}
	if v != "" {
		t.Errorf("host-unset %s crossed as %q, want the explicit empty", WorkspaceAgentMaxBytesEnv, v)
	}

	// The operator's own override crosses with its value, and the container
	// resolves the same ceiling as the host: rebuild the in-container view
	// from what crossed alone and ask the resolver itself.
	t.Setenv(WorkspaceAgentMaxBytesEnv, "4096")
	hostCeiling := workspaceAgentMaxBytes()
	env = envFor(t, context.Background())
	t.Setenv(WorkspaceAgentMaxBytesEnv, env[WorkspaceAgentMaxBytesEnv])
	if got := workspaceAgentMaxBytes(); got != hostCeiling {
		t.Errorf("in-container ceiling = %d, host = %d — the same file is bounded two ways", got, hostCeiling)
	}

	// A planted value crosses as the explicit empty: it never spoke for the
	// operator, and forwarding it raw would launder it past the container's
	// own planted check (the name arrived present, so the runner's applyDotEnv
	// marked nothing).
	envtrust.ResetForTest()
	t.Setenv(WorkspaceAgentMaxBytesEnv, "1")
	envtrust.MarkPlanted(WorkspaceAgentMaxBytesEnv)
	env = envFor(t, context.Background())
	if got := env[WorkspaceAgentMaxBytesEnv]; got != "" {
		t.Errorf("a planted %s=%q crossed as %q — the repository under review set the ceiling its agent files are read under", WorkspaceAgentMaxBytesEnv, "1", got)
	}
}

// TestAPlantedWorkspaceAgentCeilingSpeaksForNobody: the byte ceiling is a
// security switch, so a value a project `.env` planted (cmd/iterion
// applyDotEnv auto-loads it, out of a repository under review included)
// carries no authority — "0" would unbound a read that becomes a billed
// system prompt, "1" would refuse every agent file. The operator's own
// export still governs; the planted one resolves the default, on the host
// and (through the runner's own auto-load marking) in the container.
func TestAPlantedWorkspaceAgentCeilingSpeaksForNobody(t *testing.T) {
	operatorEnv(t)
	t.Setenv(WorkspaceAgentMaxBytesEnv, "4096")
	if got := workspaceAgentMaxBytes(); got != 4096 {
		t.Fatalf("the operator's own override resolved to %d, want 4096", got)
	}

	t.Setenv(WorkspaceAgentMaxBytesEnv, "1")
	envtrust.MarkPlanted(WorkspaceAgentMaxBytesEnv)
	if got := workspaceAgentMaxBytes(); got != defaultWorkspaceAgentMaxBytes {
		t.Fatalf("a planted =1 resolved to %d — the repository under review moved the ceiling its agent files are read under", got)
	}
	// Through the resolver: the planted refusal-by-ceiling does not bite.
	dir := t.TempDir()
	writeWorkspaceAgent(t, dir, "auditor", "a body well past one byte\n")
	if _, found, err := LookupWorkspaceAgent(dir, "auditor", workspaceAgentMaxBytes()); err != nil || !found {
		t.Errorf("under a planted =1 the file must still resolve: found=%t err=%v", found, err)
	}
}

// The runner's claw backend retries the provider calls made in the
// container with the launcher's budget, which crosses in the task envelope,
// resolved: it bounds billed attempts, so neither the container's env — a
// repository's devcontainer can set it — nor the runner's own defaults stand
// in for the launcher's. The workspace's settings hooks cross the same way.
// Both are read off the wire of the real dispatch: the "runner" is
// `head -n 1`, which keeps the task envelope. The budgets are the ones a
// launcher builds from its env, every field but one left to the defaults.
func TestExecuteViaSandboxRunner_SendsTheLaunchersBudgetAndHooks(t *testing.T) {
	work := t.TempDir()
	hooks := `{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"true"}]}]}`
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".claude", "settings.json"), []byte(`{"hooks":`+hooks+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fromEnv := func(retries, transient string) RetryPolicy {
		t.Setenv("ITERION_NODE_MAX_RETRIES", retries)
		t.Setenv("ITERION_NODE_MAX_TRANSIENT_RETRIES", transient)
		return RetryPolicyFromEnv()
	}
	for _, tc := range []struct {
		name     string
		launcher RetryPolicy
	}{
		{name: "the defaults", launcher: fromEnv("", "")},
		{name: "fail-fast", launcher: fromEnv("0", "")},
		{name: "a wider transient budget", launcher: fromEnv("", "19")},
		{name: "every field set", launcher: RetryPolicy{MaxAttempts: 1, MaxAttemptsTransient: 4, BackoffBase: 7 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Whatever reads the budget from this env instead of the
			// launcher's policy sends nine retries.
			t.Setenv("ITERION_NODE_MAX_RETRIES", "9")
			t.Setenv("ITERION_NODE_MAX_TRANSIENT_RETRIES", "9")
			crossed := dispatchCapturingTheTask(t, NewClawBackend(NewRegistry(), EventHooks{}, tc.launcher), work)

			w := crossed.Retry
			if w == nil || w.MaxAttempts <= 0 || w.MaxAttemptsTransient <= 0 || w.BackoffBase <= 0 {
				t.Fatalf("the budget on the wire = %+v, want every field resolved", w)
			}
			got, want := RetryPolicyFromWire(w), tc.launcher
			if got.maxAttempts() != want.maxAttempts() || got.maxAttemptsTransient() != want.maxAttemptsTransient() || got.backoffBase() != want.backoffBase() {
				t.Errorf("the runner spends %d attempts (%d transient) backing off from %s, the launcher %d (%d) from %s",
					got.maxAttempts(), got.maxAttemptsTransient(), got.backoffBase(), want.maxAttempts(), want.maxAttemptsTransient(), want.backoffBase())
			}
			if got := string(crossed.SettingsHooks); got != hooks {
				t.Errorf("the runner's settings hooks = %s, the workspace's = %s", got, hooks)
			}
			// The launcher wraps the runner in the node's session and ends
			// it: the runner must draw none of its own, or what it stops is
			// reported into its own stderr and the launcher finds nothing.
			if !crossed.LauncherEndsNodeSession {
				t.Errorf("the task does not tell the runner the launcher ends the node's session")
			}
		})
	}
	// A launcher that predates the field: the runner keeps its built-in
	// defaults, never the budget its own env names.
	t.Setenv("ITERION_NODE_MAX_RETRIES", "0")
	t.Setenv("ITERION_NODE_MAX_TRANSIENT_RETRIES", "0")
	if got := RetryPolicyFromWire(nil); got != (RetryPolicy{}) {
		t.Errorf("no budget on the wire = %+v, want the zero policy", got)
	}
}

// dispatchCapturingTheTask sends one task through b's real sandbox dispatch
// to a runner that keeps the first envelope and never answers, and returns
// the task that envelope carried.
func dispatchCapturingTheTask(t *testing.T, b *ClawBackend, workDir string) delegate.IOTask {
	t.Helper()
	envelope := firstDispatchedEnvelope(t, b, context.Background(), delegate.Task{
		NodeID: "n", Model: "openai/gpt-5", WorkDir: workDir,
	})
	if envelope.Type != delegate.EnvelopeTask {
		t.Fatalf("the runner's first envelope is %q, want the task", envelope.Type)
	}
	var crossed delegate.IOTask
	if err := json.Unmarshal(envelope.Data, &crossed); err != nil {
		t.Fatalf("decode the task envelope: %v", err)
	}
	return crossed
}

// taskCaptureSandbox runs the claw runner as `head -n 1 > path`: it keeps the
// first envelope the launcher sends, then exits without answering.
type taskCaptureSandbox struct{ path string }

func (s *taskCaptureSandbox) Driver() string { return "task-capture" }
func (s *taskCaptureSandbox) Command(ctx context.Context, argv []string, _ sandbox.ExecOpts) *exec.Cmd {
	if isNodeSessionEnd(argv) {
		return exec.CommandContext(ctx, "true")
	}
	return exec.CommandContext(ctx, "sh", "-c", `head -n 1 > "$0"`, s.path)
}
func (s *taskCaptureSandbox) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (s *taskCaptureSandbox) Cleanup(context.Context) error { return nil }

// The run's own key still beats the ambient one across the seam — the whole
// point of the boundary is that a tenant's credential is what pays.
func TestForwardableProviderEnv_MoonshotBYOKBeatsTheAmbientKey(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "moonshot-platform-key")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-tenant-key"},
	})
	env, _, err := forwardableProviderEnv(ctx, "moonshot/kimi-k2")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if env["MOONSHOT_API_KEY"] != "moonshot-tenant-key" {
		t.Errorf("MOONSHOT_API_KEY = %q, want the tenant's own key", env["MOONSHOT_API_KEY"])
	}
}

// A Moonshot key funds no `anthropic/…` node — claw reaches Kimi through its
// own `moonshot/…` provider — so holding one must not keep the run's forfait
// out of the tenant's sandboxed anthropic nodes. When it did, the crossing was
// skipped and the platform's ambient Anthropic key crossed instead: the host
// path spends the forfait for the same node, the sandbox billed the platform.
func TestForwardableProviderEnv_MoonshotKeyDoesNotKeepTheForfaitOut(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-PLATFORM")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("MOONSHOT_API_KEY", "")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{secrets.ProviderMoonshot: "moonshot-tenant-key"},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)},
	})
	env := envFor(t, ctx)

	if got := env["CLAUDE_CONFIG_DIR"]; got != secrets.ClaudeCodeSandboxConfigDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want the seeded in-sandbox dir %q — the tenant's Moonshot key kept its forfait out", got, secrets.ClaudeCodeSandboxConfigDir)
	}
	if v, present := env["ANTHROPIC_API_KEY"]; present {
		t.Errorf("ANTHROPIC_API_KEY=%q forwarded — the platform key would serve the tenant's anthropic node", v)
	}
}

// The other half: the forfait crossing clears what would outrank the forfait
// for an ANTHROPIC node, and a moonshot node's key is not that. Deleting it
// left a sandboxed `moonshot/…` node of a forfait-holding tenant with no
// credential, while the same node works on the host.
func TestForwardableProviderEnv_ForfaitCrossingKeepsAMoonshotNodesKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("MOONSHOT_API_KEY", "moonshot-platform-key")
	t.Setenv("MOONSHOT_BASE_URL", "")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)},
	})
	env, _, err := forwardableProviderEnv(ctx, "moonshot/kimi-k2")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if env["MOONSHOT_API_KEY"] != "moonshot-platform-key" {
		t.Fatalf("MOONSHOT_API_KEY = %q — the moonshot node lost its only credential to the forfait crossing", env["MOONSHOT_API_KEY"])
	}
	// The container rebuilds its registry from what crossed: ask it.
	t.Setenv("MOONSHOT_API_KEY", env["MOONSHOT_API_KEY"])
	if _, err := NewRegistry().Resolve("moonshot/kimi-k2"); err != nil {
		t.Errorf("in-container resolve of a moonshot node: %v", err)
	}
}

// A PINNED key (secrets.RunBundle.PinnedAPIKeys) crosses the sandbox seam
// only for the node that NAMES its provider in its model spec — that spec is
// the pin on this backend. Both directions on one bench: the moonshot node
// gets it, and an `anthropic/…` node in the same run does not, or a
// credential provisioned for one route would sit in the environment of all
// of them.
func TestForwardableProviderEnv_APinnedKeyCrossesOnlyForTheNodeThatNamesIt(t *testing.T) {
	for _, k := range []string{"MOONSHOT_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ZAI_API_KEY"} {
		t.Setenv(k, "")
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderMoonshot: "platform-moonshot"},
	})

	pinnedNode, _, err := forwardableProviderEnv(ctx, "moonshot/kimi-k2")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if pinnedNode["MOONSHOT_API_KEY"] != "platform-moonshot" {
		t.Errorf("MOONSHOT_API_KEY = %q on a moonshot node — the key provisioned for this pin does not reach the container", pinnedNode["MOONSHOT_API_KEY"])
	}
	// The container rebuilds its registry from what crossed: ask it.
	t.Setenv("MOONSHOT_API_KEY", pinnedNode["MOONSHOT_API_KEY"])
	if _, rerr := NewRegistry().Resolve("moonshot/kimi-k2"); rerr != nil {
		t.Errorf("in-container resolve of the pinned moonshot node: %v", rerr)
	}
	t.Setenv("MOONSHOT_API_KEY", "")

	other, _, err := forwardableProviderEnv(ctx, "anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if got, present := other["MOONSHOT_API_KEY"]; present {
		t.Errorf("MOONSHOT_API_KEY = %q crossed for an anthropic node — a pinned key must not reach the routes that did not name it", got)
	}
}

// A pinned key is not the run's own instrument, so it must not make the
// forfait crossing believe the tenant brought a key on this wire: that would
// leave the tenant's sandboxed `anthropic/…` nodes to the platform's ambient
// key, which is #736's failure.
func TestHeldAnthropicWireAPIKey_IgnoresAPinnedKey(t *testing.T) {
	creds := secrets.Credentials{
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "platform-zai"},
	}
	if heldAnthropicWireAPIKey(creds) {
		t.Error("heldAnthropicWireAPIKey counted a pinned key — the run's forfait would stay out of its own anthropic nodes")
	}
}

// The forfait crossing is a PER-NODE decision. A key pinned for THIS node's
// own route was just injected by the pinned-key block; applying the forfait
// deleted it right back — the container spent the forfait while the
// in-process path spent the pin, and an expired forfait refused a node whose
// key was good. A pin for ANOTHER route still must not keep the forfait out
// of an unpinned node (#736, TestHeldAnthropicWireAPIKey_IgnoresAPinnedKey).
func TestForwardableProviderEnv_ForfaitCrossingKeepsThisNodesPinnedAnthropicKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-PLATFORM")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ZAI_API_KEY", "")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		PinnedAPIKeys:        map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-PINNED"},
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDirForTest(t)},
	})
	env := envFor(t, ctx)

	if env["ANTHROPIC_API_KEY"] != "sk-ant-PINNED" {
		t.Fatalf("ANTHROPIC_API_KEY = %q — the forfait crossing deleted the key this node's own pin injected", env["ANTHROPIC_API_KEY"])
	}
	if _, present := env["CLAUDE_CONFIG_DIR"]; present {
		t.Error("CLAUDE_CONFIG_DIR set although this node holds its own pinned key — the sandbox would spend the forfait while the in-process path spends the pin")
	}
}

// The workspace-command ceiling crosses with the task (#1716). `command` is
// PlacementSandbox, so the registration that EXECUTES in a sandboxed run is
// the container's, and the container's environment carries no dial: read
// only on the host, an operator who tightens the bound for a checkout under
// review would keep 256 KiB on every sandboxed run — philosophy #1's
// "never silently replace an operator's explicit choice".
//
// Mutants: drop the assignment from runnerIOTask → the forwarding readings
// redden; have WithSlashCommandCeiling read the dial instead of its
// argument → the last reading reddens, which is the devcontainer case.
func TestExecuteViaSandboxRunner_SendsTheWorkspaceCommandCeiling(t *testing.T) {
	work := t.TempDir()

	// The host's reading of the dial, in the registration's vocabulary.
	for _, tc := range []struct {
		dial string
		want int
	}{
		// A tightened bound, as set.
		{"4096", 4096},
		// The dial's documented "0" REMOVES the ceiling, which the
		// registration spells negative: a 0 on the wire would mean "the
		// built-in default" to the runner — the operator's choice replaced.
		{"0", -1},
		// No dial: the host's built-in default, NAMED on the wire rather
		// than left to whatever the container's own build defaults to.
		{"", defaultSlashCommandMaxBytes},
	} {
		t.Setenv(SlashCommandMaxBytesEnv, tc.dial)
		if got := SlashCommandMaxBytesForRegistry(); got != tc.want {
			t.Errorf("dial %q resolves to %d for a registration, want %d", tc.dial, got, tc.want)
		}
	}

	// The crossing carries the ceiling the backend was BUILT with, never one
	// read where the crossing is dispatched: an in-container runner relaying
	// a nested crossing reads its own environment — a repository's
	// devcontainer — which must not widen the operator's bound.
	t.Setenv(SlashCommandMaxBytesEnv, "999999")
	built := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{}, WithSlashCommandCeiling(4096))
	if got := dispatchCapturingTheTask(t, built, work).SlashCommandMaxBytes; got != 4096 {
		t.Errorf("the ceiling on the wire = %d, the one the backend was built with = 4096 (this process's env says 999999)", got)
	}
	// A launcher that names none sends 0: the runner keeps the built-in
	// default, as it did before the field existed.
	bare := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	if got := dispatchCapturingTheTask(t, bare, work).SlashCommandMaxBytes; got != 0 {
		t.Errorf("the ceiling on the wire = %d with no ceiling configured, want 0 — the runner's built-in default", got)
	}
}
