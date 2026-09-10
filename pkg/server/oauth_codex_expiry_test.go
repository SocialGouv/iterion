package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// A codex blob whose access token is already expired AND carries no refresh
// token is dead on arrival: nothing can renew it, so every run that draws
// this credential fails its first LLM call — with an error naming neither
// the credential nor the reason. Stamping the `exp` claim is what makes
// that state readable at connect for the first time, so it is said here,
// where the operator is still looking.
//
// Said, not refused: the record is stored and the upload succeeds. The
// operator may legitimately upload before logging in again, and the
// deployment keeps the choice — the log is the thing that was missing.
func TestCodexConnect_DeadOnArrivalCredentialSaysSo(t *testing.T) {
	srv, hs, signer, oauthStore := oauthTestServer(t)
	var logs bytes.Buffer
	srv.logger = iterlog.New(iterlog.LevelWarn, &logs)
	jo := oauthJWT(t, signer, "jo")

	expired := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	blob := `{"tokens":{"access_token":"` + codexJWTAccessToken(t, expired) +
		`","account_id":"acct-1"},"auth_mode":"chatgpt"}`

	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/credentials", jo, blob)
	if code != http.StatusOK {
		t.Fatalf("upload = %d body=%s, want 200 — this is a warning, not a refusal", code, body)
	}
	rec, err := oauthStore.Get(t.Context(), "jo", secrets.OAuthKindCodex)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if !rec.NotRefreshable {
		t.Fatal("a blob with no refresh token must be stored NotRefreshable")
	}
	if l := logs.String(); !strings.Contains(l, "NO refresh") || !strings.Contains(l, expired.Format(time.RFC3339)) {
		t.Fatalf("want a Warn naming the expiry and the missing refresh token; got:\n%s", l)
	}
}

