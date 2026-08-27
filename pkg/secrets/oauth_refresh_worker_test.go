package secrets

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
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
	if legacy.Fingerprint != OAuthIdentityFingerprint(OAuthKindClaudeCode, blob) {
		t.Fatalf("self-heal stamped %q, want the payload's subscription identity", legacy.Fingerprint)
	}

	stamped := OAuthRecord{UserID: "erin", Kind: OAuthKindClaudeCode,
		SealedPayload: sealed, Fingerprint: "prior-identity"}
	_ = RefreshRecord(context.Background(), sealer, http.DefaultClient, "client-xyz", "", &stamped)
	if stamped.Fingerprint != "prior-identity" {
		t.Fatalf("refresh replaced the fingerprint (%q): the meter identity must survive token rotation", stamped.Fingerprint)
	}
}

// stubTokenEndpoint answers any request with one canned JSON body, so the
// refresh EXCHANGE can be driven without a network. RefreshRecord posts to
// a const URL, but it takes the *http.Client — the transport is the seam.
type stubTokenEndpoint struct{ body string }

func (s stubTokenEndpoint) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    r,
	}, nil
}

// The invariant above, on the path that actually rotates tokens. The
// sibling test can only reach the ErrNotRefreshable early return, so it
// proves the field is not cleared — not that a SUCCESSFUL exchange leaves
// it alone, which is the case that repeats every few hours in production
// and would silently reset the meter each time.
func TestRefreshRecord_SuccessfulRefreshKeepsTheSubscriptionIdentity(t *testing.T) {
	sealer, _ := NewAESGCMSealer(make([]byte, 32))
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat-old-000000000000","refreshToken":"sk-ant-ort-stable-1234"}}`)
	sealed, err := SealOAuthPayload(sealer, "erin", OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	hc := &http.Client{Transport: stubTokenEndpoint{
		body: `{"access_token":"sk-ant-oat-fresh-111111111111","refresh_token":"sk-ant-ort-rotated-9999","expires_in":3600}`,
	}}

	rec := OAuthRecord{UserID: "erin", Kind: OAuthKindClaudeCode, SealedPayload: sealed}
	if err := RefreshRecord(context.Background(), sealer, hc, "client-xyz", "", &rec); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// The exchange really happened: the sealed payload now carries the
	// rotated tokens. Without this the assertion below would pass on a
	// no-op.
	fresh, err := OpenOAuthPayload(sealer, rec.UserID, rec.Kind, rec.SealedPayload)
	if err != nil {
		t.Fatalf("unseal refreshed: %v", err)
	}
	view, err := ParseAnthropicView(fresh)
	if err != nil {
		t.Fatalf("parse refreshed: %v", err)
	}
	if view.ClaudeAIOauth.AccessToken != "sk-ant-oat-fresh-111111111111" {
		t.Fatalf("payload not rotated: access token is %q", view.ClaudeAIOauth.AccessToken)
	}
	if view.ClaudeAIOauth.RefreshToken != "sk-ant-ort-rotated-9999" {
		t.Fatalf("payload not rotated: refresh token is %q", view.ClaudeAIOauth.RefreshToken)
	}

	// ... and the identity is the one stamped from the PRE-refresh payload,
	// not re-derived from the rotated one.
	want := OAuthIdentityFingerprint(OAuthKindClaudeCode, blob)
	if rec.Fingerprint != want {
		t.Fatalf("fingerprint after a successful refresh = %q, want the stamped identity %q", rec.Fingerprint, want)
	}
	// Guard against a vacuous assertion: re-deriving from the rotated
	// payload must give a DIFFERENT value, or the check above proves
	// nothing about preservation.
	if got := OAuthIdentityFingerprint(OAuthKindClaudeCode, fresh); got == want {
		t.Fatalf("the rotated payload re-derives the same identity %q — this test cannot detect a re-stamp", got)
	}
}
