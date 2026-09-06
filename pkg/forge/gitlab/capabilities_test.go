package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// TestListIssues_StateNormalizationAndEncoding pins the project-path
// URL-encoding ("group/sub/api" → %2F), the state-query mapping
// (open→opened), and the GitLab→forge state normalization (opened→open).
func TestListIssues_StateNormalizationAndEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/group%2Fsub%2Fapi/issues") {
			t.Errorf("escaped path = %q, want namespaced project id", got)
		}
		if r.URL.Query().Get("state") != "opened" {
			t.Errorf("state = %q, want opened", r.URL.Query().Get("state"))
		}
		if r.URL.Query().Get("labels") != "bug,urgent" {
			t.Errorf("labels = %q, want bug,urgent", r.URL.Query().Get("labels"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"iid": 6, "title": "boom", "description": "body text", "state": "opened",
				"web_url": "https://gl/group/sub/api/-/issues/6", "labels": []string{"bug"},
				"author":    map[string]any{"username": "alice"},
				"assignees": []map[string]any{{"username": "bob"}},
			},
		})
	}))
	defer srv.Close()

	issues, err := New(srv.Client(), srv.URL, "tok").ListIssues(context.Background(), "group/sub/api", forge.IssueListOptions{
		State:  "open",
		Labels: []string{"bug", "urgent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %d", len(issues))
	}
	i := issues[0]
	if i.Number != 6 {
		t.Errorf("number = %d, want 6 (iid)", i.Number)
	}
	if i.State != "open" {
		t.Errorf("state = %q, want open (normalized from opened)", i.State)
	}
	if i.Body != "body text" {
		t.Errorf("body = %q, want body text (from description)", i.Body)
	}
	if i.Author != "alice" {
		t.Errorf("author = %q", i.Author)
	}
	if len(i.Assignees) != 1 || i.Assignees[0] != "bob" {
		t.Errorf("assignees = %v", i.Assignees)
	}
}

// TestCreateIssue_RoundTrip pins the write shape: title/description and the
// comma-joined labels CSV, and that the iid+opened response round-trips back.
func TestCreateIssue_RoundTrip(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid": 11, "title": gotBody["title"], "description": gotBody["description"],
			"state": "opened", "web_url": "https://gl/g/p/-/issues/11",
		})
	}))
	defer srv.Close()

	got, err := New(srv.Client(), srv.URL, "tok").CreateIssue(context.Background(), "g/p", forge.NewIssue{
		Title:  "new bug",
		Body:   "describe it",
		Labels: []string{"bug", "p1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["title"] != "new bug" {
		t.Errorf("title body = %v", gotBody["title"])
	}
	if gotBody["description"] != "describe it" {
		t.Errorf("description body = %v", gotBody["description"])
	}
	if gotBody["labels"] != "bug,p1" {
		t.Errorf("labels body = %v, want comma-joined CSV", gotBody["labels"])
	}
	if got.Number != 11 || got.State != "open" {
		t.Errorf("round-trip ref = %+v", got)
	}
}

// TestUpdateIssue_StateEvent pins the state→state_event mapping (closed→close)
// and that other patch fields ride along.
func TestUpdateIssue_StateEvent(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/g%2Fp/issues/6") {
			t.Errorf("escaped path = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid": 6, "title": gotBody["title"], "state": "closed", "web_url": "u",
		})
	}))
	defer srv.Close()

	title := "renamed"
	state := "closed"
	got, err := New(srv.Client(), srv.URL, "tok").UpdateIssue(context.Background(), "g/p", 6, forge.IssuePatch{
		Title: &title,
		State: &state,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["state_event"] != "close" {
		t.Errorf("state_event = %v, want close", gotBody["state_event"])
	}
	if gotBody["title"] != "renamed" {
		t.Errorf("title = %v", gotBody["title"])
	}
	if got.State != "closed" {
		t.Errorf("returned state = %q", got.State)
	}
}

// TestUpdateIssue_Reopen pins the open→reopen mapping.
func TestUpdateIssue_Reopen(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"iid": 6, "state": "opened", "web_url": "u"})
	}))
	defer srv.Close()
	state := "open"
	_, err := New(srv.Client(), srv.URL, "tok").UpdateIssue(context.Background(), "g/p", 6, forge.IssuePatch{State: &state})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["state_event"] != "reopen" {
		t.Errorf("state_event = %v, want reopen", gotBody["state_event"])
	}
}

