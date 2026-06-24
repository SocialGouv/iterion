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

// ghConfig returns a fresh hmac-mode GitHub Config seeded with a sealed
// plaintext + token mirror. The handler tests use it as a baseline.
func ghConfig(t *testing.T, s *Server) (webhooks.Config, string) {
	t.Helper()
	pt, hash, last4, fp, err := webhooks.MintToken()
	if err != nil {
		t.Fatal(err)
	}
	cfg := webhooks.Config{
		ID:           "ghw",
		TenantID:     "t1",
		Provider:     webhooks.ProviderGitHub,
		SignMode:     webhooks.SignModeHMAC,
		Enabled:      true,
		TokenHash:    hash,
		TokenLast4:   last4,
		Fingerprint:  fp,
		BotIDs:       []string{"review-pr"},
		KeyOverrides: map[string]string{},
	}
	sealed, err := webhooks.SealHMACSecret(s.sealer, cfg.ID, pt)
	if err != nil {
		t.Fatal(err)
	}
	cfg.HMACSecretSealed = sealed
	return cfg, pt
}

// ghReq builds a request with X-GitHub-Event + X-Hub-Signature-256
// (sha256= prefix matches GitHub's wire format).
func ghReq(ctx context.Context, body, event, pt string) *http.Request {
	r := httptest.NewRequest("POST", "/api/webhooks/github/ghw", strings.NewReader(body)).WithContext(ctx)
	if event != "" {
		r.Header.Set("X-GitHub-Event", event)
	}
	if pt != "" {
		mac := hmac.New(sha256.New, []byte(pt))
		mac.Write([]byte(body))
		r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	r.SetPathValue("id", "ghw")
	return r
}

func ghCtx(cfg webhooks.Config) context.Context {
	return gitlabCtx(cfg) // same identity-stamping, just a different provider.
}

const ghOpenPR = `{
  "action": "opened",
  "number": 7,
  "repository": {"id": 42, "full_name": "acme/widgets", "clone_url": "https://github.com/acme/widgets.git"},
  "pull_request": {"number": 7, "title": "Add X", "body": "desc",
    "html_url": "https://github.com/acme/widgets/pull/7", "state": "open",
    "head": {"ref": "feature/x", "sha": "abc123"}, "base": {"ref": "main"}},
  "sender": {"login": "alice"}
}`

func TestGitHubWebhook_HappyPath(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotBot, gotURL, gotRef string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, repoURL, repoRef, projectPath string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars, gotURL, gotRef = botID, vars, repoURL, repoRef
		return "run-7", nil
	}
	cfg, pt := ghConfig(t, s)

	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusLaunched || resp["run_id"] != "run-7" {
		t.Fatalf("resp: %v", resp)
	}
	if calls != 1 || gotBot != "review-pr" {
		t.Fatalf("launch: calls=%d bot=%q", calls, gotBot)
	}
	if gotVars["pr_url"] != "https://github.com/acme/widgets/pull/7" || gotVars["base_ref"] != "main" || gotVars["post_to_board"] != "false" {
		t.Fatalf("vars: %v", gotVars)
	}
	if gotURL != "https://github.com/acme/widgets.git" || gotRef != "feature/x" {
		t.Fatalf("repo: url=%q ref=%q", gotURL, gotRef)
	}
}

func TestGitHubWebhook_BadHMAC(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("launch must not be reached on bad signature")
		return "", nil
	}
	cfg, _ := ghConfig(t, s)
	w := httptest.NewRecorder()
	// Sign with a wrong key.
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, "iwh_wrong_key"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad hmac: code=%d body=%s", w.Code, w.Body.String())
	}
	// No delivery row written either (auth gate is strictly before audit).
	if list, _ := s.webhookDeliveries.ListByWebhook(context.Background(), "t1", "ghw", 10); len(list) != 0 {
		t.Fatalf("bad-hmac delivery should not be recorded, got %d rows", len(list))
	}
}

func TestGitHubWebhook_NonPullRequestEventFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("ping must not launch")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), `{"zen":"yo"}`, "ping", pt))
	if w.Code != http.StatusOK {
		t.Fatalf("ping: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("ping should be filtered, got %v", resp)
	}
}

func TestGitHubWebhook_SynchronizeFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("synchronize must not launch (auto-review on open only)")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	body := strings.Replace(ghOpenPR, `"action": "opened"`, `"action": "synchronize"`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), body, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("sync: code=%d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("sync should be filtered: %v", resp)
	}
}

func TestGitHubWebhook_ProjectAllowlistMismatch(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("mismatched repo must not launch")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.ProjectAllowlist = []string{"other/*"}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("mismatched repo: code=%d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("mismatched repo should be filtered: %v", resp)
	}
}

// A webhook scoped to dependency bots must ignore a human PR.
func TestGitHubWebhook_AuthorAllowlistFiltersHuman(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("human-authored PR must not launch when author-allowlisted to bots")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.AuthorAllowlist = []string{"dependabot[bot]", "renovate[bot]"}
	w := httptest.NewRecorder()
	// ghOpenPR's sender is "alice" (a human) → filtered.
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusOK {
		t.Fatalf("filtered author: code=%d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("human author should be filtered: %v", resp)
	}
}

// The same webhook launches on a Dependabot PR and stamps pr_author.
func TestGitHubWebhook_AuthorAllowlistLaunchesBot(t *testing.T) {
	s := newWebhookTestServer(t)
	var gotVars map[string]string
	var calls int
	s.webhookLaunchBot = func(_ context.Context, _ string, vars map[string]string, _, _, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotVars = vars
		return "run-dep", nil
	}
	cfg, pt := ghConfig(t, s)
	cfg.AuthorAllowlist = []string{"dependabot[bot]"}
	depPR := strings.Replace(ghOpenPR, `"sender": {"login": "alice"}`, `"sender": {"login": "dependabot[bot]"}`, 1)
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), depPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusAccepted {
		t.Fatalf("dependabot PR should launch: code=%d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Fatalf("expected one launch, got %d", calls)
	}
	if gotVars["pr_author"] != "dependabot[bot]" {
		t.Fatalf("pr_author not stamped: %v", gotVars)
	}
}

func TestGitHubWebhook_IdempotentReplay(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		calls++
		return "run-7", nil
	}
	cfg, pt := ghConfig(t, s)
	w1 := httptest.NewRecorder()
	s.handleGitHubWebhook(w1, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first: %d", w1.Code)
	}
	w2 := httptest.NewRecorder()
	s.handleGitHubWebhook(w2, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w2.Code != http.StatusOK {
		t.Fatalf("replay code=%d body=%s", w2.Code, w2.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusDuplicate || resp["run_id"] != "run-7" {
		t.Fatalf("duplicate resp: %v", resp)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one launch, got %d", calls)
	}
}

func TestGitHubWebhook_BotNotAllowed(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("disallowed bot must not launch")
		return "", nil
	}
	cfg, pt := ghConfig(t, s)
	// SelectBot() returns "" when there are multiple non-default bots
	// → handler falls back to defaultWebhookBotReviewPR. Pin two bots
	// that exclude review-pr so the AllowsBot gate fires.
	cfg.BotIDs = []string{"some-other-bot", "and-another"}
	w := httptest.NewRecorder()
	s.handleGitHubWebhook(w, ghReq(ghCtx(cfg), ghOpenPR, prforge.EventHeaderPullRequest, pt))
	if w.Code != http.StatusForbidden {
		t.Fatalf("bot not allowed: code=%d body=%s", w.Code, w.Body.String())
	}
}
