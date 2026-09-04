package model

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// clientBearerForTest reads the OAuth bearer a resolved claw client will send.
// It is the only oracle that separates "a client was built" from "a client that
// can authenticate": the unauthenticated fall-through returns a non-nil client
// too.
func clientBearerForTest(c api.APIClient) string {
	cc, ok := c.(*api.Client)
	if !ok {
		return ""
	}
	return cc.OAuthToken
}

// writeClaudeCodeCreds drops a Claude Code .credentials.json into a fresh
// CLAUDE_CONFIG_DIR-shaped dir and returns it (the shape
// Credentials.OAuthDir("claude_code") yields on a runner pod).
func writeClaudeCodeCreds(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-test","refreshToken":"rt","expiresAt":9999999999999}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A run whose only anthropic credential is a materialised Claude Code OAuth
// forfait must resolve an AUTHENTICATED claw client. Before the ctx-forfait
// branch existed, ResolveWithContext fell through to the env factory, which on
// a runner pod finds no ANTHROPIC_* var and builds a client with no credential
// at all — every call then answers 401 "x-api-key header is required". That is
// exactly how Revi's pacer supervisor died in prod (issue #687) while its unit
// tests stayed green.
func TestResolveWithContext_AnthropicForfaitFromCtx(t *testing.T) {
	dir := writeClaudeCodeCreds(t)
	SetCredentialsLookup(func(ctx context.Context) (func(string) string, bool) {
		return func(string) string { return "" }, true
	})
	SetOAuthDirLookup(func(ctx context.Context) (func(string) string, bool) {
		return func(kind string) string {
			if kind == "claude_code" {
				return dir
			}
			return ""
		}, true
	})
	t.Cleanup(func() {
		SetCredentialsLookup(func(context.Context) (func(string) string, bool) { return nil, false })
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false })
	})
	// No ambient anthropic auth: the ctx forfait is the only possible signal.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")

	reg := NewRegistry()
	client, err := reg.ResolveWithContext(context.Background(), "anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatalf("ResolveWithContext: %v", err)
	}
	if client == nil {
		t.Fatal("expected a forfait-backed client from the ctx-resolved claude_code dir, got nil")
	}
	// The oracle is the bearer, not the absence of an error: the env factory
	// also returns a non-nil client — an unauthenticated one.
	if tok := clientBearerForTest(client); tok != "sk-ant-oat-test" {
		t.Fatalf("client carries bearer %q, want the forfait access token — an unauthenticated client is what produces the 401", tok)
	}
}

// The desktop twin: no ctx credentials at all, but this host has a Claude
// Code forfait on disk. The openai factory has read ~/.codex since day one;
// the anthropic one read env vars only, so the commonest desktop setup (a
// Claude subscription, no API key) resolved an unauthenticated client.
func TestResolve_AnthropicForfaitFromDisk(t *testing.T) {
	dir := writeClaudeCodeCreds(t)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")

	client, err := NewRegistry().Resolve("anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok := clientBearerForTest(client); tok != "sk-ant-oat-test" {
		t.Fatalf("client carries bearer %q, want the on-disk forfait token", tok)
	}
}

// An operator who redirected the wire chose that destination: a subscription
// bearer must not be sent there implicitly. An explicit ANTHROPIC_AUTH_TOKEN
// still travels — that one is the operator's own choice.
func TestResolve_AnthropicDiskForfaitNotSentToRedirectedWire(t *testing.T) {
	dir := writeClaudeCodeCreds(t)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example.test")

	client, err := NewRegistry().Resolve("anthropic/claude-haiku-4-5")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok := clientBearerForTest(client); tok != "" {
		t.Fatalf("forfait bearer %q leaked to a redirected base URL", tok)
	}
}

func TestAnthropicFromCtxForfait_DisabledAndAbsent(t *testing.T) {
	reg := NewRegistry()
	clear := func() {
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false })
	}

	t.Run("no oauth dir → not ok", func(t *testing.T) {
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
			return func(string) string { return "" }, true
		})
		t.Cleanup(clear)
		if _, ok, _ := reg.anthropicFromCtxForfait(context.Background(), "claude-haiku-4-5"); ok {
			t.Error("expected ok=false with no claude_code dir")
		}
	})

	t.Run("z.ai base URL → not ok (the token is not a forfait bearer there)", func(t *testing.T) {
		dir := writeClaudeCodeCreds(t)
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
			return func(string) string { return dir }, true
		})
		t.Cleanup(clear)
		t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic")
		if _, ok, _ := reg.anthropicFromCtxForfait(context.Background(), "claude-haiku-4-5"); ok {
			t.Error("expected ok=false against a z.ai facade base URL")
		}
	})

	t.Run("FORBID_SUBSCRIPTION_OAUTH=1 → refused, loudly", func(t *testing.T) {
		dir := writeClaudeCodeCreds(t)
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
			return func(string) string { return dir }, true
		})
		t.Cleanup(clear)
		t.Setenv("ANTHROPIC_BASE_URL", "")
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
		_, ok, err := reg.anthropicFromCtxForfait(context.Background(), "claude-haiku-4-5")
		if !ok || err == nil {
			t.Fatalf("expected an explicit refusal (ok=true, err!=nil), got ok=%v err=%v", ok, err)
		}
	})

	t.Run("malformed credentials file → not ok", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte("{nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
			return func(string) string { return dir }, true
		})
		t.Cleanup(clear)
		t.Setenv("ANTHROPIC_BASE_URL", "")
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
		if _, ok, _ := reg.anthropicFromCtxForfait(context.Background(), "claude-haiku-4-5"); ok {
			t.Error("expected ok=false on a malformed credentials.json")
		}
	})
}