func TestGetIssue_404IsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "tok").GetIssue(context.Background(), "g/p", 99)
	if !errors.Is(err, forge.ErrHookNotFound) {
		t.Errorf("get 404 = %v, want ErrHookNotFound", err)
	}
}

// TestListPullRequests_Mapping pins MR state normalization (opened→open),
// draft (work_in_progress→Draft), head SHA, branches, and LinkedIssues parse.
func TestListPullRequests_Mapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/g%2Fp/merge_requests") {
			t.Errorf("escaped path = %q", got)
		}
		if r.URL.Query().Get("state") != "opened" {
			t.Errorf("state = %q, want opened", r.URL.Query().Get("state"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"iid": 42, "title": "fix the thing closes #12 and #7", "state": "opened",
				"web_url": "https://gl/g/p/-/merge_requests/42", "source_branch": "feat", "target_branch": "main",
				"sha": "abc123", "work_in_progress": true, "draft": false,
				"author": map[string]any{"username": "carol"},
			},
		})
	}))
	defer srv.Close()

	prs, err := New(srv.Client(), srv.URL, "tok").ListPullRequests(context.Background(), "g/p", forge.PullListOptions{State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("prs = %d", len(prs))
	}
	p := prs[0]
	if p.Number != 42 || p.State != "open" {
		t.Errorf("number/state = %d/%q", p.Number, p.State)
	}
	if p.HeadSHA != "abc123" || p.SourceBranch != "feat" || p.TargetBranch != "main" {
		t.Errorf("branches/sha = %+v", p)
	}
	if !p.Draft {
		t.Errorf("draft = false, want true (work_in_progress)")
	}
	if len(p.LinkedIssues) != 2 || p.LinkedIssues[0] != 12 || p.LinkedIssues[1] != 7 {
		t.Errorf("linked issues = %v, want [12 7]", p.LinkedIssues)
	}
}

// TestPullRequest_HeadRepo pins the head-repo identity every same-repo-only
// lane (auto-fix, gate relaunch, /command) reads through
// forge.PullRef.SameRepoAs. GitLab's MR payload carries the source and
// target PROJECT IDS and never the source project's path: equal ids mean the
// head branch lives in the project the caller addressed the MR under, so
// that reference IS the head repo; a fork MR (differing ids) has its source
// project resolved by id, so the head is the fork's own path — never the
// addressed project; a source project the token cannot see, and a payload
// without the ids, leave it empty — not proven same-repo, which those lanes
// refuse. Both read paths (get + list) must agree.
func TestPullRequest_HeadRepo(t *testing.T) {
	cases := []struct {
		name     string
		ids      map[string]any
		wantHead string
		wantSame bool
	}{
		{"same project: the addressed project is the head repo", map[string]any{"source_project_id": 3, "target_project_id": 3}, "g/p", true},
		{"fork MR: the source project resolves to the fork's own path", map[string]any{"source_project_id": 5, "target_project_id": 3}, "alice/p", false},
		{"fork MR: a source project the token cannot see stays unproven", map[string]any{"source_project_id": 7, "target_project_id": 3}, "", false},
		{"payload without project ids proves nothing", map[string]any{}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mr := map[string]any{
				"iid": 42, "title": "t", "state": "opened", "web_url": "https://gl/g/p/-/merge_requests/42",
				"source_branch": "feat", "target_branch": "main", "sha": "abc123",
				"author": map[string]any{"username": "carol"},
			}
			for k, v := range c.ids {
				mr[k] = v
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch p := r.URL.EscapedPath(); {
				case strings.HasSuffix(p, "/projects/g%2Fp/merge_requests/42"):
					_ = json.NewEncoder(w).Encode(mr)
				case strings.HasSuffix(p, "/projects/5"):
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "path_with_namespace": "alice/p", "http_url_to_repo": "https://gl/alice/p.git"})
				case strings.HasSuffix(p, "/projects/7"):
					w.WriteHeader(http.StatusNotFound)
				default:
					_ = json.NewEncoder(w).Encode([]map[string]any{mr})
				}
			}))
			defer srv.Close()
			c1 := New(srv.Client(), srv.URL, "tok")

			got, err := c1.GetPullRequest(context.Background(), "g/p", 42)
			if err != nil {
				t.Fatal(err)
			}
			if got.HeadRepoFullName != c.wantHead {
				t.Errorf("GetPullRequest HeadRepoFullName = %q, want %q", got.HeadRepoFullName, c.wantHead)
			}
			if got.SameRepoAs("g/p") != c.wantSame {
				t.Errorf("SameRepoAs(g/p) = %v with head %q — the same-repo-only lanes would decide wrongly", got.SameRepoAs("g/p"), got.HeadRepoFullName)
			}
			list, err := c1.ListPullRequests(context.Background(), "g/p", forge.PullListOptions{})
			if err != nil || len(list) != 1 {
				t.Fatalf("list = %v, %v", list, err)
			}
			if list[0].HeadRepoFullName != c.wantHead {
				t.Errorf("ListPullRequests HeadRepoFullName = %q, want %q", list[0].HeadRepoFullName, c.wantHead)
			}
		})
	}
}

