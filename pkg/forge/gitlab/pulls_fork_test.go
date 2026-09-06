package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// forkForge is a fake GitLab serving one target project's merge requests —
// two from the same fork (source project 9), one from a private fork the
// token cannot see (13), one same-project — and the source-project lookup,
// counting the lookups.
func forkForge(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var lookups int32
	mr := func(iid int, branch string, source int64) map[string]any {
		return map[string]any{
			"iid": iid, "state": "opened", "title": "t", "web_url": "https://gitlab.example/acme/widgets/-/merge_requests/" + strconv.Itoa(iid),
			"source_branch": branch, "target_branch": "main", "sha": "abc" + strconv.Itoa(iid),
			"source_project_id": source, "target_project_id": 42,
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.EscapedPath()
		switch {
		case strings.HasSuffix(p, "/projects/acme%2Fwidgets/merge_requests/7"):
			_ = json.NewEncoder(w).Encode(mr(7, "feature/x", 9))
		case strings.HasSuffix(p, "/projects/acme%2Fwidgets/merge_requests/8"):
			_ = json.NewEncoder(w).Encode(mr(8, "feature/y", 42))
		case strings.HasSuffix(p, "/projects/acme%2Fwidgets/merge_requests/11"):
			_ = json.NewEncoder(w).Encode(mr(11, "feature/z", 13))
		case strings.HasSuffix(p, "/projects/acme%2Fwidgets/merge_requests"):
			_ = json.NewEncoder(w).Encode([]map[string]any{mr(7, "feature/x", 9), mr(9, "feature/w", 9), mr(8, "feature/y", 42)})
		case p == "/api/v4/projects/9":
			atomic.AddInt32(&lookups, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "path_with_namespace": "mallory/widgets", "http_url_to_repo": "https://gitlab.example/mallory/widgets.git"})
		case p == "/api/v4/projects/13":
			atomic.AddInt32(&lookups, 1)
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "404 Project Not Found"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &lookups
}

func heads(list []forge.PullRef) []string {
	out := make([]string, 0, len(list))
	for _, pr := range list {
		out = append(out, pr.HeadRepoFullName)
	}
	return out
}

// GitLab's MR object names the source project by id only; a fork MR left
// HeadRepoFullName empty, which every same-project lane read as "not
// proven" and refused without being able to say what it refused. The source
// project is one call away: GET /projects/:id gives its path and clone URL,
// cached per instance and id, so the guards compare real names and a
// refusal names the fork.
func TestGitLabGetPullRequestResolvesTheForkSourceProject(t *testing.T) {
	srv, lookups := forkForge(t)
	ctx := context.Background()
	c := New(srv.Client(), srv.URL, "tok")
	pr, err := c.GetPullRequest(ctx, "acme/widgets", 7)
	if err != nil {
		t.Fatal(err)
	}
	if pr.HeadRepoFullName != "mallory/widgets" || pr.HeadCloneURL != "https://gitlab.example/mallory/widgets.git" {
		t.Fatalf("fork MR head = %q clone=%q, want the source project resolved by id", pr.HeadRepoFullName, pr.HeadCloneURL)
	}
	if pr.SameRepoAs("acme/widgets") {
		t.Fatal("a fork MR must not read as same-project once its head is named")
	}
	// Cached per (instance, project id): a second read, and another client on
	// the same instance, do not look the project up again.
	if _, err := c.GetPullRequest(ctx, "acme/widgets", 7); err != nil {
		t.Fatal(err)
	}
	if _, err := New(srv.Client(), srv.URL, "tok2").GetPullRequest(ctx, "acme/widgets", 7); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(lookups); n != 1 {
		t.Errorf("source project looked up %d times over three reads, want 1 (cached)", n)
	}
	// A same-project MR needs no lookup: the head is the project queried.
	same, err := c.GetPullRequest(ctx, "acme/widgets", 8)
	if err != nil {
		t.Fatal(err)
	}
	if same.HeadRepoFullName != "acme/widgets" || !same.SameRepoAs("acme/widgets") || same.HeadCloneURL != "" {
		t.Errorf("same-project MR = %+v, want the queried project as head and no clone URL of its own", same)
	}
	if n := atomic.LoadInt32(lookups); n != 1 {
		t.Errorf("a same-project MR must not look its own project up, lookups = %d", n)
	}
	// A listing resolves each distinct fork once (here from the cache).
	list, err := c.ListPullRequests(ctx, "acme/widgets", forge.PullListOptions{State: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].HeadRepoFullName != "mallory/widgets" || list[1].HeadRepoFullName != "mallory/widgets" || list[2].HeadRepoFullName != "acme/widgets" {
		t.Errorf("listing heads = %v, want the two fork MRs named and the same-project one on its own project", heads(list))
	}
	if n := atomic.LoadInt32(lookups); n != 1 {
		t.Errorf("lookups after the listing = %d, want still 1", n)
	}
}

// A source project the token cannot see (a private fork, a deleted one)
// answers 404: the head stays UNPROVEN — every same-project guard refuses,
// as it did — and the read itself does not fail, so the MR is still served
// to the lanes that only need its branches.
func TestGitLabForkSourceProjectTheTokenCannotSeeStaysUnproven(t *testing.T) {
	srv, _ := forkForge(t)
	pr, err := New(srv.Client(), srv.URL, "tok").GetPullRequest(context.Background(), "acme/widgets", 11)
	if err != nil {
		t.Fatalf("an invisible source project must not fail the MR read: %v", err)
	}
	if pr.HeadRepoFullName != "" || pr.HeadCloneURL != "" || pr.SameRepoAs("acme/widgets") {
		t.Errorf("MR = %+v, want an unproven head (empty) that every guard refuses", pr)
	}
	if pr.SourceBranch != "feature/z" || pr.HeadSHA != "abc11" {
		t.Errorf("the MR's own fields must still be served: %+v", pr)
	}
}
