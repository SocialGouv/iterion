package model

import (
	"context"
	"testing"

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
		env := forwardableProviderEnv(context.Background())
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
		env := forwardableProviderEnv(ctx)
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
		env := forwardableProviderEnv(ctx)
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
		if got := forwardableProviderEnv(ctx)["OPENAI_API_KEY"]; got != "platform-key" {
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
	env := forwardableProviderEnv(context.Background())
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
	env := forwardableProviderEnv(context.Background())
	if env["ITERION_CODEX_VERSION"] != "0.144.6" {
		t.Errorf("ITERION_CODEX_VERSION = %q, want the host-probed version forwarded when no override is set", env["ITERION_CODEX_VERSION"])
	}

	// No codex on the host either: forward nothing, never an empty pair.
	hostCodexVersion = func() string { return "" }
	env = forwardableProviderEnv(context.Background())
	if v, ok := env["ITERION_CODEX_VERSION"]; ok {
		t.Errorf("ITERION_CODEX_VERSION = %q set with nothing to forward — an empty value must not cross", v)
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
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): "/tmp/run-forfait"},
	})
	env := forwardableProviderEnv(ctx)

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
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): "/tmp/run-forfait"},
	})
	env := forwardableProviderEnv(ctx)

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
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): "/tmp/run-forfait"},
	})
	if _, present := forwardableProviderEnv(ctx)["CLAUDE_CONFIG_DIR"]; present {
		t.Error("forfait pointed into the container while the wire is redirected at a gateway")
	}
}
