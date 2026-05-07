package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestOAuthSealAADBound(t *testing.T) {
	sealer := newSealer(t)
	payload := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-..."}}`)

	sealed, err := SealOAuthPayload(sealer, "alice", OAuthKindClaudeCode, payload)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := OpenOAuthPayload(sealer, "alice", OAuthKindClaudeCode, sealed)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("roundtrip: %v / %q", err, got)
	}
	// AAD pinning: switching the user must fail.
	if _, err := OpenOAuthPayload(sealer, "bob", OAuthKindClaudeCode, sealed); err == nil {
		t.Fatal("expected user mismatch failure")
	}
	// Switching the kind must fail too.
	if _, err := OpenOAuthPayload(sealer, "alice", OAuthKindCodex, sealed); err == nil {
		t.Fatal("expected kind mismatch failure")
	}
}

func TestMemoryOAuthStoreUpsertGetDelete(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryOAuthStore()
	rec := OAuthRecord{
		UserID:        "alice",
		Kind:          OAuthKindClaudeCode,
		SealedPayload: []byte("sealed"),
		CreatedAt:     time.Now(),
	}
	if err := store.Upsert(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := store.Get(ctx, "alice", OAuthKindClaudeCode)
	if err != nil || got.UserID != "alice" || got.Kind != OAuthKindClaudeCode {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := store.Get(ctx, "alice", OAuthKindCodex); !errors.Is(err, ErrOAuthNotFound) {
		t.Fatalf("expected ErrOAuthNotFound, got %v", err)
	}
	if err := store.Delete(ctx, "alice", OAuthKindClaudeCode); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, "alice", OAuthKindClaudeCode); !errors.Is(err, ErrOAuthNotFound) {
		t.Fatalf("expected ErrOAuthNotFound after delete")
	}
}

func TestMemoryOAuthStoreExpiringBefore(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryOAuthStore()
	now := time.Now()
	soon := now.Add(2 * time.Minute)
	later := now.Add(2 * time.Hour)

	if err := store.Upsert(ctx, OAuthRecord{UserID: "a", Kind: OAuthKindClaudeCode, AccessTokenExpiresAt: &soon}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, OAuthRecord{UserID: "a", Kind: OAuthKindCodex, AccessTokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ExpiringBefore(ctx, now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("expiring: %v", err)
	}
	if len(got) != 1 || got[0].Kind != OAuthKindClaudeCode {
		t.Fatalf("expected 1 expiring claude_code, got %+v", got)
	}
}

func TestParseAnthropicView(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "sk-ant-1",
			"refreshToken": "sk-rt-1",
			"expiresAt":    int64(1_700_000_000_000),
			"scopes":       []string{"user:inference"},
		},
	})
	v, err := ParseAnthropicView(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.ClaudeAIOauth.AccessToken != "sk-ant-1" || v.ClaudeAIOauth.RefreshToken != "sk-rt-1" {
		t.Fatalf("view mismatch: %+v", v)
	}
	if v.ClaudeAIOauth.ExpiresAt != 1_700_000_000_000 {
		t.Fatalf("expiresAt mismatch: %d", v.ClaudeAIOauth.ExpiresAt)
	}
}
