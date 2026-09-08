package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// codexJWTAccessToken builds an access token shaped like a real one: three
// segments, unpadded base64url payload, a numeric `exp`.
func codexJWTAccessToken(t *testing.T, exp time.Time) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	claims, err := json.Marshal(map[string]any{"exp": exp.Unix(), "client_id": "app_x"})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(claims) + ".sig"
}

// Connecting a codex forfait must leave a record the refresh worker can
// actually SEE. The worker sweeps OAuthStore.ExpiringBefore, whose query
// requires access_token_expires_at to exist — so a record connected
// without that field is invisible to it forever and can only be renewed by
// a human re-paste.
//
// That was the hole: the connect path stamped the field only from
// `expires_in`, which real ~/.codex/auth.json blobs never carry. The
// oracle here is deliberately ExpiringBefore itself rather than the field:
// asserting the stamp alone would not prove the record became sweepable,
// which is the property the ten-day dead forfait actually lacked.
func TestCodexConnect_LeavesARecordTheRefreshWorkerCanSee(t *testing.T) {
	_, hs, signer, oauthStore := oauthTestServer(t)
	jo := oauthJWT(t, signer, "jo")

	exp := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	blob := `{"tokens":{"access_token":"` + codexJWTAccessToken(t, exp) +
		`","refresh_token":"rt","account_id":"acct-1"},"auth_mode":"chatgpt"}`

	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/credentials", jo, blob)
	if code != http.StatusOK {
		t.Fatalf("upload = %d body=%s, want 200", code, body)
	}

	rec, err := oauthStore.Get(t.Context(), "jo", secrets.OAuthKindCodex)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if rec.AccessTokenExpiresAt == nil {
		t.Fatal("stored codex record carries no access-token expiry — ExpiringBefore can never " +
			"return it, so the refresh worker will never renew this forfait")
	}
	if got := rec.AccessTokenExpiresAt.UTC(); !got.Equal(exp) {
		t.Errorf("stored expiry = %s, want the access token's own exp claim %s", got, exp)
	}

	// The property that matters: the worker's own selector reaches it.
	found, err := oauthStore.ExpiringBefore(t.Context(), exp.Add(time.Minute))
	if err != nil {
		t.Fatalf("ExpiringBefore: %v", err)
	}
	var seen bool
	for _, r := range found {
		if r.UserID == "jo" && r.Kind == secrets.OAuthKindCodex {
			seen = true
		}
	}
	if !seen {
		t.Error("the refresh worker's sweep does not return the codex record just connected")
	}
}

// A blob whose access token carries no readable `exp` must not be stamped
// with an invented deadline. Leaving the field nil is honest — the record
// stays out of the sweep, which is a visible gap — where a guessed expiry
// would put a dead token in front of a run claiming to be valid.
func TestCodexConnect_UnreadableExpiryIsNotInvented(t *testing.T) {
	_, hs, signer, oauthStore := oauthTestServer(t)
	jo := oauthJWT(t, signer, "jo")

	blob := `{"tokens":{"access_token":"opaque-not-a-jwt-token","refresh_token":"rt",` +
		`"account_id":"acct-1"},"auth_mode":"chatgpt"}`
	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/credentials", jo, blob)
	if code != http.StatusOK {
		t.Fatalf("upload = %d body=%s, want 200", code, body)
	}
	rec, err := oauthStore.Get(t.Context(), "jo", secrets.OAuthKindCodex)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if rec.AccessTokenExpiresAt != nil {
		t.Errorf("expiry = %s, want nil: nothing in the blob states when this token dies",
			rec.AccessTokenExpiresAt)
	}
}

// expires_in stays the fallback for a blob that carries one, so the older
// shape keeps working.
func TestCodexConnect_FallsBackToExpiresIn(t *testing.T) {
	_, hs, signer, oauthStore := oauthTestServer(t)
	jo := oauthJWT(t, signer, "jo")

	blob := `{"tokens":{"access_token":"opaque-not-a-jwt-token","refresh_token":"rt",` +
		`"account_id":"acct-1","expires_in":3600},"auth_mode":"chatgpt"}`
	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/credentials", jo, blob)
	if code != http.StatusOK {
		t.Fatalf("upload = %d body=%s, want 200", code, body)
	}
	rec, err := oauthStore.Get(t.Context(), "jo", secrets.OAuthKindCodex)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if rec.AccessTokenExpiresAt == nil {
		t.Fatal("expires_in was present but no expiry was stored")
	}
	if d := time.Until(*rec.AccessTokenExpiresAt); d < 55*time.Minute || d > 65*time.Minute {
		t.Errorf("expiry in %s, want ~1h from expires_in", d)
	}
}
