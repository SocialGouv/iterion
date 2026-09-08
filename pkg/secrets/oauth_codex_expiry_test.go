package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// A codex refresh must leave the record SELECTABLE for the next sweep.
// The worker only ever sees records ExpiringBefore returns, and that query
// requires access_token_expires_at to exist — so a refresh that renews the
// token but leaves the stored expiry untouched refreshes the record once
// and then loses sight of it for good.
//
// The token endpoint is not required to send expires_in (and the real one
// does not always), so the fallback is the new access token's own `exp`
// claim. This is the case that fallback exists for: a successful refresh
// whose response states no lifetime.
func TestRefreshRecord_CodexStampsExpiryFromTheNewTokenWhenTheResponseStatesNone(t *testing.T) {
	freshRetrySchedule(t)
	sealer, err := NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}

	newExp := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	// Deliberately NO expires_in in the response.
	body, err := json.Marshal(map[string]any{
		"access_token":  fakeJWT(t, map[string]any{"exp": newExp.Unix(), "client_id": "app_derived"}),
		"refresh_token": "rt.rotated",
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	srv := newFakeOAuthServer(string(body), http.StatusOK)
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL", srv.URL+"/oauth/token")

	blob := codexBlob(t, fakeJWT(t, map[string]any{
		"exp": time.Now().Add(time.Minute).Unix(), "client_id": "app_derived",
	}), "")
	sealed, err := SealOAuthPayload(sealer, "alice", OAuthKindCodex, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	rec := OAuthRecord{UserID: "alice", Kind: OAuthKindCodex, SealedPayload: sealed}

	if err := RefreshRecord(context.Background(), sealer, pinnedClient(t, srv.URL), "", "", &rec); err != nil {
		t.Fatalf("RefreshRecord: %v", err)
	}
	if rec.AccessTokenExpiresAt == nil {
		t.Fatal("no expiry stamped after a successful refresh — ExpiringBefore will never return " +
			"this record again, so the forfait silently stops being refreshed")
	}
	if got := rec.AccessTokenExpiresAt.UTC(); !got.Equal(newExp) {
		t.Errorf("stamped expiry = %s, want the refreshed token's own exp %s", got, newExp)
	}
}

// The provider's own expires_in stays authoritative when it sends one —
// the claim is the fallback, not a replacement.
func TestRefreshRecord_CodexPrefersTheProvidersExpiresIn(t *testing.T) {
	freshRetrySchedule(t)
	sealer, err := NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	claimExp := time.Now().Add(9 * time.Hour).UTC().Truncate(time.Second)
	body, err := json.Marshal(map[string]any{
		"access_token":  fakeJWT(t, map[string]any{"exp": claimExp.Unix()}),
		"refresh_token": "rt.rotated",
		"expires_in":    3600,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	srv := newFakeOAuthServer(string(body), http.StatusOK)
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL", srv.URL+"/oauth/token")

	blob := codexBlob(t, fakeJWT(t, map[string]any{"client_id": "app_derived"}), "")
	sealed, err := SealOAuthPayload(sealer, "alice", OAuthKindCodex, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	rec := OAuthRecord{UserID: "alice", Kind: OAuthKindCodex, SealedPayload: sealed}

	if err := RefreshRecord(context.Background(), sealer, pinnedClient(t, srv.URL), "", "", &rec); err != nil {
		t.Fatalf("RefreshRecord: %v", err)
	}
	if rec.AccessTokenExpiresAt == nil {
		t.Fatal("no expiry stamped")
	}
	if d := time.Until(*rec.AccessTokenExpiresAt); d < 55*time.Minute || d > 65*time.Minute {
		t.Errorf("expiry in %s, want ~1h from the response's expires_in (not the %s claim)",
			d, time.Until(claimExp))
	}
}

// An unreadable token yields no stamp rather than an invented one; the
// previously stored value is left exactly as it was.
func TestCodexRefreshedExpiry_UnknownStaysZero(t *testing.T) {
	blob := codexBlob(t, "opaque-not-a-jwt", "")
	if got := codexRefreshedExpiry(RefreshResult{}, blob); !got.IsZero() {
		t.Errorf("expiry = %s, want the zero time for a token stating nothing", got)
	}
	if got := codexRefreshedExpiry(RefreshResult{}, []byte("not json")); !got.IsZero() {
		t.Errorf("expiry = %s, want the zero time for an unparseable blob", got)
	}
}
