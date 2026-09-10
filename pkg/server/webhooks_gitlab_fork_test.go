package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
	"github.com/SocialGouv/iterion/pkg/webhooks/gitlab"
)

// glForkMR is an MR opened from a fork: the payload names the source project
// (mallory/widgets) and the target project (acme/widgets) by id and path.
const glForkMR = `{
  "object_kind": "merge_request",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "object_attributes": {"iid": 11, "action": "open", "source_branch": "feature/fork", "target_branch": "main",
    "title": "From a fork", "description": "d", "url": "https://gitlab.com/acme/widgets/-/merge_requests/11",
    "last_commit": {"id": "forksha"},
    "source_project_id": 9, "target_project_id": 42,
    "source": {"id": 9, "path_with_namespace": "mallory/widgets", "git_http_url": "https://gitlab.com/mallory/widgets.git"},
    "target": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"}}
}`

// glSameProjectMR is the same event shape for an MR whose head lives in the
// project itself (ids agree, source = the project).
const glSameProjectMR = `{
  "object_kind": "merge_request",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "object_attributes": {"iid": 12, "action": "open", "source_branch": "feature/own", "target_branch": "main",
    "title": "Own branch", "description": "d", "url": "https://gitlab.com/acme/widgets/-/merge_requests/12",
    "last_commit": {"id": "ownsha"},
    "source_project_id": 42, "target_project_id": 42,
    "source": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
    "target": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"}}
}`

func lastDeliveryReason(t *testing.T, s *Server) string {
	t.Helper()
	list, err := s.webhookDeliveries.ListByWebhook(context.Background(), "t1", "w1", 10)
	if err != nil || len(list) == 0 {
		t.Fatalf("deliveries: %v (%d)", err, len(list))
	}
	return list[0].Error
}

// The GitLab MR lane launched a fork MR on the launch pair (this project's
// clone URL + the fork's branch name), which names no repository: the
// checkout misses, or hits a same-named branch here and the bot reviews the
// wrong code under the bot's identity. The payload names the source project,
// so a PROVEN fork is refused here the way the GitHub/Forgejo lanes refuse
// one — and the refusal names the fork.
func TestGitLabWebhook_ForkMRNeverAutoLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-fork", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(glConfig()), glForkMR, gitlab.EventHeaderMergeRequest))
	if launched != 0 {
		t.Fatalf("a fork MR launched on the auto lane (code=%d body=%s): the launch pair names no repository", w.Code, w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("a fork MR is a benign refusal (200/filtered), got %d body=%s", w.Code, w.Body.String())
	}
	if reason := lastDeliveryReason(t, s); !strings.Contains(reason, "mallory/widgets") || !strings.Contains(reason, "fork") {
		t.Fatalf("the refusal must name the fork's own project, got %q", reason)
	}
}

// The guard reads the payload's project ids: a same-project MR that carries
// them launches exactly as one without them.
func TestGitLabWebhook_SameProjectMRWithProjectIDsStillLaunches(t *testing.T) {
	s := newWebhookTestServer(t)
	launched := 0
	var gotURL, gotRef string
	s.webhookLaunchBot = func(_ context.Context, _ string, _ map[string]string, repoURL, repoRef, _ string, _, _ map[string]string) (string, error) {
		launched++
		gotURL, gotRef = repoURL, repoRef
		return "run-own", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glReq(gitlabCtx(glConfig()), glSameProjectMR, gitlab.EventHeaderMergeRequest))
	if w.Code != http.StatusAccepted || launched != 1 {
		t.Fatalf("same-project MR must launch: code=%d launched=%d body=%s", w.Code, launched, w.Body.String())
	}
	if gotURL != "https://gitlab.com/acme/widgets.git" || gotRef != "feature/own" {
		t.Fatalf("launch pair = %q@%q, want the project's own clone URL and the MR branch", gotURL, gotRef)
	}
}

// The /command note lane already refused a fork MR (fail-closed on an
// unproven head). With the source project resolved by id, the refusal names
// the fork instead of "unverifiable".
func TestGitLabNoteHook_ForkRefusalNamesTheHead(t *testing.T) {
	s := newWebhookTestServer(t)
	s.webhookGitLabPRResolver = func(context.Context, webhooks.Config, gitlab.ParsedNote, string) (forge.PullRef, error) {
		return forge.PullRef{State: "open", SourceBranch: "feature/x", TargetBranch: "main", HeadRepoFullName: "mallory/widgets"}, nil
	}
	launched := 0
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		launched++
		return "run-forbidden", nil
	}
	w := httptest.NewRecorder()
	s.handleGitLabWebhook(w, glNoteReq(gitlabCtx(featurlyConfig()), glNoteFeaturly))
	if launched != 0 || w.Code != http.StatusOK {
		t.Fatalf("a fork MR command must filter: launched=%d code=%d body=%s", launched, w.Code, w.Body.String())
	}
	if reason := lastDeliveryReason(t, s); !strings.Contains(reason, "mallory/widgets") {
		t.Fatalf("the refusal must name the fork's own project, got %q", reason)
	}
}
