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
