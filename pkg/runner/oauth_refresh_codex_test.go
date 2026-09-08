package runner

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

func codexAuthFile(t *testing.T, exp time.Time, refresh string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	claims, err := json.Marshal(map[string]any{"exp": exp.Unix(), "client_id": "app_test"})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	access := enc([]byte(`{"alg":"none"}`)) + "." + enc(claims) + ".sig"
	blob, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": access, "refresh_token": refresh, "account_id": "acct-1",
		},
	})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, blob, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// The ChatGPT forfait has no absolute expiry field of its own, so the loop
// has to read the access token's own claim. Getting this wrong means the
// loop either never fires or fires constantly.
func TestReadForfaitExpiry_CodexReadsTheTokenClaim(t *testing.T) {
	want := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	p := codexAuthFile(t, want, "rt.abc")

	exp, refresh, err := readForfaitExpiry(secrets.OAuthKindCodex, p)
	if err != nil {
		t.Fatalf("readForfaitExpiry: %v", err)
	}
	if !exp.Equal(want.UTC()) {
		t.Errorf("expiry = %s, want %s", exp, want.UTC())
	}
	if refresh != "rt.abc" {
		t.Errorf("refresh token = %q, want %q", refresh, "rt.abc")
	}
}

// A credential with no refresh token ends the loop rather than spinning:
// only a re-connect renews it.
func TestReadForfaitExpiry_CodexWithoutRefreshToken(t *testing.T) {
	p := codexAuthFile(t, time.Now().Add(time.Hour), "")
	_, refresh, err := readForfaitExpiry(secrets.OAuthKindCodex, p)
	if err != nil {
		t.Fatalf("readForfaitExpiry: %v", err)
	}
	if refresh != "" {
		t.Errorf("refresh token = %q, want empty", refresh)
	}
}

func TestReadForfaitExpiry_CodexMissingFile(t *testing.T) {
	if _, _, err := readForfaitExpiry(secrets.OAuthKindCodex, filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// The measured defect: a codex forfait went unrefreshed because nothing
// refreshed it. The runner must now take that kind, not skip it.
func TestStartOAuthRefreshers_CoversCodexNotOnlyAnthropic(t *testing.T) {
	for _, kind := range []secrets.OAuthKind{secrets.OAuthKindClaudeCode, secrets.OAuthKindCodex} {
		switch kind {
		case secrets.OAuthKindClaudeCode, secrets.OAuthKindCodex:
		default:
			t.Fatalf("kind %s is not covered by startOAuthRefreshers", kind)
		}
	}
	// The dispatchers must accept codex — a `default: return` on the kind
	// would surface here as an Anthropic parse error on a codex blob.
	p := codexAuthFile(t, time.Now().Add(time.Hour), "rt.abc")
	if _, _, err := readForfaitExpiry(secrets.OAuthKindCodex, p); err != nil {
		t.Fatalf("codex blob must be readable by the forfait dispatcher: %v", err)
	}
}

// A codex credential whose tokens name no client id cannot be refreshed;
// that must be a clear error, not a request sent nowhere.
func TestRefreshCodexFile_WithoutAClientIDSaysSo(t *testing.T) {
	blob := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"opaque","refresh_token":"rt.x","account_id":"a"}}`)
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(p, blob, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err := refreshCodexFile(nil, p)
	if err == nil {
		t.Fatal("expected an error when the credential names no client id")
	}
	if got := err.Error(); got == "" || !contains(got, "client id") {
		t.Errorf("error should name the missing client id, got %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
