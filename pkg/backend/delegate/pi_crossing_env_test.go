package delegate

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// WITNESSES (ADR-162 decision 7, U5): the fork lane's pi container env. The
// launcher's credentials never reach an untrusted container — not emptied,
// not carried at all — and the run's own keys cross as Layer 2 placeholders
// the egress proxy substitutes toward the credential's host. Each subtest
// drives the REAL env builders (piSandboxEnv, piRouteEnv) with a trusted
// control that keeps today's crossing.
//
// Mutants: drop `!untrusted` from the ambient loop guard → the first row;
// drop the piEnvKeys placeholder sweep → the second; drop the
// `!task.WorkspaceTrust.Trusted()` branch in piRouteEnv → the third.
func TestPi_ForkLaneCrossingEnv(t *testing.T) {
	// Hermetic: piSandboxEnv forwards the HOST's credential names, so pin
	// the launcher's own money to a value nothing real can be.
	t.Setenv("OPENAI_API_KEY", "sk-pod-pi-fork-witness")
	t.Setenv("ZAI_API_KEY", "zai-pod-pi-fork-witness")

	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "sk-ant-pi-run-key-witness"},
	})

	t.Run("the launcher's env never reaches the fork container", func(t *testing.T) {
		fork := piSandboxEnv(ctx, Task{WorkspaceTrust: store.RunTrustFork})
		if v, present := fork["OPENAI_API_KEY"]; present {
			t.Errorf("fork: OPENAI_API_KEY = %q crossed — the launcher's key is the deployment's money, not the run's", v)
		}
		if v, present := fork["ZAI_API_KEY"]; present {
			t.Errorf("fork: ZAI_API_KEY = %q crossed", v)
		}
		trusted := piSandboxEnv(ctx, Task{})
		if got := trusted["OPENAI_API_KEY"]; got != "sk-pod-pi-fork-witness" {
			t.Errorf("trusted control: OPENAI_API_KEY = %q, want the pod's — the trusted crossing is today's", got)
		}
	})

	t.Run("the run's own key crosses as its placeholder", func(t *testing.T) {
		fork := piSandboxEnv(ctx, Task{WorkspaceTrust: store.RunTrustFork})
		key := "sk-ant-pi-run-key-witness"
		if got := fork["ANTHROPIC_API_KEY"]; got != secrets.CredentialPlaceholder(key) {
			t.Errorf("fork: ANTHROPIC_API_KEY = %q, want the run key's Layer 2 placeholder", got)
		}
		// piBindKey clears the channels that would outrank the bound key:
		// under fork the explicit empties still cross (they carry no value).
		for _, name := range piAnthropicEnvChannels {
			if name != "ANTHROPIC_API_KEY" && fork[name] != "" {
				t.Errorf("fork: %s = %q, want the explicit empty", name, fork[name])
			}
		}
		trusted := piSandboxEnv(ctx, Task{})
		if got := trusted["ANTHROPIC_API_KEY"]; got != key {
			t.Errorf("trusted control: ANTHROPIC_API_KEY = %q, want the raw key", got)
		}
	})

	t.Run("a pin sealed for the node's route crosses as its placeholder", func(t *testing.T) {
		pinCtx := secrets.WithCredentials(context.Background(), secrets.Credentials{
			PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "zai-pi-pinned-witness"},
		})
		fork := piRouteEnv(pinCtx, Task{Model: "glm-5.3", ProviderHint: "zai", WorkspaceTrust: store.RunTrustFork})
		if got := fork["ZAI_API_KEY"]; got != secrets.CredentialPlaceholder("zai-pi-pinned-witness") {
			t.Errorf("fork: ZAI_API_KEY = %q, want the pin's placeholder — the raw pin never crosses", got)
		}
		// The pin is NOT the pod's key the launcher exported above.
		trusted := piRouteEnv(pinCtx, Task{Model: "glm-5.3", ProviderHint: "zai"})
		if got := trusted["ZAI_API_KEY"]; got != "zai-pi-pinned-witness" {
			t.Errorf("trusted control: ZAI_API_KEY = %q, want the raw pin — the trusted crossing is today's", got)
		}
	})
}
