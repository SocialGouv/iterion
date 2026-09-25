package delegate

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// ctxWithPinnedCreds wires a context whose bundle carries `pinned` keys a
// shared tier funded because a route NAMES their provider, beside the run's
// own `own` keys.
func ctxWithPinnedCreds(t *testing.T, own, pinned map[secrets.Provider]string, oauthDirs map[string]string) context.Context {
	t.Helper()
	return secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys:              own,
		PinnedAPIKeys:        pinned,
		OAuthCredentialFiles: oauthDirs,
	})
}

// A node PINNED to a facade provider spends the key a shared tier funded for
// that pin — that is the whole point of funding it.
func TestFacadeHint_SpendsAPinnedKey(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithPinnedCreds(t, nil, map[secrets.Provider]string{secrets.ProviderMoonshot: "platform-moonshot"}, nil)

	got := anthropicCredEnvForCLI(ctx, "moonshot", false)
	if got["ANTHROPIC_AUTH_TOKEN"] != "platform-moonshot" {
		t.Fatalf("ANTHROPIC_AUTH_TOKEN = %q, want the pinned key — the node the key was provisioned for cannot spend it", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if got["ANTHROPIC_BASE_URL"] != secrets.MoonshotDefaultBaseURL {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want Moonshot's endpoint", got["ANTHROPIC_BASE_URL"])
	}
	if err := facadeHintRefusal("moonshot", got); err != nil {
		t.Errorf("a node funded by a pinned key was refused: %v", err)
	}
}

// …and the run's OWN key outranks it: a tenant's instrument is not displaced
// by the deployment's.
func TestFacadeHint_TenantKeyOutranksThePinnedOne(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithPinnedCreds(t,
		map[secrets.Provider]string{secrets.ProviderMoonshot: "tenant-moonshot"},
		map[secrets.Provider]string{secrets.ProviderMoonshot: "platform-moonshot"}, nil)

	if got := anthropicCredEnvForCLI(ctx, "moonshot", false)["ANTHROPIC_AUTH_TOKEN"]; got != "tenant-moonshot" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want the tenant's own key", got)
	}
}

// THE property the separate channel exists for: an UNPINNED node must not be
// able to reach a pinned key. The anthropic wire ranks the facade slots
// FIRST, so a moonshot key visible to the default precedence would reroute
// every unpinned node of the run onto Kimi — past the tenant's own forfait,
// on the deployment's account.
//
// Both halves on one bench: the unpinned node keeps the forfait, and the
// pinned node still gets the key. A test that only asserted the first half
// would pass on a build that dropped pinned keys altogether.
func TestDefaultPrecedence_CannotSeeAPinnedKey(t *testing.T) {
	resetClaudeCredEnv(t)
	forfait := t.TempDir()
	ctx := ctxWithPinnedCreds(t, nil,
		map[secrets.Provider]string{secrets.ProviderMoonshot: "platform-moonshot"},
		map[string]string{string(secrets.OAuthKindClaudeCode): forfait})

	unpinned := anthropicCredEnvForCLI(ctx, "", false)
	if got := unpinned["ANTHROPIC_AUTH_TOKEN"]; got != "" {
		t.Errorf("an unpinned node was served %q — a pinned key must be invisible to the default precedence", got)
	}
	if got := unpinned["CLAUDE_CONFIG_DIR"]; got != forfait {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want the run's forfait %q — the unpinned work must keep the credential it had", got, forfait)
	}
	if fp := providerFingerprint(unpinned); fp != "anthropic-oauth" {
		t.Errorf("providerFingerprint = %q, want anthropic-oauth", fp)
	}
	if got := anthropicCredEnvForCLI(ctx, "moonshot", false)["ANTHROPIC_AUTH_TOKEN"]; got != "platform-moonshot" {
		t.Fatalf("the PINNED node got %q — the bench cannot tell the two routes apart, so its first half proves nothing", got)
	}
}

// An `anthropic` pin reads the pinned channel too: same licence, same class.
// Without it, a node pinned `provider: anthropic` beside a tenant forfait
// would be served the forfait — a different instrument than the one the
// deployment provisioned for the pin.
func TestAnthropicHint_SpendsAPinnedKey(t *testing.T) {
	resetClaudeCredEnv(t)
	ctx := ctxWithPinnedCreds(t, nil, map[secrets.Provider]string{secrets.ProviderAnthropic: "platform-anthropic"}, nil)
	if got := anthropicCredEnvForCLI(ctx, "anthropic", false)["ANTHROPIC_API_KEY"]; got != "platform-anthropic" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the pinned key", got)
	}
}

// A pin with NO key anywhere is still refused by name — the pinned channel
// adds a funding source, it does not soften the refusal.
func TestFacadeHint_NoPinnedKeyStillRefuses(t *testing.T) {
	resetClaudeCredEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ambient-anthropic")
	ctx := ctxWithPinnedCreds(t, nil, map[secrets.Provider]string{secrets.ProviderZAI: "platform-zai"}, nil)

	err := facadeHintRefusal("moonshot", anthropicCredEnvForCLI(ctx, "moonshot", false))
	var refusal *ErrNoFacadeCredential
	if !errors.As(err, &refusal) {
		t.Fatalf("a moonshot node funded by nobody (the pinned key is z.ai's) must be refused by name, got %v", err)
	}
}
