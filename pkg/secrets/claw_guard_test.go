package secrets

import (
	"context"
	"errors"
	"testing"
)

// TestGuardThirdPartyOAuth_ClawCannotConsumeAnthropicForfait pins the
// CGU rule: when the only Anthropic credential available to an
// in-process LLM consumer (claw) is a stored Claude Code OAuth-forfait
// blob, the guard MUST refuse the call. See memory
// feedback_no_anthropic_oauth_in_third_party.md.
//
// This is the canonical test the cloud admin plan §verification calls
// out as bloquant. If it ever turns red, ship-blocker.
func TestGuardThirdPartyOAuth_ClawCannotConsumeAnthropicForfait(t *testing.T) {
	ctx := WithCredentials(context.Background(), Credentials{
		// No API key.
		APIKeys: map[Provider]string{},
		// But an OAuth-forfait dir is present (runner materialised
		// the credentials.json for the claude_code delegate).
		OAuthCredentialFiles: map[string]string{
			string(OAuthKindClaudeCode): "/tmp/iter-oauth-fake",
		},
	})
	if err := GuardThirdPartyOAuth(ctx, ProviderAnthropic, OAuthKindClaudeCode); !errors.Is(err, ErrOAuthForfaitInThirdParty) {
		t.Fatalf("expected ErrOAuthForfaitInThirdParty, got %v", err)
	}
}

// When an API key IS available, the guard must let the call proceed
// even if an OAuth dir is also present (BYOK takes priority).
func TestGuardThirdPartyOAuth_APIKeyOverridesOAuthForfait(t *testing.T) {
	ctx := WithCredentials(context.Background(), Credentials{
		APIKeys: map[Provider]string{
			ProviderAnthropic: "sk-ant-api-key",
		},
		OAuthCredentialFiles: map[string]string{
			string(OAuthKindClaudeCode): "/tmp/iter-oauth-fake",
		},
	})
	if err := GuardThirdPartyOAuth(ctx, ProviderAnthropic, OAuthKindClaudeCode); err != nil {
		t.Fatalf("expected nil (API key wins), got %v", err)
	}
}

// Without any credentials in ctx the guard is a no-op (env-fallback
// path). The Anthropic provider library itself surfaces the missing
// key error when it tries to make a request.
func TestGuardThirdPartyOAuth_NoCredentialsIsNoOp(t *testing.T) {
	if err := GuardThirdPartyOAuth(context.Background(), ProviderAnthropic, OAuthKindClaudeCode); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// When an OAuth blob is present for a different kind (codex), the
// guard for Anthropic should not fire.
func TestGuardThirdPartyOAuth_DifferentKindIsNoOp(t *testing.T) {
	ctx := WithCredentials(context.Background(), Credentials{
		OAuthCredentialFiles: map[string]string{
			string(OAuthKindCodex): "/tmp/iter-oauth-codex",
		},
	})
	if err := GuardThirdPartyOAuth(ctx, ProviderAnthropic, OAuthKindClaudeCode); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}