// The manual refresh endpoint is the second holder of a shared credential,
// beside the background sweep — a refresh is read → provider round trip →
// persist, and OpenAI retires the refresh token it is handed, so two of
// them overlapping leave one holding a credential the provider has already
// invalidated. It therefore takes the same per-record claim the sweep does.
//
// Both answers the docs promise are asserted on the real HTTP path:
//   - claim held elsewhere  → 409, and NO exchange is attempted;
//   - claim lost in flight  → 409, and the tokens are discarded rather than
//     written over the record that superseded them.
func TestOAuthRefresh_ManualRefreshIsFencedByTheSameClaim(t *testing.T) {
	srv, hs, signer, store := oauthTestServer(t)
	jo := oauthJWT(t, signer, "jo")

	var hits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-refreshed1234567890abcd","refresh_token":"rf-new","expires_in":3600}`))
	}))
	defer provider.Close()
	srv.httpClient = &http.Client{Transport: rewriteHostTransport{target: provider.URL}}
	srv.cfg.AnthropicOAuthClientID = "client-xyz"

	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-stored1234567890abcdef","refreshToken":"rf-old","expiresAt":0}}`)
	sealed, err := secrets.SealOAuthPayload(srv.sealer, "jo", secrets.OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	exp := time.Now().Add(time.Hour).UTC()
	if err := store.Upsert(t.Context(), secrets.OAuthRecord{
		UserID: "jo", Kind: secrets.OAuthKindClaudeCode, SealedPayload: sealed, AccessTokenExpiresAt: &exp,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Somebody else (a sweep, another operator) holds the claim.
	now := time.Now().UTC()
	ok, err := store.ClaimRefresh(t.Context(), secrets.OAuthRecordID("jo", secrets.OAuthKindClaudeCode, 0), "someone-else", now, now.Add(secrets.RefreshClaimTTL))
	if err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/refresh", jo, "")
	if code != http.StatusConflict {
		t.Fatalf("refresh behind a live claim = %d body=%s, want 409", code, body)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("provider called %d times behind a live claim, want 0 — the exchange retires the "+
			"refresh token the other holder is about to store", n)
	}

	// Claim released, but the credential is REPLACED while this refresh is
	// in flight: the record it is about to write no longer exists. The
	// re-connect (Upsert) clears the claim, which is what makes the commit
	// fail closed.
	if err := store.ReleaseRefreshClaim(t.Context(), secrets.OAuthRecordID("jo", secrets.OAuthKindClaudeCode, 0), "someone-else", nil); err != nil {
		t.Fatalf("release: %v", err)
	}
	reconnected, err := secrets.SealOAuthPayload(srv.sealer, "jo", secrets.OAuthKindClaudeCode,
		[]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-reconnected123456789ab","refreshToken":"rf-brand-new","expiresAt":0}}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	srv.httpClient = &http.Client{Transport: reconnectMidFlightTransport{
		target: provider.URL,
		before: func() {
			_ = store.Upsert(context.Background(), secrets.OAuthRecord{
				UserID: "jo", Kind: secrets.OAuthKindClaudeCode, SealedPayload: reconnected, AccessTokenExpiresAt: &exp,
			})
		},
	}}
	code, body = oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/refresh", jo, "")
	if code != http.StatusConflict {
		t.Fatalf("refresh superseded mid-flight = %d body=%s, want 409", code, body)
	}
	rec, err := store.Get(t.Context(), "jo", secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	plain, err := secrets.OpenOAuthPayload(srv.sealer, "jo", secrets.OAuthKindClaudeCode, rec.SealedPayload)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if !strings.Contains(string(plain), "rf-brand-new") {
		t.Fatalf("stored credential = %s, want the one the operator just connected: a refresh of the "+
			"session it replaced must not overwrite it", plain)
	}
}

// A refused claim has two causes, and the endpoint must not report the
// wrong one: a live lease is seconds away, while the cool-down a refresh
// that could not date its token leaves behind lasts an HOUR. Telling an
// operator in the second state to "retry in a moment" sends them back to a
// button that answers 409 for the rest of the hour, with the real remedy —
// a re-connect, which clears the field — never mentioned. The two states
// share one field on the record (a cool-down IS the lease instant with no
// owner), so nothing but the stored record distinguishes them.
func TestOAuthRefresh_ACoolDownIsNotReportedAsARefreshInFlight(t *testing.T) {
	srv, hs, signer, store := oauthTestServer(t)
	jo := oauthJWT(t, signer, "jo")

	var hits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"` + codexJWTAccessToken(t, time.Now().Add(time.Hour)) + `"}`))
	}))
	defer provider.Close()
	srv.httpClient = &http.Client{Transport: rewriteHostTransport{target: provider.URL}}
	srv.cfg.CodexOAuthClientID = "app_x"

	sealed, err := secrets.SealOAuthPayload(srv.sealer, "jo", secrets.OAuthKindCodex,
		[]byte(`{"tokens":{"access_token":"`+codexJWTAccessToken(t, time.Now().Add(-time.Hour))+
			`","refresh_token":"rt.old","account_id":"acct-1"},"auth_mode":"chatgpt"}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	expired := time.Now().Add(-time.Hour).UTC()
	if err := store.Upsert(t.Context(), secrets.OAuthRecord{
		UserID: "jo", Kind: secrets.OAuthKindCodex, SealedPayload: sealed, AccessTokenExpiresAt: &expired,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// The state an undatable refresh leaves: no owner, a cool-down an hour out.
	now := time.Now().UTC()
	if ok, err := store.ClaimRefresh(t.Context(), secrets.OAuthRecordID("jo", secrets.OAuthKindCodex, 0), "previous-sweep", now, now.Add(secrets.RefreshClaimTTL)); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	cool := now.Add(time.Hour).Truncate(time.Second)
	if err := store.ReleaseRefreshClaim(t.Context(), secrets.OAuthRecordID("jo", secrets.OAuthKindCodex, 0), "previous-sweep", &cool); err != nil {
		t.Fatalf("seed cool-down: %v", err)
	}

	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/refresh", jo, "")
	if code != http.StatusConflict {
		t.Fatalf("refresh during a cool-down = %d body=%s, want 409", code, body)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("provider called %d times during a cool-down, want 0", n)
	}
	if strings.Contains(body, "already in flight") {
		t.Fatalf("409 body = %s — no refresh is in flight; a cool-down reported as one tells the operator "+
			"to retry in a moment for an hour", body)
	}
	if !strings.Contains(body, "cool-down") || !strings.Contains(body, cool.UTC().Format(time.RFC3339)) {
		t.Fatalf("409 body = %s, want the cool-down named with its end instant %s",
			body, cool.UTC().Format(time.RFC3339))
	}

	// A live lease still reads as one: the branch above must not swallow the
	// answer the docs promise for a genuinely concurrent refresh.
	if err := store.Upsert(t.Context(), secrets.OAuthRecord{
		UserID: "jo", Kind: secrets.OAuthKindCodex, SealedPayload: sealed, AccessTokenExpiresAt: &expired,
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if ok, err := store.ClaimRefresh(t.Context(), secrets.OAuthRecordID("jo", secrets.OAuthKindCodex, 0), "someone-else", now, now.Add(secrets.RefreshClaimTTL)); err != nil || !ok {
		t.Fatalf("seed live claim: ok=%v err=%v", ok, err)
	}
	code, body = oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/refresh", jo, "")
	if code != http.StatusConflict || !strings.Contains(body, "already in flight") {
		t.Fatalf("refresh behind a live claim = %d body=%s, want 409 naming the refresh in flight", code, body)
	}
}

// reconnectMidFlightTransport runs `before` while the refresh request is on
// the wire — the only place a re-connect can race a refresh that has
// already taken its claim.
type reconnectMidFlightTransport struct {
	target string
	before func()
}

func (rt reconnectMidFlightTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.before()
	return rewriteHostTransport{target: rt.target}.RoundTrip(req)
}

// A codex forfait exported from a machine whose token has already lapsed is
// ACCEPTED — nothing is wrong with it that a refresh cannot fix — on the
// promise that the worker renews it. Without a kick that promise is up to a
// ticker period away, and the publisher seals whatever the store holds with
// no tier consulting the expiry: every run launched in between is handed a
// token the server itself knows is dead.
//
// The oracle is the STORE settling on a renewed record, not the response:
// the upload succeeds either way, which is exactly why the bug would be
// invisible from the endpoint.
func TestCodexConnect_AnExpiredCredentialIsRefreshedWithoutWaitingForTheTicker(t *testing.T) {
	srv, hs, signer, store := oauthTestServer(t)
	jo := oauthJWT(t, signer, "jo")

	fresh := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"` + codexJWTAccessToken(t, fresh) + `","refresh_token":"rt.rotated"}`))
	}))
	defer provider.Close()
	srv.httpClient = &http.Client{Transport: rewriteHostTransport{target: provider.URL}}
	srv.cfg.CodexOAuthClientID = "app_x"

	expired := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	blob := `{"tokens":{"access_token":"` + codexJWTAccessToken(t, expired) +
		`","refresh_token":"rt.old","account_id":"acct-1"},"auth_mode":"chatgpt"}`
	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/credentials", jo, blob)
	if code != http.StatusOK {
		t.Fatalf("upload = %d body=%s, want 200 — an expired but refreshable credential is accepted", code, body)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, err := store.Get(t.Context(), "jo", secrets.OAuthKindCodex)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if rec.AccessTokenExpiresAt != nil && rec.AccessTokenExpiresAt.After(time.Now()) {
			plain, err := secrets.OpenOAuthPayload(srv.sealer, "jo", secrets.OAuthKindCodex, rec.SealedPayload)
			if err != nil {
				t.Fatalf("unseal: %v", err)
			}
			if !strings.Contains(string(plain), "rt.rotated") {
				t.Fatalf("stored payload was not re-sealed around the refreshed tokens: %s", plain)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the credential is still expired after the connect: it waits for the periodic sweep, " +
				"and every run launched until then is handed a token that cannot serve it")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
