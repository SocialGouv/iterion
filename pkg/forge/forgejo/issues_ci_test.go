package forgejo

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

// Capability interfaces are implemented (compile-time assertions also live in
// issues.go / ci.go; restated here so a test reader sees the contract).
var (
	_ forge.IssueClient = (*AdminClient)(nil)
	_ forge.PullClient  = (*AdminClient)(nil)
)

func TestForgejoListIssues_FiltersPRsAndAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok" {
			t.Errorf("auth scheme = %q, want token tok", r.Header.Get("Authorization"))
		}
		if !strings.HasSuffix(r.URL.Path, "/api/v1/repos/org/api/issues") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Errorf("state = %q, want open", got)
		}
		if got := r.URL.Query().Get("labels"); got != "bug,urgent" {
			t.Errorf("labels = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"number":   7,
				"title":    "a real issue",
				"state":    "open",
				"html_url": "https://fj/org/api/issues/7",
				"user":     map[string]any{"login": "alice"},
				"labels":   []map[string]any{{"name": "bug"}},
				"assignees": []map[string]any{
					{"login": "bob"},
				},
			},
			{
				"number":       8,
				"title":        "a pull request",
				"state":        "open",
				"html_url":     "https://fj/org/api/pulls/8",
				"pull_request": map[string]any{"merged": false, "html_url": "https://fj/org/api/pulls/8"},
			},
		})
	}))
	defer srv.Close()

	c := New(srv.Client(), srv.URL, "tok")
	issues, err := c.ListIssues(context.Background(), "org/api", forge.IssueListOptions{
		Labels: []string{"bug", "urgent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2", len(issues))
	}
	if issues[0].IsPullRequest {
		t.Errorf("issue #7 flagged as PR")
	}
	if issues[0].Author != "alice" || len(issues[0].Labels) != 1 || issues[0].Labels[0] != "bug" {
		t.Errorf("issue #7 = %+v", issues[0])
	}
	if len(issues[0].Assignees) != 1 || issues[0].Assignees[0] != "bob" {
		t.Errorf("issue #7 assignees = %v", issues[0].Assignees)
	}
	if !issues[1].IsPullRequest {
		t.Errorf("issue #8 (PR) not flagged as PR")
	}
}

func TestForgejoGetIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/issues/12") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 12, "title": "t", "state": "closed", "html_url": "u",
		})
	}))
	defer srv.Close()
	ref, err := New(srv.Client(), srv.URL, "x").GetIssue(context.Background(), "o/r", 12)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Number != 12 || ref.State != "closed" {
		t.Errorf("ref = %+v", ref)
	}
}

func TestForgejoCreateIssue_BodyAndAssignees(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 99, "title": "new", "state": "open", "html_url": "https://fj/o/r/issues/99",
		})
	}))
	defer srv.Close()

	ref, err := New(srv.Client(), srv.URL, "x").CreateIssue(context.Background(), "o/r", forge.NewIssue{
		Title:     "new",
		Body:      "details",
		Labels:    []string{"bug"}, // intentionally NOT sent (Gitea labels = []int64 IDs)
		Assignees: []string{"carol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Number != 99 {
		t.Errorf("ref = %+v", ref)
	}
	if got["title"] != "new" || got["body"] != "details" {
		t.Errorf("body = %+v", got)
	}
	if _, present := got["labels"]; present {
		t.Errorf("labels must be omitted on create (need int64 IDs), got %+v", got["labels"])
	}
	asg, _ := got["assignees"].([]any)
	if len(asg) != 1 || asg[0] != "carol" {
		t.Errorf("assignees = %v", got["assignees"])
	}
}

func TestForgejoUpdateIssue_PartialPatch(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || !strings.HasSuffix(r.URL.Path, "/issues/5") {
			t.Errorf("method/path = %s %q", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 5, "title": "t", "state": "closed", "html_url": "u",
		})
	}))
	defer srv.Close()

	state := "closed"
	ref, err := New(srv.Client(), srv.URL, "x").UpdateIssue(context.Background(), "o/r", 5, forge.IssuePatch{
		State: &state,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.State != "closed" {
		t.Errorf("ref = %+v", ref)
	}
	if got["state"] != "closed" {
		t.Errorf("patch body = %+v", got)
	}
	if _, present := got["title"]; present {
		t.Errorf("nil patch fields must be omitted, got title=%v", got["title"])
	}
}

func TestForgejoListPullRequests_MergedAndLinkedIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/pulls") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"number":   3,
				"title":    "fix thing (closes #12)",
				"body":     "also relates to #7 and #7",
				"state":    "closed",
				"merged":   true,
				"draft":    false,
				"html_url": "https://fj/o/r/pulls/3",
				"user":     map[string]any{"login": "dev"},
				"head":     map[string]any{"ref": "feat/x", "sha": "abc123"},
				"base":     map[string]any{"ref": "main", "sha": "def456"},
			},
		})
	}))
	defer srv.Close()

	pulls, err := New(srv.Client(), srv.URL, "x").ListPullRequests(context.Background(), "o/r", forge.PullListOptions{State: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pulls) != 1 {
		t.Fatalf("len = %d", len(pulls))
	}
	p := pulls[0]
	if p.State != "merged" {
		t.Errorf("state = %q, want merged", p.State)
	}
	if p.SourceBranch != "feat/x" || p.TargetBranch != "main" || p.HeadSHA != "abc123" {
		t.Errorf("branches = %+v", p)
	}
	if len(p.LinkedIssues) != 2 || p.LinkedIssues[0] != 12 || p.LinkedIssues[1] != 7 {
		t.Errorf("linked issues = %v, want [12 7] (deduped)", p.LinkedIssues)
	}
}

func TestForgejoGetPullRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pulls/4") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 4, "title": "t", "state": "open", "html_url": "u",
			"head": map[string]any{"ref": "b", "sha": "s"},
		})
	}))
	defer srv.Close()
	p, err := New(srv.Client(), srv.URL, "x").GetPullRequest(context.Background(), "o/r", 4)
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != 4 || p.State != "open" || p.HeadSHA != "s" {
		t.Errorf("pull = %+v", p)
	}
}

func TestForgejoGetCIStatus_CombinedAggregation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/commits/main/status") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"state": "failure",
			"sha":   "deadbeef",
			"statuses": []map[string]any{
				{"status": "success", "context": "build", "target_url": "https://ci/1"},
				{"status": "failure", "context": "test", "target_url": "https://ci/2"},
			},
		})
	}))
	defer srv.Close()

	st, err := New(srv.Client(), srv.URL, "x").GetCIStatus(context.Background(), "o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA != "deadbeef" {
		t.Errorf("sha = %q", st.SHA)
	}
	if st.State != forge.CIFailed {
		t.Errorf("aggregate state = %q, want %q (failure→failed)", st.State, forge.CIFailed)
	}
	if len(st.Runs) != 2 {
		t.Fatalf("runs = %d", len(st.Runs))
	}
	if st.Runs[0].Name != "build" || st.Runs[0].Status != forge.CISuccess {
		t.Errorf("run[0] = %+v", st.Runs[0])
	}
	if st.Runs[1].Status != forge.CIFailed || st.Runs[1].SHA != "deadbeef" {
		t.Errorf("run[1] = %+v", st.Runs[1])
	}
}

func TestForgejoListCIHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/commits/main/statuses") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("limit"); got != "5" {
			t.Errorf("limit = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"status": "success", "context": "build"},
			{"status": "pending", "context": "deploy"},
		})
	}))
	defer srv.Close()

	runs, err := New(srv.Client(), srv.URL, "x").ListCIHistory(context.Background(), "o/r", "main", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Status != forge.CISuccess || runs[1].Status != forge.CIRunning && runs[1].Status != forge.CIPending {
		t.Errorf("runs = %+v", runs)
	}
}

func TestForgejoIssueErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "x").GetIssue(context.Background(), "o/r", 1)
	if !errors.Is(err, forge.ErrUnauthorized) {
		t.Errorf("401 → %v, want ErrUnauthorized", err)
	}

	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv404.Close()
	_, err = New(srv404.Client(), srv404.URL, "x").GetPullRequest(context.Background(), "o/r", 1)
	if !errors.Is(err, forge.ErrHookNotFound) {
		t.Errorf("404 → %v, want ErrHookNotFound", err)
	}
}
