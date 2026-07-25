package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
)

// glLabeledIssue is an "Issue Hook" adding the "implement" label (changes.labels
// previous→current diff) on an open issue.
const glLabeledIssue = `{
  "object_kind": "issue",
  "user": {"id": 9, "username": "maintainer-bob"},
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git", "default_branch": "main"},
  "object_attributes": {"iid": 42, "title": "Add CSV export", "description": "as CSV", "state": "opened", "action": "update", "url": "https://gitlab.com/acme/widgets/-/issues/42"},
  "labels": [{"title": "implement"}],
  "changes": {"labels": {"previous": [], "current": [{"title": "implement"}]}}
}`

// glIssueConfig is a GitLab webhook pinned to featurly + scoped to "implement".
func glIssueConfig() webhooks.Config {
	cfg := glConfig()
	cfg.BotIDs = []string{"feature-dev"}
	cfg.LabelAllowlist = []string{"implement"}
	return cfg
}

func TestGitLabWebhook_IssueLabeled_HappyPath(t *testing.T) {
	s := newWebhookTestServer(t)
	var calls int
	var gotBot, gotURL, gotRef string
	var gotVars map[string]string
	s.webhookLaunchBot = func(_ context.Context, botID string, vars map[string]string, repoURL, repoRef, _ string, _, _ map[string]string) (string, error) {
		calls++
		gotBot, gotVars, gotURL, gotRef = botID, vars, repoURL, repoRef
		return "run-gl42", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(glIssueConfig()), glLabeledIssue, gitlab.EventHeaderIssue))
	if w.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusLaunched || resp["run_id"] != "run-gl42" {
		t.Fatalf("resp: %v", resp)
	}
	if calls != 1 || gotBot != "feature-dev" {
		t.Fatalf("launch: calls=%d bot=%q", calls, gotBot)
	}
	if gotVars["feature_prompt"] != "Add CSV export\n\nas CSV" {
		t.Fatalf("feature_prompt: %q", gotVars["feature_prompt"])
	}
	if gotVars["open_mr"] != "true" || gotVars["source_issue_ref"] != "https://gitlab.com/acme/widgets/-/issues/42" {
		t.Fatalf("mr vars: %v", gotVars)
	}
	// Issue carries no MR branch → clone url + default branch as base.
	if gotURL != "https://gitlab.com/acme/widgets.git" || gotRef != "main" {
		t.Fatalf("repo: url=%q ref=%q", gotURL, gotRef)
	}
}

func TestGitLabWebhook_IssueWrongLabelFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a label outside the allowlist must not launch")
		return "", nil
	}
	body := strings.ReplaceAll(glLabeledIssue, `"title": "implement"`, `"title": "question"`)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(glIssueConfig()), body, gitlab.EventHeaderIssue))
	assertFiltered(t, w)
}

// A non-label update (no labels diff) must not launch.
func TestGitLabWebhook_IssueNonLabelUpdateFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a non-label issue update must not launch")
		return "", nil
	}
	body := `{"object_kind":"issue","user":{"id":9,"username":"bob"},
	  "project":{"id":42,"path_with_namespace":"acme/widgets","git_http_url":"https://gitlab.com/acme/widgets.git","default_branch":"main"},
	  "object_attributes":{"iid":42,"title":"t","state":"opened","action":"update","url":"https://gitlab.com/acme/widgets/-/issues/42"},
	  "changes":{"title":{"previous":"a","current":"t"}}}`
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(glIssueConfig()), body, gitlab.EventHeaderIssue))
	assertFiltered(t, w)
}

// A label added to a CLOSED issue must not launch.
func TestGitLabWebhook_IssueClosedFiltered(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		t.Fatal("a label on a closed issue must not launch")
		return "", nil
	}
	body := strings.Replace(glLabeledIssue, `"state": "opened"`, `"state": "closed"`, 1)
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(glIssueConfig()), body, gitlab.EventHeaderIssue))
	assertFiltered(t, w)
}

func assertFiltered(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 filtered, code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != webhooks.StatusFiltered {
		t.Fatalf("expected filtered, got %v", resp)
	}
}
