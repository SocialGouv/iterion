package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

var _ forge.PullClient = (*AdminClient)(nil)

func TestGitHubGetPullRequest_Mapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 5, "title": "feat: x (fixes #12, closes #7)", "state": "closed",
			"body":      "also references #12 again",
			"html_url":  "https://github.com/o/r/pull/5",
			"draft":     false,
			"merged_at": "2026-01-02T03:04:05Z",
			"head":      map[string]any{"ref": "feature", "sha": "deadbeef"},
			"base":      map[string]any{"ref": "main"},
			"user":      map[string]any{"login": "dave"},
		})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	pr, err := c.GetPullRequest(context.Background(), "o/r", 5)
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != "merged" {
		t.Errorf("merged_at present → state should be merged, got %q", pr.State)
	}
	if pr.SourceBranch != "feature" || pr.TargetBranch != "main" || pr.HeadSHA != "deadbeef" {
		t.Errorf("branch/sha mapping wrong: %+v", pr)
	}
	// LinkedIssues parsed from title+body, deduped.
	if len(pr.LinkedIssues) != 2 {
		t.Fatalf("linked issues = %v want [12 7]", pr.LinkedIssues)
	}
	if pr.LinkedIssues[0] != 12 || pr.LinkedIssues[1] != 7 {
		t.Errorf("linked issues order/dedup wrong: %v", pr.LinkedIssues)
	}
}

func TestGitHubGetCIStatus_Aggregation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/commits/abc123/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 2,
				"check_runs": []map[string]any{
					{"name": "build", "status": "completed", "conclusion": "success",
						"html_url": "https://github.com/o/r/runs/1", "started_at": "2026-01-01T00:00:00Z"},
					{"name": "deploy", "status": "in_progress", "conclusion": nil,
						"html_url": "https://github.com/o/r/runs/2", "started_at": "2026-01-01T00:01:00Z"},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/commits/abc123/status"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": "pending", "sha": "abc123",
				"statuses": []map[string]any{
					{"context": "legacy-lint", "state": "success", "target_url": "https://ci/lint"},
				},
			})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	st, err := c.GetCIStatus(context.Background(), "o/r", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if st.SHA != "abc123" {
		t.Errorf("sha = %q", st.SHA)
	}
	// A run is in_progress → aggregate is running.
	if st.State != forge.CIRunning {
		t.Errorf("aggregate state = %q want %q", st.State, forge.CIRunning)
	}
	// 2 check-runs + 1 commit-status = 3 runs.
	if len(st.Runs) != 3 {
		t.Fatalf("runs = %d want 3", len(st.Runs))
	}
}

func TestGitHubGetCIStatus_FailedWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"check_runs": []map[string]any{
					{"name": "build", "status": "completed", "conclusion": "success"},
					{"name": "test", "status": "completed", "conclusion": "failure"},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/status"):
			// No legacy statuses configured → 404 is the common case.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	st, err := c.GetCIStatus(context.Background(), "o/r", "sha")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != forge.CIFailed {
		t.Errorf("any failure → failed, got %q", st.State)
	}
	if len(st.Runs) != 2 {
		t.Errorf("404 on /status must be empty, not error: runs=%d", len(st.Runs))
	}
}

func TestGitHubGetCIStatus_AllSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"check_runs": []map[string]any{
					{"name": "build", "status": "completed", "conclusion": "success"},
					{"name": "skipped-opt", "status": "completed", "conclusion": "skipped"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "sha": "z", "statuses": []any{}})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	st, err := c.GetCIStatus(context.Background(), "o/r", "z")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != forge.CISuccess {
		t.Errorf("success+skipped → success, got %q", st.State)
	}
}

func TestGitHubListCIHistory_NewestFirstCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"check_runs": []map[string]any{
				{"name": "old", "status": "completed", "conclusion": "success", "started_at": "2026-01-01T00:00:00Z"},
				{"name": "new", "status": "completed", "conclusion": "success", "started_at": "2026-03-01T00:00:00Z"},
				{"name": "mid", "status": "completed", "conclusion": "success", "started_at": "2026-02-01T00:00:00Z"},
			},
		})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	runs, err := c.ListCIHistory(context.Background(), "o/r", "main", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("limit not applied: %d", len(runs))
	}
	if runs[0].Name != "new" || runs[1].Name != "mid" {
		t.Errorf("not newest-first: %s, %s", runs[0].Name, runs[1].Name)
	}
}

func TestGitHubListPullRequests_ErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	if _, err := c.ListPullRequests(context.Background(), "o/r", forge.PullListOptions{}); err != forge.ErrUnauthorized {
		t.Errorf("401 → %v want ErrUnauthorized", err)
	}
}
