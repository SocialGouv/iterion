package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The refresh worker's ticker is re-phased by every restart, so without a
// sweep at start a token the old replica was about to rotate sits expired
// for a full period on the new one. Nothing downstream compensates: the
// cloud publisher seals whatever the store holds and no tier consults
// access_token_expires_at, so those minutes are spent handing runs a
// credential the server itself knows is dead — and this branch's connect
// path accepts an expired-but-refreshable codex record precisely on the
// promise that "the refresh worker renews it on its next pass".
//
// The oracle is the provider being called at all within a second of start:
// a ten-minute tick cannot pass this test, and a unit test of RunOnce
// cannot see the hazard, which is how it went missing while the forge
// worker twenty lines above has had its boot sweep all along.
func TestStartOAuthForfaitRefresh_SweepsAtBootNotOnlyOnTheTicker(t *testing.T) {
	var hits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-refreshed1234567890abcd","refresh_token":"rf-new","expires_in":3600}`))
	}))
	defer provider.Close()

	srv, _, _, store := oauthTestServer(t)
	srv.cfg.AnthropicOAuthClientID = "client-xyz"
	srv.httpClient = &http.Client{Transport: rewriteHostTransport{target: provider.URL}}

	// A record that is already expired but carries a refresh token: exactly
	// what a restart inherits from the replica it replaced.
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-stale1234567890abcdef","refreshToken":"rf-old","expiresAt":0}}`)
	sealed, err := secrets.SealOAuthPayload(srv.sealer, "alice", secrets.OAuthKindClaudeCode, blob)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	past := time.Now().Add(-time.Hour).UTC()
	if err := store.Upsert(context.Background(), secrets.OAuthRecord{
		UserID: "alice", Kind: secrets.OAuthKindClaudeCode, SealedPayload: sealed, AccessTokenExpiresAt: &past,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	srv.startOAuthForfaitRefresh()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("no refresh at start: an expired credential stays expired until the first 10-minute tick, " +
			"and every run launched meanwhile is handed a token the server knows is dead")
	}

	rec, err := store.Get(context.Background(), "alice", secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.AccessTokenExpiresAt == nil || !rec.AccessTokenExpiresAt.After(time.Now()) {
		t.Fatalf("stored expiry = %v, want a future one: the boot sweep must persist what it refreshed", rec.AccessTokenExpiresAt)
	}
}

// rewriteHostTransport points every request at a local test server,
// whatever production URL the refresh client builds.
type rewriteHostTransport struct{ target string }

func (rt rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u, err := clone.URL.Parse(rt.target)
	if err != nil {
		return nil, err
	}
	clone.URL = u
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}
