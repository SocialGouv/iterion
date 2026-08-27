package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestCodexCredentialsView_IsChatGPTMode(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{
			name: "chatgpt mode with full token + account id",
			body: map[string]any{
				"auth_mode": "chatgpt",
				"tokens": map[string]any{
					"access_token": "tok-1",
					"account_id":   "acct-1",
				},
			},
			want: true,
		},
		{
			name: "chatgpt mode but missing account_id",
			body: map[string]any{
				"auth_mode": "chatgpt",
				"tokens":    map[string]any{"access_token": "tok-1"},
			},
			want: false,
		},
		{
			name: "apikey mode",
			body: map[string]any{
				"auth_mode":      "apikey",
				"OPENAI_API_KEY": "sk-test",
			},
			want: false,
		},
		{
			name: "no auth_mode at all",
			body: map[string]any{
				"tokens": map[string]any{"access_token": "tok-1", "account_id": "acct-1"},
			},
			want: false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			payload, _ := json.Marshal(tt.body)
			v, err := ParseCodexView(payload)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := v.IsChatGPTMode(); got != tt.want {
				t.Errorf("IsChatGPTMode() = %v, want %v (view=%+v)", got, tt.want, v)
			}
		})
	}
}

func TestLoadCodexCredentialsFromDisk_HonoursCODEX_HOME(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"tok-a","refresh_token":"rt-a","account_id":"acct-a"}}`)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), payload, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("CODEX_HOME", dir)

	v, err := LoadCodexCredentialsFromDisk()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !v.IsChatGPTMode() || v.Tokens.AccessToken != "tok-a" || v.Tokens.AccountID != "acct-a" {
		t.Fatalf("view mismatch: %+v", v)
	}
}

func TestLoadCodexCredentialsFromDisk_MissingFile(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir()) // empty dir, no auth.json
	if _, err := LoadCodexCredentialsFromDisk(); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestOAuthKindValid(t *testing.T) {
	for _, k := range []OAuthKind{OAuthKindClaudeCode, OAuthKindCodex} {
		if !k.Valid() {
			t.Errorf("OAuthKind(%q).Valid() = false, want true", k)
		}
	}
	for _, k := range []OAuthKind{"", "openai", "claude", "Codex"} {
		if k.Valid() {
			t.Errorf("OAuthKind(%q).Valid() = true, want false", k)
		}
	}
}

func TestCodexAuthJSONPath(t *testing.T) {
	// CODEX_HOME override wins.
	t.Run("CODEX_HOME override", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "/custom/codex")
		want := filepath.Join("/custom/codex", "auth.json")
		if got := CodexAuthJSONPath(); got != want {
			t.Fatalf("CodexAuthJSONPath() = %q, want %q", got, want)
		}
	})

	// Falls back to ~/.codex/auth.json when CODEX_HOME is unset.
	t.Run("HOME fallback", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "")
		home := t.TempDir()
		t.Setenv("HOME", home)
		want := filepath.Join(home, ".codex", "auth.json")
		if got := CodexAuthJSONPath(); got != want {
			t.Fatalf("CodexAuthJSONPath() = %q, want %q", got, want)
		}
	})
}

func TestParseAnthropicView_Malformed(t *testing.T) {
	v, err := ParseAnthropicView([]byte("{not json"))
	if err == nil {
		t.Fatal("expected error for malformed credentials.json")
	}
	if v.ClaudeAIOauth.AccessToken != "" {
		t.Fatalf("expected zero view on parse error, got %+v", v)
	}
}

func TestParseCodexView_Malformed(t *testing.T) {
	v, err := ParseCodexView([]byte("{not json"))
	if err == nil {
		t.Fatal("expected error for malformed auth.json")
	}
	if v.Tokens.AccessToken != "" {
		t.Fatalf("expected zero view on parse error, got %+v", v)
	}
}

// The meter identity must be the SUBSCRIPTION, not the credentials blob.
// The blob is not one: the same account connected twice — org-level and
// personally, or re-pasted after a token looked broken — serialises
// differently every time, and a blob hash would open a second meter that
// starts empty and admits a run the first would have parked. Codex names
// its account; Anthropic does not, and that gap is pinned here too so it
// stays a documented limit rather than a silent one.
func TestSubscriptionFingerprint_IdentifiesTheAccountNotTheBlob(t *testing.T) {
	codex := func(token, account string) []byte {
		return []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"` + token +
			`","refresh_token":"r-` + token + `","account_id":"` + account + `"}}`)
	}

	// One account, two connections: different tokens, ONE meter.
	first := SubscriptionFingerprint(OAuthKindCodex, codex("tok-1", "acct-A"))
	second := SubscriptionFingerprint(OAuthKindCodex, codex("tok-2", "acct-A"))
	if first != second {
		t.Errorf("same codex account fingerprinted %q then %q — one subscription would get two meters and two ceilings", first, second)
	}
	if first == "" {
		t.Fatal("empty fingerprint: every payload must yield an identity")
	}

	// Two accounts must never share a meter — that is the whole point of
	// keying the ledger on the credential.
	if other := SubscriptionFingerprint(OAuthKindCodex, codex("tok-1", "acct-B")); other == first {
		t.Error("two distinct codex accounts share a fingerprint: one account's spend would park the other's runs")
	}

	// An account-derived identity must never collide with a blob-derived
	// one, hence the namespace in the hashed input.
	if raw := SubscriptionFingerprint(OAuthKindClaudeCode, codex("tok-1", "acct-A")); raw == first {
		t.Error("account-derived and blob-derived identities collide — the namespace is not applied")
	}

	// Fallbacks: a codex payload with no account_id, and Anthropic, which
	// carries no account identifier at all (see SubscriptionFingerprint's
	// KNOWN GAP). Both must still yield a stable per-blob identity rather
	// than an empty one, which would collapse every credential onto the
	// historical slot-shaped key.
	noAccount := []byte(`{"auth_mode":"apikey","tokens":{"access_token":"tok-9"}}`)
	if got := SubscriptionFingerprint(OAuthKindCodex, noAccount); got != fingerprintHex(string(noAccount)) {
		t.Errorf("codex without account_id = %q, want the blob fallback", got)
	}
	anth := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-1"}}`)
	if got := SubscriptionFingerprint(OAuthKindClaudeCode, anth); got != fingerprintHex(string(anth)) {
		t.Errorf("anthropic = %q, want the blob fallback", got)
	}
	if SubscriptionFingerprint(OAuthKindClaudeCode, anth) ==
		SubscriptionFingerprint(OAuthKindClaudeCode, []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-2"}}`)) {
		t.Error("two distinct anthropic credentials share a fingerprint")
	}
}