// TestGetCIStatus_Aggregation pins per-job status normalization (canceled→
// cancelled) and the worst-wins aggregate (a failed job → failed state).
func TestGetCIStatus_Aggregation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/repository/commits/abc123/statuses") {
			t.Errorf("escaped path = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "build", "status": "success", "sha": "abc123", "target_url": "https://gl/build"},
			{"name": "test", "status": "failed", "sha": "abc123", "target_url": "https://gl/test"},
			{"name": "lint", "status": "canceled", "sha": "abc123"},
		})
	}))
	defer srv.Close()

	st, err := New(srv.Client(), srv.URL, "tok").GetCIStatus(context.Background(), "g/p", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA != "abc123" {
		t.Errorf("sha = %q", st.SHA)
	}
	if st.State != forge.CIFailed {
		t.Errorf("aggregate state = %q, want failed", st.State)
	}
	if len(st.Runs) != 3 {
		t.Fatalf("runs = %d", len(st.Runs))
	}
	if st.Runs[2].Status != forge.CICancelled {
		t.Errorf("lint status = %q, want cancelled (from canceled)", st.Runs[2].Status)
	}
}

// TestGetCIStatus_AllSuccess pins the all-success aggregate.
func TestGetCIStatus_AllSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "build", "status": "success", "sha": "ddd"},
			{"name": "test", "status": "success", "sha": "ddd"},
		})
	}))
	defer srv.Close()
	st, err := New(srv.Client(), srv.URL, "tok").GetCIStatus(context.Background(), "g/p", "ddd")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != forge.CISuccess {
		t.Errorf("state = %q, want success", st.State)
	}
}

// TestListCIHistory_Pipelines pins one CIRun per pipeline, newest-first query
// params, and the per-pipeline naming.
func TestListCIHistory_Pipelines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("ref = %q", r.URL.Query().Get("ref"))
		}
		if r.URL.Query().Get("per_page") != "5" {
			t.Errorf("per_page = %q, want 5", r.URL.Query().Get("per_page"))
		}
		if r.URL.Query().Get("sort") != "desc" {
			t.Errorf("sort = %q, want desc", r.URL.Query().Get("sort"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 48, "status": "running", "ref": "main", "sha": "eee", "web_url": "https://gl/p/48"},
			{"id": 47, "status": "success", "ref": "main", "sha": "ddd", "web_url": "https://gl/p/47"},
		})
	}))
	defer srv.Close()

	runs, err := New(srv.Client(), srv.URL, "tok").ListCIHistory(context.Background(), "g/p", "main", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	if runs[0].Name != "pipeline #48" || runs[0].Status != forge.CIRunning {
		t.Errorf("run[0] = %+v", runs[0])
	}
	if runs[1].Name != "pipeline #47" || runs[1].Status != forge.CISuccess {
		t.Errorf("run[1] = %+v", runs[1])
	}
}

func TestListIssues_401IsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "tok").ListIssues(context.Background(), "g/p", forge.IssueListOptions{})
	if !errors.Is(err, forge.ErrUnauthorized) {
		t.Errorf("list 401 = %v, want ErrUnauthorized", err)
	}
}

// Compile-time assertions that AdminClient satisfies the optional capability
// interfaces (also declared next to each impl, asserted here for visibility).
var (
	_ forge.IssueClient = (*AdminClient)(nil)
	_ forge.PullClient  = (*AdminClient)(nil)
)
