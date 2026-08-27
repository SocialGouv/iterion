package secrets

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// seedRecord seals a minimal credentials.json (with a refresh token) for
// ownerKey/kind and stores it in st, expiring at exp.
func seedRecord(t *testing.T, st OAuthStore, sealer Sealer, ownerKey string, kind OAuthKind, exp time.Time) {
	t.Helper()
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-old1234567890abcdef","refreshToken":"rf-old","expiresAt":0}}`)
	sealed, err := SealOAuthPayload(sealer, ownerKey, kind, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	e := exp
	if err := st.Upsert(context.Background(), OAuthRecord{
		UserID:               ownerKey,
		Kind:                 kind,
		SealedPayload:        sealed,
		AccessTokenExpiresAt: &e,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func TestOAuthRefreshWorker_RefreshesPersonalAndOrg(t *testing.T) {
	freshRetrySchedule(t)
	sealer, err := NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	st := NewMemoryOAuthStore()
	// One personal record + one org record, both expiring soon.
	soon := time.Now().Add(5 * time.Minute)
	seedRecord(t, st, sealer, "alice", OAuthKindClaudeCode, soon)
	seedRecord(t, st, sealer, OrgOwnerKey("team1"), OAuthKindClaudeCode, soon)
	// A record far from expiry must NOT be touched.
	seedRecord(t, st, sealer, "bob", OAuthKindClaudeCode, time.Now().Add(48*time.Hour))

	srv := newFakeOAuthServer(`{"access_token":"sk-ant-fresh1234567890abcdef","refresh_token":"rf-fresh","expires_in":28800}`, http.StatusOK)
	defer srv.Close()

	w := &OAuthRefreshWorker{
		Store:             st,
		Sealer:            sealer,
		HTTP:              redirectingClient(srv.URL),
		AnthropicClientID: "client-xyz",
		Lead:              30 * time.Minute,
	}
	n, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("refreshed count: got %d want 2 (alice + org)", n)
	}
	// Both refreshed records now carry the fresh access token + a bumped
	// LastRefreshedAt.
	for _, owner := range []string{"alice", OrgOwnerKey("team1")} {
		rec, err := st.Get(context.Background(), owner, OAuthKindClaudeCode)
		if err != nil {
			t.Fatalf("get %s: %v", owner, err)
		}
		if rec.LastRefreshedAt == nil {
			t.Errorf("%s: LastRefreshedAt not set", owner)
		}
		blob, err := OpenOAuthPayload(sealer, owner, OAuthKindClaudeCode, rec.SealedPayload)
		if err != nil {
			t.Fatalf("unseal %s: %v", owner, err)
		}
		v, _ := ParseAnthropicView(blob)
		if v.ClaudeAIOauth.AccessToken != "sk-ant-fresh1234567890abcdef" {
			t.Errorf("%s: token not rotated: %q", owner, v.ClaudeAIOauth.AccessToken)
		}
	}
	// bob (far from expiry) untouched.
	bob, _ := st.Get(context.Background(), "bob", OAuthKindClaudeCode)
	if bob.LastRefreshedAt != nil {
		t.Error("bob should not have been refreshed")
	}
}

// seedRecordNoRefreshToken seals a credentials.json WITHOUT a refresh
// token (an access-token-only snapshot) expiring at exp.
func seedRecordNoRefreshToken(t *testing.T, st OAuthStore, sealer Sealer, ownerKey string, exp time.Time) {
	t.Helper()
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-snapshot1234567890abcd","expiresAt":0}}`)
	sealed, err := SealOAuthPayload(sealer, ownerKey, OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	e := exp
	if err := st.Upsert(context.Background(), OAuthRecord{
		UserID:               ownerKey,
		Kind:                 OAuthKindClaudeCode,
		SealedPayload:        sealed,
		AccessTokenExpiresAt: &e,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func TestOAuthRefreshWorker_NotRefreshableIsSkippedNotFailed(t *testing.T) {
	freshRetrySchedule(t)
	sealer, _ := NewAESGCMSealer(make([]byte, 32))
	st := NewMemoryOAuthStore()
	// Legacy-shaped record: expiry tracked, but sealed payload has no
	// refresh token and NotRefreshable is unset (pre-migration row).
	seedRecordNoRefreshToken(t, st, sealer, "carol", time.Now().Add(time.Minute))

	w := &OAuthRefreshWorker{
		Store:             st,
		Sealer:            sealer,
		HTTP:              http.DefaultClient,
		AnthropicClientID: "client-xyz",
		Lead:              30 * time.Minute,
	}
	// First sweep: attempts, hits ErrNotRefreshable, self-heals — no error.
	n, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 refreshed, got %d", n)
	}
	rec, err := st.Get(context.Background(), "carol", OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !rec.NotRefreshable {
		t.Fatal("record should have been self-healed to NotRefreshable")
	}
	// Second sweep: skipped structurally, still quiet.
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce (2nd): %v", err)
	}
}

func TestRefreshRecord_NoRefreshTokenIsErrNotRefreshable(t *testing.T) {
	sealer, _ := NewAESGCMSealer(make([]byte, 32))
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-snapshot1234567890abcd"}}`)
	sealed, err := SealOAuthPayload(sealer, "dave", OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	rec := OAuthRecord{UserID: "dave", Kind: OAuthKindClaudeCode, SealedPayload: sealed}
	err = RefreshRecord(context.Background(), sealer, http.DefaultClient, "client-xyz", "", &rec)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrNotRefreshable) {
		t.Fatalf("expected ErrNotRefreshable, got %v", err)
	}
}

func TestOAuthRefreshWorker_SkipsKindWithoutClientID(t *testing.T) {
	sealer, _ := NewAESGCMSealer(make([]byte, 32))
	st := NewMemoryOAuthStore()
	seedRecord(t, st, sealer, "alice", OAuthKindClaudeCode, time.Now().Add(time.Minute))
	// No Anthropic client id configured → nothing refreshed, no error.
	w := &OAuthRefreshWorker{Store: st, Sealer: sealer, HTTP: http.DefaultClient}
	n, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 refreshed, got %d", n)
	}
}

// The subscription fingerprint's two invariants, pinned where they live:
// a legacy record is stamped once from its current payload (self-heal),
// and a record that already carries one KEEPS it through any refresh
// outcome — the refresh rewrites tokens for the SAME subscription, so a
// fingerprint that moved with the payload would rotate the meter every
// few hours and no reading would ever accumulate against its cap.
func TestRefreshRecord_SubscriptionFingerprintStampsOnceThenSticks(t *testing.T) {
	sealer, _ := NewAESGCMSealer(make([]byte, 32))
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-snapshot1234567890abcd"}}`)
	sealed, err := SealOAuthPayload(sealer, "erin", OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	legacy := OAuthRecord{UserID: "erin", Kind: OAuthKindClaudeCode, SealedPayload: sealed}
	_ = RefreshRecord(context.Background(), sealer, http.DefaultClient, "client-xyz", "", &legacy)
	if legacy.Fingerprint == "" {
		t.Fatal("legacy record not self-healed: fingerprint still empty after a refresh attempt")
	}
	if legacy.Fingerprint != fingerprintHex(string(blob)) {
		t.Fatalf("self-heal stamped %q, want the current payload's fingerprint", legacy.Fingerprint)
	}

	stamped := OAuthRecord{UserID: "erin", Kind: OAuthKindClaudeCode,
		SealedPayload: sealed, Fingerprint: "prior-identity"}
	_ = RefreshRecord(context.Background(), sealer, http.DefaultClient, "client-xyz", "", &stamped)
	if stamped.Fingerprint != "prior-identity" {
		t.Fatalf("refresh replaced the fingerprint (%q): the meter identity must survive token rotation", stamped.Fingerprint)
	}
}

// The half above cannot reach: a payload with no refreshToken returns
// ErrNotRefreshable before any token exchange, so "a SUCCESSFUL refresh
// preserves the identity" was asserted but never executed — and that is
// the invariant that actually protects the cap. A refresh REWRITES the
// payload (new access + refresh token, same subscription); if the
// fingerprint moved with it, the meter would rotate every few hours, no
// reading would ever accumulate against a cap, and the failure would be
// silent — a cap that measures nothing looks exactly like a cap with
// headroom.
func TestRefreshRecord_ASuccessfulRefreshKeepsTheSubscriptionIdentity(t *testing.T) {
	freshRetrySchedule(t)
	sealer, _ := NewAESGCMSealer(make([]byte, 32))
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-old1234567890abcd","refreshToken":"rf-old"}}`)
	sealed, err := SealOAuthPayload(sealer, "erin", OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	srv := newFakeOAuthServer(
		`{"access_token":"sk-ant-rotated1234567890abcd","refresh_token":"rf-rotated","expires_in":3600}`,
		http.StatusOK)
	defer srv.Close()

	rec := OAuthRecord{UserID: "erin", Kind: OAuthKindClaudeCode,
		SealedPayload: sealed, Fingerprint: "connect-time-identity"}
	if err := RefreshRecord(context.Background(), sealer, redirectingClient(srv.URL), "client-xyz", "", &rec); err != nil {
		t.Fatalf("RefreshRecord: %v", err)
	}

	// The refresh really happened — otherwise the assertion below would
	// hold vacuously, which is exactly the trap this test replaces.
	rotated, err := OpenOAuthPayload(sealer, rec.UserID, rec.Kind, rec.SealedPayload)
	if err != nil {
		t.Fatalf("unseal refreshed payload: %v", err)
	}
	view, err := ParseAnthropicView(rotated)
	if err != nil {
		t.Fatalf("parse refreshed payload: %v", err)
	}
	if view.ClaudeAIOauth.AccessToken != "sk-ant-rotated1234567890abcd" {
		t.Fatalf("payload not rotated (%q): the success path never ran", view.ClaudeAIOauth.AccessToken)
	}
	if rec.Fingerprint != "connect-time-identity" {
		t.Errorf("fingerprint = %q after a successful refresh, want the connect-time identity: the meter would rotate with every token", rec.Fingerprint)
	}
}
