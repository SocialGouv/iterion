package runner

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// codexAuthBlob builds a ~/.codex/auth.json whose access token carries an
// `exp` claim — the only expiry a real codex blob has, and therefore the
// one a refresher would schedule from.
func codexAuthBlob(t *testing.T, exp time.Time) []byte {
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
			"access_token": access, "refresh_token": "rt.codex", "account_id": "acct-1",
		},
	})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	return blob
}

// TestStartOAuthRefreshers_NeverRefreshesACodexForfait pins the ownership
// rule the runner-side refresher rests on: a codex credential is refreshed
// by exactly ONE owner, the server-side OAuthRefreshWorker, never by a run.
//
// The rule is not cosmetic. A run's codex blob comes from a SHARED tier
// (the platform record is one meter for the whole deployment), this loop
// rewrites only the run-local file and never the OAuthStore, and OpenAI
// rotates the refresh token on use — so one run refreshing would invalidate
// the token the store and every sibling run still hold. That is the
// measured incident in docs/bot-runs/feed-watch.md ("each refresh
// invalidates the other holder" → "one session, one record, one
// refresher").
//
// Both credentials are handed over ALREADY EXPIRED, so a refresher that
// admitted either kind would fire immediately. The anthropic hit is the
// positive control: it proves the function really ran and really refreshes
// what it does admit, so a silent zero-refresh regression cannot pass this
// test while the codex assertion stays green.
func TestStartOAuthRefreshers_NeverRefreshesACodexForfait(t *testing.T) {
	var anthropicHits, codexHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/anthropic":
			anthropicHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			// A far-future expiry so the anthropic loop refreshes once and
			// then sleeps, instead of spinning for the rest of the test.
			fmt.Fprintf(w, `{"access_token":"fresh","refresh_token":"rt.anthropic2","expires_in":28800}`)
		case "/codex":
			codexHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("ITERION_OAUTH_FORFAIT_ANTHROPIC_TOKEN_URL", srv.URL+"/anthropic")
	t.Setenv("ITERION_OAUTH_FORFAIT_CODEX_TOKEN_URL", srv.URL+"/codex")

	dir := t.TempDir()
	codexPath := filepath.Join(dir, "auth.json")
	codexBefore := codexAuthBlob(t, time.Now().Add(-time.Hour))
	if err := os.WriteFile(codexPath, codexBefore, 0o600); err != nil {
		t.Fatalf("write codex: %v", err)
	}
	anthropicPath := filepath.Join(dir, ".credentials.json")
	anthropicBlob := fmt.Sprintf(
		`{"claudeAiOauth":{"accessToken":"stale","refreshToken":"rt.anthropic","expiresAt":%d}}`,
		time.Now().Add(-time.Hour).UnixMilli())
	if err := os.WriteFile(anthropicPath, []byte(anthropicBlob), 0o600); err != nil {
		t.Fatalf("write anthropic: %v", err)
	}

	r := &Runner{}
	stop := make(chan struct{})
	defer close(stop)
	r.startOAuthRefreshers(stop, "run-ownership", map[string]string{
		string(secrets.OAuthKindClaudeCode): anthropicPath,
		string(secrets.OAuthKindCodex):      codexPath,
	})

	// Positive control: wait for the kind that IS owned by the runner.
	deadline := time.Now().Add(10 * time.Second)
	for anthropicHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if anthropicHits.Load() == 0 {
		t.Fatal("the claude_code forfait was not refreshed — the positive control failed, " +
			"so the codex assertion below would pass vacuously")
	}
	// Give a codex refresher, had one been started, the same chance to run.
	time.Sleep(250 * time.Millisecond)

	if got := codexHits.Load(); got != 0 {
		t.Errorf("the runner exchanged a codex refresh token %d time(s); it must never refresh a "+
			"credential the OAuthRefreshWorker owns — rotation invalidates the store's copy and "+
			"every sibling run's (docs/bot-runs/feed-watch.md)", got)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex after: %v", err)
	}
	if string(after) != string(codexBefore) {
		t.Errorf("the runner rewrote the codex credential file; it must leave it untouched")
	}
}
