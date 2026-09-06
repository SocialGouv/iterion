package gitlab

import (
	"strings"
	"testing"
)

// mrForkPayload is a merge_request event whose head branch lives in a FORK:
// GitLab names both projects by id and carries the source project's own
// path and clone URL under object_attributes.source.
const mrForkPayload = `{
  "object_kind": "merge_request",
  "project": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"},
  "object_attributes": {
    "iid": 11, "action": "open", "source_branch": "feature/fork", "target_branch": "main",
    "title": "From a fork", "url": "https://gitlab.com/acme/widgets/-/merge_requests/11",
    "last_commit": {"id": "forksha"},
    "source_project_id": 9, "target_project_id": 42,
    "source": {"id": 9, "path_with_namespace": "mallory/widgets", "git_http_url": "https://gitlab.com/mallory/widgets.git"},
    "target": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"}
  }
}`

// The MR event payload names the head project (source), so the fork guard
// on the MR lane decides from the payload alone — as the GitHub/Forgejo
// twin does from head.repo — and a refusal names the fork.
func TestParseMergeRequest_ForkSourceProject(t *testing.T) {
	p, err := ParseMergeRequest([]byte(mrForkPayload))
	if err != nil {
		t.Fatal(err)
	}
	if p.HeadRepoFullName != "mallory/widgets" || p.HeadCloneURL != "https://gitlab.com/mallory/widgets.git" {
		t.Fatalf("head = %q clone=%q, want the source project the payload names", p.HeadRepoFullName, p.HeadCloneURL)
	}
	if p.SourceProjectID != 9 || p.TargetProjectID != 42 {
		t.Fatalf("project ids = %d/%d, want 9/42", p.SourceProjectID, p.TargetProjectID)
	}
	if p.SameRepoAsBase() || !p.IsFork() {
		t.Fatalf("a payload naming two projects is a PROVEN fork: same=%v fork=%v", p.SameRepoAsBase(), p.IsFork())
	}

	same := strings.Replace(mrForkPayload, `"source_project_id": 9`, `"source_project_id": 42`, 1)
	same = strings.Replace(same, `"source": {"id": 9, "path_with_namespace": "mallory/widgets", "git_http_url": "https://gitlab.com/mallory/widgets.git"}`,
		`"source": {"id": 42, "path_with_namespace": "acme/widgets", "git_http_url": "https://gitlab.com/acme/widgets.git"}`, 1)
	q, err := ParseMergeRequest([]byte(same))
	if err != nil {
		t.Fatal(err)
	}
	if !q.SameRepoAsBase() || q.IsFork() || q.HeadRepoFullName != "acme/widgets" {
		t.Fatalf("a same-project payload must prove same-repo: %+v", q)
	}

	// A payload without the source (a legacy sender) proves nothing: not
	// same-repo, but not a fork either — unknown is unproven, not refused
	// as a fork.
	legacy, err := ParseMergeRequest([]byte(mrOpenPayload))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.HeadRepoFullName != "" || legacy.SameRepoAsBase() || legacy.IsFork() {
		t.Fatalf("a payload without source/target must stay unproven: %+v", legacy)
	}
}
