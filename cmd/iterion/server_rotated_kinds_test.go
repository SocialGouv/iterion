package main

import (
	"slices"
	"testing"

	iterconfig "github.com/SocialGouv/iterion/pkg/config"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The kinds a run may follow are exactly the kinds the server's refresh
// worker rotates: its gate is no worker without a client id for either kind,
// claude_code only with its own, codex whenever the worker runs.
func TestRotatedOAuthKindsMirrorTheRefreshWorker(t *testing.T) {
	for _, tc := range []struct {
		anthropic, codex string
		want             []secrets.OAuthKind
	}{
		{"", "", nil},
		{"anthropic-id", "", []secrets.OAuthKind{secrets.OAuthKindCodex, secrets.OAuthKindClaudeCode}},
		{"", "codex-id", []secrets.OAuthKind{secrets.OAuthKindCodex}},
		{"anthropic-id", "codex-id", []secrets.OAuthKind{secrets.OAuthKindCodex, secrets.OAuthKindClaudeCode}},
	} {
		var cfg iterconfig.Config
		cfg.Auth.OAuthForfait.AnthropicClientID = tc.anthropic
		cfg.Auth.OAuthForfait.CodexClientID = tc.codex
		if got := rotatedOAuthKinds(cfg); !slices.Equal(got, tc.want) {
			t.Errorf("anthropic=%q codex=%q: rotated kinds %v, want %v", tc.anthropic, tc.codex, got, tc.want)
		}
	}
}
