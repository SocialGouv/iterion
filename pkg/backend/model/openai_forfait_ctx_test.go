package model

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeCodexAuth drops a chatgpt-mode auth.json into a fresh CODEX_HOME-shaped
// dir and returns that dir (the shape Credentials.OAuthDir("codex") yields).
func writeCodexAuth(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	blob := `{"auth_mode":"chatgpt","tokens":{"access_token":"tok-abc","account_id":"acct-xyz"}}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveWithContext_OpenAICodexForfaitFromCtx(t *testing.T) {
	dir := writeCodexAuth(t)
	// A tenant with NO BYOK openai key, but a resolved codex forfait dir.
	SetCredentialsLookup(func(ctx context.Context) (func(string) string, bool) {
		return func(string) string { return "" }, true
	})
	SetOAuthDirLookup(func(ctx context.Context) (func(string) string, bool) {
		return func(kind string) string {
			if kind == "codex" {
				return dir
			}
			return ""
		}, true
	})
	t.Cleanup(func() {
		SetCredentialsLookup(func(context.Context) (func(string) string, bool) { return nil, false })
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false })
	})
	// Neutralise the disk/env knobs so the ctx forfait is the only signal.
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("ITERION_OPENAI_USE_OAUTH", "")

	reg := NewRegistry()
	client, err := reg.ResolveWithContext(context.Background(), "openai/gpt-5.4-mini")
	if err != nil {
		t.Fatalf("ResolveWithContext: %v", err)
	}
	if client == nil {
		t.Fatal("expected a ChatGPT-forfait client from the ctx-resolved codex dir, got nil")
	}
}

func TestOpenAIFromCtxForfait_DisabledAndAbsent(t *testing.T) {
	reg := NewRegistry()

	t.Run("no oauth dir → not ok", func(t *testing.T) {
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
			return func(string) string { return "" }, true
		})
		t.Cleanup(func() { SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false }) })
		t.Setenv("OPENAI_BASE_URL", "")
		t.Setenv("ITERION_OPENAI_USE_OAUTH", "")
		if _, ok, _ := reg.openAIFromCtxForfait(context.Background(), "gpt-5.4-mini"); ok {
			t.Error("expected ok=false with no codex dir")
		}
	})

	t.Run("USE_OAUTH=0 disables → not ok", func(t *testing.T) {
		dir := writeCodexAuth(t)
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
			return func(string) string { return dir }, true
		})
		t.Cleanup(func() { SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false }) })
		t.Setenv("OPENAI_BASE_URL", "")
		t.Setenv("ITERION_OPENAI_USE_OAUTH", "0")
		if _, ok, _ := reg.openAIFromCtxForfait(context.Background(), "gpt-5.4-mini"); ok {
			t.Error("expected ok=false when ITERION_OPENAI_USE_OAUTH=0")
		}
	})

	t.Run("OPENAI_BASE_URL set disables → not ok", func(t *testing.T) {
		dir := writeCodexAuth(t)
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
			return func(string) string { return dir }, true
		})
		t.Cleanup(func() { SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false }) })
		t.Setenv("ITERION_OPENAI_USE_OAUTH", "")
		t.Setenv("OPENAI_BASE_URL", "http://localhost:1234")
		if _, ok, _ := reg.openAIFromCtxForfait(context.Background(), "gpt-5.4-mini"); ok {
			t.Error("expected ok=false when OPENAI_BASE_URL is set")
		}
	})
}
