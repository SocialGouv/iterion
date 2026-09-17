package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A refresh that succeeds but leaves the record with no FUTURE deadline must
// leave a cool-down, whatever the kind. DueForRefresh selects exactly such a
// record — an unknown deadline reads as due — so without one the next sweep
// runs the same exchange ten minutes later, forever, rotating a provider
// refresh token each pass.
//
// Per kind is how it shipped: the codex arm set the cool-down, the anthropic
// arm did not, and only the codex half converged. The decision now lives
// outside the switch, so the table below is the whole class and the next kind
// is covered before it is written.
func TestRefreshRecord_AnUndatableRefreshLeavesACoolDownForEveryKind(t *testing.T) {
	codexNoExpiry, err := json.Marshal(map[string]any{
		// No expires_in, and an access token with no readable `exp` claim.
		"access_token":  "opaque-not-a-jwt",
		"refresh_token": "rt.rotated",
	})
	if err != nil {
		t.Fatalf("marshal codex body: %v", err)
	}

	cases := []struct {
		kind     OAuthKind
		tokenEnv string
		body     string
		blob     func(t *testing.T) []byte
	}{
		{
			kind:     OAuthKindClaudeCode,
			tokenEnv: "ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL",
			// The exchange answers without expires_in — a shape the code
			// handles explicitly (RefreshAnthropic only stamps when > 0).
			body: `{"access_token":"sk-ant-newaccess1234567890abcdef","refresh_token":"rf-new"}`,
			blob: func(*testing.T) []byte {
				return []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-old1234567890abcdef","refreshToken":"rf-old"}}`)
			},
		},
		{
			kind:     OAuthKindCodex,
			tokenEnv: "ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL",
			body:     string(codexNoExpiry),
			blob: func(t *testing.T) []byte {
				return codexBlob(t, "opaque-not-a-jwt-either", "")
			},
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			freshRetrySchedule(t)
			sealer, err := NewAESGCMSealer(make([]byte, 32))
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			srv := newFakeOAuthServer(tc.body, http.StatusOK)
			defer srv.Close()
			t.Setenv(tc.tokenEnv, srv.URL+"/oauth/token")

			sealed, err := SealOAuthPayload(sealer, "alice", tc.kind, tc.blob(t))
			if err != nil {
				t.Fatalf("seal: %v", err)
			}
			rec := OAuthRecord{UserID: "alice", Kind: tc.kind, SealedPayload: sealed}

			before := time.Now().UTC()
			if err := RefreshRecord(context.Background(), sealer, pinnedClient(t, srv.URL), "client-id", "client-id", &rec); err != nil {
				t.Fatalf("RefreshRecord: %v", err)
			}
			if rec.AccessTokenExpiresAt != nil {
				t.Fatalf("expiry = %s, want none: this case exists because the exchange states no deadline",
					rec.AccessTokenExpiresAt)
			}
			if rec.RefreshNotBefore == nil {
				t.Fatal("no cool-down after an undatable refresh — the record is due on every sweep, so this " +
					"exchange re-runs every ten minutes for good and rotates the provider's refresh token each time")
			}
			if got := rec.RefreshNotBefore.Sub(before); got < undatableRefreshBackoff || got > undatableRefreshBackoff+time.Minute {
				t.Errorf("cool-down = %s from now, want ~%s", got, undatableRefreshBackoff)
			}
		})
	}
}

// The control, and the reason the cool-down is conditional: a refresh that
// DOES state a future deadline schedules on that deadline and must leave no
// cool-down behind — one would hold the sweep off a record that is due.
func TestRefreshRecord_ADatedRefreshLeavesNoCoolDown(t *testing.T) {
	freshRetrySchedule(t)
	sealer, err := NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	srv := newFakeOAuthServer(`{"access_token":"sk-ant-newaccess1234567890abcdef","refresh_token":"rf-new","expires_in":3600}`, http.StatusOK)
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", srv.URL+"/oauth/token")

	sealed, err := SealOAuthPayload(sealer, "alice", OAuthKindClaudeCode,
		[]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-old1234567890abcdef","refreshToken":"rf-old"}}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	past := time.Now().Add(-time.Hour).UTC()
	rec := OAuthRecord{
		UserID: "alice", Kind: OAuthKindClaudeCode, SealedPayload: sealed,
		AccessTokenExpiresAt: &past, RefreshNotBefore: &past,
	}
	if err := RefreshRecord(context.Background(), sealer, pinnedClient(t, srv.URL), "client-id", "", &rec); err != nil {
		t.Fatalf("RefreshRecord: %v", err)
	}
	if rec.AccessTokenExpiresAt == nil || !rec.AccessTokenExpiresAt.After(time.Now()) {
		t.Fatalf("expiry = %v, want the hour the exchange stated", rec.AccessTokenExpiresAt)
	}
	if rec.RefreshNotBefore != nil {
		t.Errorf("cool-down = %s on a record that now has a real deadline — the sweep would be held off a "+
			"record it is supposed to renew on time", rec.RefreshNotBefore)
	}
}
