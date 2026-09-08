package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
// failed before it started. It must now reach the token endpoint carrying
// the id the credential names.
//
// The oracle is the client_id that ARRIVES on the wire, not the shape of
// an error. Asserting "the failure is no longer the 'not configured' one"
// looks equivalent and is not: every other failure — including the
// derivation returning nothing and refusing one line later — satisfies it
// too, so the test passes with the whole feature removed.
func TestRefreshRecord_CodexDerivesItsClientIDWhenUnconfigured(t *testing.T) {
	freshRetrySchedule(t)
	var gotClientID, gotGrant, gotRefresh string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotClientID, gotGrant = r.PostForm.Get("client_id"), r.PostForm.Get("grant_type")
		gotRefresh = r.PostForm.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"codex-newaccess1234567890","refresh_token":"rt.new","expires_in":3600}`))
	}))
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL", srv.URL+"/oauth/token")

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

	// No configured id anywhere: only the credential can supply one.
	if err := RefreshRecord(context.Background(), sealer, pinnedClient(t, srv.URL), "", "", &rec); err != nil {
		t.Fatalf("RefreshRecord with a derivable client id: %v", err)
	}
	if gotClientID != "app_derived" {
		t.Errorf("client_id on the wire = %q, want %q — the id was not taken from the credential",
			gotClientID, "app_derived")
	}
	if gotGrant != "refresh_token" || gotRefresh != "rt.test" {
		t.Errorf("grant_type=%q refresh_token=%q, want refresh_token/rt.test", gotGrant, gotRefresh)
	}
}

// An explicitly configured id stays the operator's override: it is the
// escape hatch for a credential whose own claim is wrong, so a derivation
// that quietly outranked it would remove the only way to correct one.
func TestRefreshRecord_CodexPrefersTheConfiguredClientID(t *testing.T) {
	freshRetrySchedule(t)
	var gotClientID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotClientID = r.PostForm.Get("client_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"codex-newaccess1234567890","refresh_token":"rt.new","expires_in":3600}`))
	}))
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL", srv.URL+"/oauth/token")

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

	if err := RefreshRecord(context.Background(), sealer, pinnedClient(t, srv.URL), "", "app_configured", &rec); err != nil {
		t.Fatalf("RefreshRecord: %v", err)
	}
	if gotClientID != "app_configured" {
		t.Errorf("client_id on the wire = %q, want the configured %q", gotClientID, "app_configured")
	}
}
