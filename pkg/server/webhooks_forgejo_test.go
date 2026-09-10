package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/prforge"
)

func fjConfig(t *testing.T, s *Server) (webhooks.Config, string) {
	t.Helper()
	pt, hash, last4, fp, err := webhooks.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg := webhooks.Config{
		ID:          "fjw",
		TenantID:    "t1",
		Provider:    webhooks.ProviderForgejo,
		SignMode:    webhooks.SignModeHMAC,
		Enabled:     true,
		TokenHash:   hash,
		TokenLast4:  last4,
		Fingerprint: fp,
		BotIDs:      []string{"review-pr"},
	}
	sealed, err := webhooks.SealHMACSecret(s.sealer, cfg.ID, pt)
	if err != nil {
		t.Fatal(err)
	}
	cfg.HMACSecretSealed = sealed
	return cfg, pt
}

// fjReq signs the body using a Gitea-style raw-hex signature header.
// The eventHeader argument lets each test pick between
// X-Forgejo-Event / X-Gitea-Event / a custom value.
func fjReq(ctx context.Context, body, eventName, eventVal, pt, sigHeader string) *http.Request {
	r := httptest.NewRequest("POST", "/api/webhooks/forgejo/fjw", strings.NewReader(body)).WithContext(ctx)
	if eventName != "" {
		r.Header.Set(eventName, eventVal)
	}
	if pt != "" {
		mac := hmac.New(sha256.New, []byte(pt))
		mac.Write([]byte(body))
		r.Header.Set(sigHeader, hex.EncodeToString(mac.Sum(nil)))
	}
	r.SetPathValue("id", "fjw")
	return r
}

const fjOpenPR = `{
  "action": "opened",
  "number": 7,
  "pull_request": {"number": 7, "title": "Add X", "body": "desc",
    "html_url": "https://codeberg.org/acme/widgets/pulls/7", "state": "open",
    "head": {"ref": "feature/x", "sha": "abc123", "repo": {"full_name": "acme/widgets"}}, "base": {"ref": "main"}},
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://codeberg.org/acme/widgets.git"},
  "sender": {"login": "alice"}
}`

func TestForgejoWebhook_HappyPath_ForgejoHeaders(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotURL, gotRef string
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, repoURL, repoRef, projectPath string, _, _ map[string]string) (string, error) {
		calls++
		gotURL, gotRef = repoURL, repoRef
		return "run-fj-1", nil
	}
	cfg, pt := fjConfig(t, s)
	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjOpenPR, "X-Forgejo-Event", prforge.EventHeaderPullRequest, pt, "X-Forgejo-Signature"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 || gotURL != "https://codeberg.org/acme/widgets.git" || gotRef != "feature/x" {
		t.Fatalf("launch: calls=%d url=%q ref=%q", calls, gotURL, gotRef)
	}
}

func TestForgejoWebhook_HappyPath_GiteaHeaders(t *testing.T) {
	// Gitea spelling for both event + signature headers.
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-fj-2", nil
	}
	cfg, pt := fjConfig(t, s)
	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjOpenPR, "X-Gitea-Event", prforge.EventHeaderPullRequest, pt, "X-Gitea-Signature"))
	if w.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("gitea headers: code=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
}

func TestForgejoWebhook_BadHMAC(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("must not launch")
		return "", nil
	}
	cfg, _ := fjConfig(t, s)
	w := httptest.NewRecorder()
	s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjOpenPR, "X-Forgejo-Event", prforge.EventHeaderPullRequest, "iwh_wrong", "X-Forgejo-Signature"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad hmac: %d", w.Code)
	}
}

func TestForgejoWebhook_IdempotentReplay(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-fj-3", nil
	}
	cfg, pt := fjConfig(t, s)
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		s.handleForgejoWebhook(w, fjReq(gitlabCtx(cfg), fjOpenPR, "X-Forgejo-Event", prforge.EventHeaderPullRequest, pt, "X-Forgejo-Signature"))
		if i == 0 && w.Code != http.StatusAccepted {
			t.Fatalf("first: %d", w.Code)
		}
		if i == 1 {
			if w.Code != http.StatusOK {
				t.Fatalf("replay: %d", w.Code)
			}
			var resp map[string]string
			json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["status"] != webhooks.StatusDuplicate {
				t.Fatalf("replay status: %v", resp)
			}
		}
	}
	if calls != 1 {
		t.Fatalf("expected one launch, got %d", calls)
	}
}
