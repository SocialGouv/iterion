package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeJWT builds an unsigned three-segment token carrying claims. The
// signature is never verified by the code under test — deriving a refresh
// endpoint from our own stored credential is not an authentication.
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(body) + ".sig"
}

func codexBlob(t *testing.T, access, id string) []byte {
	t.Helper()
	v := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token":  access,
			"refresh_token": "rt.test",
			"id_token":      id,
			"account_id":    "acct-1",
		},
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	return b
}

func TestCodexOAuthClientID_ReadFromTheCredential(t *testing.T) {
	cases := []struct {
		name   string
		access map[string]any
		id     map[string]any
		want   string
	}{
		{
			name:   "access token client_id wins",
			access: map[string]any{"client_id": "app_from_access"},
			id:     map[string]any{"aud": []any{"app_from_id"}},
			want:   "app_from_access",
		},
		{
			name:   "falls back to the id token audience as an array",
			access: map[string]any{"sub": "u1"},
			id:     map[string]any{"aud": []any{"app_from_id", "other"}},
			want:   "app_from_id",
		},
		{
			// OIDC allows `aud` to be a bare string; a reader that only
			// handles the array form would silently find nothing.
			name:   "falls back to the id token audience as a string",
			access: map[string]any{"sub": "u1"},
			id:     map[string]any{"aud": "app_bare_string"},
			want:   "app_bare_string",
		},
		{
			name:   "neither carries it",
			access: map[string]any{"sub": "u1"},
			id:     map[string]any{"sub": "u1"},
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view, err := ParseCodexView(codexBlob(t, fakeJWT(t, tc.access), fakeJWT(t, tc.id)))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := view.OAuthClientID(); got != tc.want {
				t.Errorf("OAuthClientID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A malformed or absent token must yield "" rather than panic: these blobs
// come from an operator paste.
func TestCodexOAuthClientID_MalformedTokensAreEmptyNotFatal(t *testing.T) {
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.b.c.d", "x." + "!!!not-base64!!!" + ".z"} {
		view, err := ParseCodexView(codexBlob(t, bad, bad))
		if err != nil {
			t.Fatalf("parse %q: %v", bad, err)
		}
		if got := view.OAuthClientID(); got != "" {
			t.Errorf("OAuthClientID() on %q = %q, want empty", bad, got)
		}
	}
}

// The defect this closes: with no client id configured, a codex refresh
// used to fail before it started. It must now reach the network with the
// id the credential names.
func TestRefreshRecord_CodexDerivesItsClientIDWhenUnconfigured(t *testing.T) {
	sealer, err := NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	blob := codexBlob(t, fakeJWT(t, map[string]any{"client_id": "app_derived"}), "")
	sealed, err := SealOAuthPayload(sealer, "alice", OAuthKindCodex, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	rec := OAuthRecord{UserID: "alice", Kind: OAuthKindCodex, SealedPayload: sealed}

	// No configured id. Refusing offline is the old behaviour; what must
	// not survive is the "not configured" refusal.
	err = RefreshRecord(context.Background(), sealer, &http.Client{Timeout: time.Millisecond}, "", "", &rec)
	if err == nil {
		t.Skip("refresh unexpectedly succeeded offline; nothing to assert")
	}
	if strings.Contains(err.Error(), "not configured") {
		t.Fatalf("codex refresh still refuses for a missing configured id: %v", err)
	}
}
