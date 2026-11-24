package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// WITNESSES (ADR-162 decision 7, U5): the fork lane's claude_code spawn env.
// The one resolver every spawn composes (selectedAnthropicCredEnvForCLI)
// crosses the run's keys as Layer 2 placeholders — never the value — and
// crossingRefusal refuses the forfait's file mount an untrusted container
// cannot hold. Trusted controls keep today's raw crossing.
//
// Mutants: `cross := sandboxed && !credroute.CarriedRunTrust(ctx).Trusted()`
// losing the trust term → the first two rows; crossedKey returning the key →
// with it; crossingRefusal's trust guard → the third.
func TestClaudeCode_ForkLaneCrossingEnv(t *testing.T) {
	key := "sk-ant-cc-run-key-witness"
	creds := secrets.Credentials{APIKeys: map[secrets.Provider]string{
		secrets.ProviderAnthropic: key,
		secrets.ProviderZAI:       "zai-cc-run-key-witness",
	}}
	forkCtx := credroute.WithRunTrust(secrets.WithCredentials(context.Background(), creds), store.RunTrustFork)
	trustedCtx := secrets.WithCredentials(context.Background(), creds)

	t.Run("the run's anthropic key crosses as its placeholder", func(t *testing.T) {
		env, inherit := selectedAnthropicCredEnvForCLI(forkCtx, "anthropic", true)
		if inherit {
			t.Fatal("fork: the spawn would inherit the ambient env — the crossing must carry its own channel")
		}
		if got := env["ANTHROPIC_API_KEY"]; got != secrets.CredentialPlaceholder(key) {
			t.Errorf("fork: ANTHROPIC_API_KEY = %q, want the run key's Layer 2 placeholder", got)
		}
		// Trusted control: the raw key is today's crossing.
		env, _ = selectedAnthropicCredEnvForCLI(trustedCtx, "anthropic", true)
		if got := env["ANTHROPIC_API_KEY"]; got != key {
			t.Errorf("trusted control: ANTHROPIC_API_KEY = %q, want the raw key", got)
		}
		// An untrusted spawn on the HOST keeps the value: the crossing is
		// the sandbox's floor, not the lane's.
		env, _ = selectedAnthropicCredEnvForCLI(forkCtx, "anthropic", false)
		if got := env["ANTHROPIC_API_KEY"]; got != key {
			t.Errorf("fork on the host: ANTHROPIC_API_KEY = %q, want the raw key — no sandbox, no crossing", got)
		}
	})

	t.Run("a facade key crosses as its placeholder bearer on its own endpoint", func(t *testing.T) {
		env, _ := selectedAnthropicCredEnvForCLI(forkCtx, "", true)
		zai := "zai-cc-run-key-witness"
		// The facade slot wins the wire's slot order, and its channel is
		// the bearer: the value is the placeholder, the endpoint the
		// vendor's own (the pod's routing env is not the run's).
		if got := env["ANTHROPIC_AUTH_TOKEN"]; got != secrets.CredentialPlaceholder(zai) {
			t.Errorf("fork: ANTHROPIC_AUTH_TOKEN = %q, want the facade key's placeholder", got)
		}
		if got := env["ANTHROPIC_BASE_URL"]; got != secrets.ZAIDefaultBaseURL {
			t.Errorf("fork: ANTHROPIC_BASE_URL = %q, want %q — under the crossing the vendor's own endpoint serves the key", got, secrets.ZAIDefaultBaseURL)
		}
		env, _ = selectedAnthropicCredEnvForCLI(trustedCtx, "", true)
		if got := env["ANTHROPIC_AUTH_TOKEN"]; got != zai {
			t.Errorf("trusted control: ANTHROPIC_AUTH_TOKEN = %q, want the raw key", got)
		}
	})

	t.Run("the forfait's file mount is refused, not crossed", func(t *testing.T) {
		forfait := map[string]string{string(secrets.OAuthKindClaudeCode): t.TempDir()}
		forfaitCtx := credroute.WithRunTrust(secrets.WithCredentials(context.Background(), secrets.Credentials{
			OAuthCredentialFiles: forfait,
		}), store.RunTrustFork)
		task := Task{Sandbox: k8sLikeRun{}}
		env, _ := selectedAnthropicCredEnvForCLI(forfaitCtx, "", true)
		err := crossingRefusal(forfaitCtx, task, env)
		if err == nil {
			t.Fatal("fork: the forfait's config dir crossed an untrusted container with nothing said")
		}
		var refused *credroute.ErrCrossingRefused
		if !asCrossingRefusal(err, &refused) || !strings.Contains(refused.Channel, "Claude forfait") {
			t.Fatalf("fork: refusal = %v, want a typed crossing refusal naming the forfait", err)
		}
		// Trusted control: the same forfait mounts as today.
		env, _ = selectedAnthropicCredEnvForCLI(secrets.WithCredentials(context.Background(), secrets.Credentials{OAuthCredentialFiles: forfait}), "", true)
		if err := crossingRefusal(secrets.WithCredentials(context.Background(), secrets.Credentials{OAuthCredentialFiles: forfait}), task, env); err != nil {
			t.Fatalf("trusted control: the forfait is today's sandboxed crossing — %v", err)
		}
	})
}

// asCrossingRefusal is errors.As for the typed refusal, local so the witness
// reads as what it asserts.
func asCrossingRefusal(err error, target **credroute.ErrCrossingRefused) bool {
	for err != nil {
		if r, ok := err.(*credroute.ErrCrossingRefused); ok {
			*target = r
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
