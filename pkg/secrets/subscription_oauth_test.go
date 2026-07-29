package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// forfaitOnlyCtx is a run scope whose only Anthropic credential is a
// materialised Claude Code OAuth blob — no metered API key.
func forfaitOnlyCtx() context.Context {
	return WithCredentials(context.Background(), Credentials{
		APIKeys: map[Provider]string{},
		OAuthCredentialFiles: map[string]string{
			string(OAuthKindClaudeCode): "/tmp/iter-oauth-fake",
		},
	})
}

// TestSubscriptionOAuthOnly covers the detection every caller keys on.
func TestSubscriptionOAuthOnly(t *testing.T) {
	t.Run("subscription token is the sole credential", func(t *testing.T) {
		if !SubscriptionOAuthOnly(forfaitOnlyCtx(), ProviderAnthropic, OAuthKindClaudeCode) {
			t.Fatal("expected true: the forfait is the only credential available")
		}
	})

	// BYOK wins: an explicit metered key means the operator is not spending
	// their subscription's extra-usage balance, so there is nothing to warn
	// about.
	t.Run("api key takes priority", func(t *testing.T) {
		ctx := WithCredentials(context.Background(), Credentials{
			APIKeys: map[Provider]string{ProviderAnthropic: "sk-ant-api-key"},
			OAuthCredentialFiles: map[string]string{
				string(OAuthKindClaudeCode): "/tmp/iter-oauth-fake",
			},
		})
		if SubscriptionOAuthOnly(ctx, ProviderAnthropic, OAuthKindClaudeCode) {
			t.Fatal("expected false when a metered API key is available")
		}
	})

	// No credentials in ctx = the env-fallback path; the provider library
	// surfaces any missing-key error itself.
	t.Run("no credentials scope", func(t *testing.T) {
		if SubscriptionOAuthOnly(context.Background(), ProviderAnthropic, OAuthKindClaudeCode) {
			t.Fatal("expected false outside a per-run credential scope")
		}
	})

	t.Run("a different oauth kind does not count", func(t *testing.T) {
		ctx := WithCredentials(context.Background(), Credentials{
			OAuthCredentialFiles: map[string]string{
				string(OAuthKindCodex): "/tmp/iter-oauth-codex",
			},
		})
		if SubscriptionOAuthOnly(ctx, ProviderAnthropic, OAuthKindClaudeCode) {
			t.Fatal("a codex OAuth blob must not register as an Anthropic subscription")
		}
	})
}

// TestGuardSubscriptionOAuth pins the contract that replaced the old blanket
// refusal.
//
// Anthropic ACCEPTS a subscription token from a third-party app and bills it
// against a separate extra-usage balance instead of the plan's limits, so
// consuming it is a billing choice rather than a policy breach. The guard
// therefore permits by default and refuses only on an explicit opt-out, which
// exists for shared and cloud deployments where spending an operator's
// extra-usage balance is a decision taken on everyone's behalf. See ADR-085.
func TestGuardSubscriptionOAuth(t *testing.T) {
	t.Run("permitted by default", func(t *testing.T) {
		if err := GuardSubscriptionOAuth(forfaitOnlyCtx(), ProviderAnthropic, OAuthKindClaudeCode); err != nil {
			t.Fatalf("expected nil by default, got %v", err)
		}
	})

	t.Run("opt-out refuses", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		err := GuardSubscriptionOAuth(forfaitOnlyCtx(), ProviderAnthropic, OAuthKindClaudeCode)
		if !errors.Is(err, ErrSubscriptionOAuthForbidden) {
			t.Fatalf("expected ErrSubscriptionOAuthForbidden, got %v", err)
		}
	})

	// The opt-out is about the subscription, not about Anthropic: a metered
	// key must keep working with it set.
	t.Run("opt-out does not block a metered key", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		ctx := WithCredentials(context.Background(), Credentials{
			APIKeys: map[Provider]string{ProviderAnthropic: "sk-ant-api-key"},
			OAuthCredentialFiles: map[string]string{
				string(OAuthKindClaudeCode): "/tmp/iter-oauth-fake",
			},
		})
		if err := GuardSubscriptionOAuth(ctx, ProviderAnthropic, OAuthKindClaudeCode); err != nil {
			t.Fatalf("expected nil with a metered key, got %v", err)
		}
	})

	t.Run("opt-out is off unless set to 1", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "true")
		if ForbidSubscriptionOAuth() {
			t.Error(`only the exact value "1" opts out — a truthy-looking string must not`)
		}
	})
}

// The notice is what the operator actually reads, so it must name the
// surprising fact (a different balance is being spent) and the way out.
func TestSubscriptionOAuthNotice(t *testing.T) {
	notice := SubscriptionOAuthNotice(ProviderAnthropic)
	for _, want := range []string{"anthropic", "EXTRA USAGE", "ITERION_FORBID_SUBSCRIPTION_OAUTH", "API key"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice lacks %q, so the operator cannot act on it: %s", want, notice)
		}
	}
}
