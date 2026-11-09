package delegate

import (
	"context"
	"os"
	"path/filepath"
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

// WITNESS (ADR-162 decision 7, U5): the ChatGPT forfait on a pi node. The
// bridge seeds it as an OAuth file plus its refresh flow — a channel the
// egress proxy cannot substitute — and the fork lane's seed steps aside
// (piCodexSeed), so the admission probe (PiSandboxCredential, the REAL
// predicate both the launch gate and this test read) must refuse it TYPED
// under the channel name claw's landing floor uses for the same forfait,
// never answer "found" and let the launch die credential-less mid-run
// (#1768). The forfait is DELIVERED to the run — the ctx copy the runner
// materialises — which piLoadCodexView reads for any trust; the host's
// ~/.codex has its own, separate refusal.
//
// Mutants: drop the `!trust.Trusted()` branch in the codex leg of
// PiSandboxCredential → the fork row reddens on its crossing assertion;
// widen piCodexSeed's gate back to StateInCheckout-only and the seed stays
// refused here only because the probe holds — see the seed's own witness in
// untrusted_pi_floor_test.go.
func TestPi_ForkLaneRefusesTheChatGPTForfaitTyped(t *testing.T) {
	// Hermetic: the ambient rows below read the launcher's env.
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("KIMI_API_KEY", "")

	// The forfait DELIVERED to the run: a real ChatGPT-mode auth.json, the
	// shape LoadCodexCredentialsFrom parses (access + account_id is what
	// IsChatGPTMode asks; expiry is judged at spend, not at load).
	forfait := t.TempDir()
	blob := `{"auth_mode":"chatgpt","tokens":{"access_token":"access-pi-run","refresh_token":"refresh-pi-run","account_id":"acct-pi-run","id_token":"id-pi-run"},"last_refresh":"2026-07-29T10:00:00Z"}`
	if err := os.WriteFile(filepath.Join(forfait, "auth.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	delivered := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindCodex): forfait},
	})

	t.Run("a fork is refused typed, under the forfait's channel", func(t *testing.T) {
		channel, ok, crossing := PiSandboxCredential(delivered, "openai-codex/gpt-5.6-sol", store.RunTrustFork)
		if ok || channel != "" {
			t.Fatalf("fork probe = (%q, %v) — the forfait was counted as funding; the launch admits and dies credential-less mid-run (#1768)", channel, ok)
		}
		if crossing == nil {
			t.Fatal("crossing = nil — an unstamped fork with a delivered forfait is a bare no-credential, not the named channel the operator must replace")
		}
		if crossing.Channel != "the run's ChatGPT forfait" {
			t.Errorf("channel = %q, want %q — one vocabulary with claw's landing floor for the same forfait", crossing.Channel, "the run's ChatGPT forfait")
		}
	})

	t.Run("the same forfait funds a trusted run", func(t *testing.T) {
		channel, ok, crossing := PiSandboxCredential(delivered, "openai-codex/gpt-5.6-sol", store.RunTrustDefault)
		if !ok || crossing != nil {
			t.Fatalf("trusted probe = (%q, %v, %v) — the bridge still funds a trusted run", channel, ok, crossing)
		}
		if channel != "the ChatGPT-forfait bridge (the codex credential seeded into the sandbox)" {
			t.Errorf("trusted channel = %q, want the bridge", channel)
		}
	})

	t.Run("a fork with no forfait at all is credential-less, not refused", func(t *testing.T) {
		_, ok, crossing := PiSandboxCredential(context.Background(), "openai-codex/gpt-5.6-sol", store.RunTrustFork)
		if ok || crossing != nil {
			t.Fatalf("no-forfait fork probe = (%v, %v) — the typed refusal belongs to a channel that EXISTS", ok, crossing)
		}
	})
}
