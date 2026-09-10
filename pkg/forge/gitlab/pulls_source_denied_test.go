package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// deniedSourceForge serves one target project (42) whose merge requests come
// from three source projects the credential is answered differently about:
// 9 resolves, 13 is 404 (gone), 17 is 403 (a permission answer).
func deniedSourceForge(t *testing.T) *httptest.Server {
	t.Helper()
	mr := func(iid int, source int64) map[string]any {
		return map[string]any{
			"iid": iid, "state": "opened", "title": "t",
			"web_url":       "https://gitlab.example/acme/widgets/-/merge_requests/" + strconv.Itoa(iid),
			"source_branch": "feature/x", "target_branch": "main", "sha": "abc" + strconv.Itoa(iid),
			"source_project_id": source, "target_project_id": 42,
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.EscapedPath()
		switch {
		case strings.HasSuffix(p, "/projects/acme%2Fwidgets/merge_requests/17"):
			_ = json.NewEncoder(w).Encode(mr(17, 17))
		case strings.HasSuffix(p, "/projects/acme%2Fwidgets/merge_requests/13"):
			_ = json.NewEncoder(w).Encode(mr(13, 13))
		case strings.HasSuffix(p, "/projects/acme%2Fwidgets/merge_requests/8"):
			_ = json.NewEncoder(w).Encode(mr(8, 42))
		case p == "/api/v4/projects/17":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "403 Forbidden"})
		case p == "/api/v4/projects/13":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "404 Project Not Found"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A fork MR whose source project the credential is REFUSED (403) must not
// hand back a head that reads like the base project's. It stays unnamed, it
// says the forge declared one, and it carries the forge's own refusal typed
// — a permission answer, not "the project is gone" — so the lane that
// refuses can say which of the two it met.
func TestGitLabForkSourceProject403IsATypedPermissionAnswer(t *testing.T) {
	srv := deniedSourceForge(t)
	pr, err := New(srv.Client(), srv.URL, "tok").GetPullRequest(context.Background(), "acme/widgets", 17)
	if err != nil {
		t.Fatalf("a refused source project must not fail the MR read: %v", err)
	}
	if pr.HeadRepoFullName != "" || pr.HeadCloneURL != "" {
		t.Fatalf("head = %q clone = %q, want neither named — a head the credential may not read is NOT the base project",
			pr.HeadRepoFullName, pr.HeadCloneURL)
	}
	if pr.SameRepoAs("acme/widgets") {
		t.Fatal("a fork MR with an unreadable source project must never read as same-project")
	}
	if !pr.HeadRepoDeclared || !pr.HeadRepoWithheld() {
		t.Fatalf("declared = %v withheld = %v, want both: the MR names a source project id the read could not resolve",
			pr.HeadRepoDeclared, pr.HeadRepoWithheld())
	}
	if !errors.Is(pr.HeadRepoErr, forge.ErrForbidden) {
		t.Fatalf("HeadRepoErr = %v, want a typed ErrForbidden so a caller refuses on the permission answer instead of guessing", pr.HeadRepoErr)
	}
	if !strings.Contains(pr.HeadRepoErr.Error(), "source project") {
		t.Errorf("HeadRepoErr = %v, want the operation named (the per-operation error vocabulary)", pr.HeadRepoErr)
	}
}

// A source project that is GONE answers 404. Same unproven head, but the
// refusal is typed as an absence, not a permission.
func TestGitLabForkSourceProject404IsATypedAbsence(t *testing.T) {
	srv := deniedSourceForge(t)
	pr, err := New(srv.Client(), srv.URL, "tok").GetPullRequest(context.Background(), "acme/widgets", 13)
	if err != nil {
		t.Fatalf("a missing source project must not fail the MR read: %v", err)
	}
	if pr.HeadRepoFullName != "" || pr.SameRepoAs("acme/widgets") {
		t.Fatalf("head = %q, want unproven", pr.HeadRepoFullName)
	}
	if !pr.HeadRepoWithheld() {
		t.Fatal("a declared source project that could not be read is withheld, not absent")
	}
	if !errors.Is(pr.HeadRepoErr, forge.ErrNotFound) || errors.Is(pr.HeadRepoErr, forge.ErrForbidden) {
		t.Fatalf("HeadRepoErr = %v, want a typed 404 and NOT a permission answer", pr.HeadRepoErr)
	}
}

// A same-project MR declares its head and proves it: nothing is withheld.
func TestGitLabSameProjectMRDeclaresAProvenHead(t *testing.T) {
	srv := deniedSourceForge(t)
	pr, err := New(srv.Client(), srv.URL, "tok").GetPullRequest(context.Background(), "acme/widgets", 8)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.HeadRepoDeclared || pr.HeadRepoWithheld() || !pr.SameRepoAs("acme/widgets") {
		t.Fatalf("same-project MR = declared %v withheld %v same %v, want declared, not withheld, same-project",
			pr.HeadRepoDeclared, pr.HeadRepoWithheld(), pr.SameRepoAs("acme/widgets"))
	}
	if pr.HeadRepoErr != nil {
		t.Errorf("HeadRepoErr = %v, want none: nothing was refused", pr.HeadRepoErr)
	}
}
