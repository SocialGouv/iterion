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
			// No legacy statuses configured: GitHub answers 200 with an
			// empty list, not 404.
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "sha": "sha", "statuses": []any{}})
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
		t.Errorf("an empty /status contributes nothing: runs=%d", len(st.Runs))
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

func TestGitHubCreatePull_Mapping(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/pulls") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 9, "title": body["title"], "state": "open",
			"html_url": "https://github.com/o/r/pull/9", "draft": body["draft"],
			"head": map[string]any{"ref": body["head"], "sha": "sha9"},
			"base": map[string]any{"ref": body["base"]},
		})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	pr, err := c.CreatePull(context.Background(), "o/r", forge.NewPull{
		Title: "feat: x", Body: "details", SourceBranch: "feature", TargetBranch: "main", Draft: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["head"] != "feature" || body["base"] != "main" {
		t.Errorf("source/target mapping wrong: head=%v base=%v", body["head"], body["base"])
	}
	if body["title"] != "feat: x" || body["body"] != "details" {
		t.Errorf("title/body = %v", body)
	}
	if body["draft"] != true {
		t.Errorf("draft = %v want true", body["draft"])
	}
	if pr.Number != 9 || pr.SourceBranch != "feature" || pr.TargetBranch != "main" {
		t.Errorf("created pr = %+v", pr)
	}
}

func TestGitHubUpdatePull_StateAndTarget(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/pulls/3") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 3, "title": "t", "state": body["state"],
			"html_url": "https://github.com/o/r/pull/3",
			"base":     map[string]any{"ref": body["base"]},
		})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	closed, target := "closed", "develop"
	pr, err := c.UpdatePull(context.Background(), "o/r", 3, forge.PullPatch{State: &closed, TargetBranch: &target})
	if err != nil {
		t.Fatal(err)
	}
	if body["state"] != "closed" {
		t.Errorf("state maps to %v want closed", body["state"])
	}
	if body["base"] != "develop" {
		t.Errorf("TargetBranch maps to base=%v want develop", body["base"])
	}
	if _, present := body["title"]; present {
		t.Errorf("nil patch field leaked: %v", body)
	}
	if pr.State != "closed" || pr.TargetBranch != "develop" {
		t.Errorf("updated pr = %+v", pr)
	}
}

func TestGitHubMergePull_SquashAndDeleteBranch(t *testing.T) {
	var mergeBody map[string]any
	getCount := 0
	deleteHit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/5"):
			// GitHub impl GETs once, AFTER the merge (it reads the source branch
			// off the re-fetched ref for the optional delete).
			getCount++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 5, "title": "t", "state": "closed",
				"html_url":  "https://github.com/o/r/pull/5",
				"head":      map[string]any{"ref": "feature", "sha": "abc"},
				"base":      map[string]any{"ref": "main"},
				"merged_at": "2026-01-02T03:04:05Z",
			})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/pulls/5/merge"):
			_ = json.NewDecoder(r.Body).Decode(&mergeBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "mergesha"})
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/git/refs/heads/feature"):
			deleteHit = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %q", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	pr, err := c.MergePull(context.Background(), "o/r", 5, forge.MergeOptions{
		Method: forge.MergeSquash, DeleteBranch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mergeBody["merge_method"] != "squash" {
		t.Errorf("merge_method = %v want squash", mergeBody["merge_method"])
	}
	if !deleteHit {
		t.Error("DeleteBranch did not trigger a branch delete")
	}
	if getCount != 1 {
		t.Errorf("expected 1 GET (post-merge only), got %d", getCount)
	}
	if pr.State != "merged" {
		t.Errorf("post-merge state = %q want merged", pr.State)
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

// TestGitHubGetPullRequest_ForkAdmissionFields: the fork review lane's
// admission reads three fields off the pull request the API answers — the
// author's NUMERIC id (the per-contributor bound's key), the BASE's clone
// URL (the one repository a fork launch names) and the base's visibility
// (a private base is refused before the bound is metered). GitHub reports
// `visibility` on the repository object; a payload that carries only the
// legacy `private` boolean still decides, and one that carries neither
// stays unproven — the admission refuses unproven, never guesses public.
func TestGitHubGetPullRequest_ForkAdmissionFields(t *testing.T) {
	cases := []struct {
		name          string
		baseRepo      map[string]any
		userID        any
		wantVis       string
		wantAuthorID  string
		wantBaseClone string
	}{
		{
			name: "visibility wins",
			baseRepo: map[string]any{
				"clone_url": "https://github.com/o/r.git", "private": false, "visibility": "public",
			},
			userID:        4242,
			wantVis:       "public",
			wantAuthorID:  "4242",
			wantBaseClone: "https://github.com/o/r.git",
		},
		{
			name: "the private boolean decides when visibility is absent",
			baseRepo: map[string]any{
				"clone_url": "https://github.com/o/r.git", "private": true,
			},
			userID:        7,
			wantVis:       "private",
			wantAuthorID:  "7",
			wantBaseClone: "https://github.com/o/r.git",
		},
		{
			name: "neither present stays unproven",
			baseRepo: map[string]any{
				"clone_url": "https://github.com/o/r.git",
			},
			userID:        9,
			wantVis:       "",
			wantAuthorID:  "9",
			wantBaseClone: "https://github.com/o/r.git",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 5, "title": "t", "state": "open",
					"head": map[string]any{
						"ref": "feature", "sha": "deadbeef",
						"repo": map[string]any{"full_name": "alice/fork", "clone_url": "https://github.com/alice/fork.git"},
					},
					"base": map[string]any{"ref": "main", "repo": tc.baseRepo},
					"user": map[string]any{"login": "alice", "id": tc.userID},
				})
			}))
			defer srv.Close()
			c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
			pr, err := c.GetPullRequest(context.Background(), "o/r", 5)
			if err != nil {
				t.Fatal(err)
			}
			if pr.AuthorID != tc.wantAuthorID {
				t.Fatalf("AuthorID = %q, want %q — the per-contributor bound keys on the numeric id", pr.AuthorID, tc.wantAuthorID)
			}
			if pr.BaseVisibility != tc.wantVis {
				t.Fatalf("BaseVisibility = %q, want %q", pr.BaseVisibility, tc.wantVis)
			}
			if pr.BaseCloneURL != tc.wantBaseClone {
				t.Fatalf("BaseCloneURL = %q, want %q", pr.BaseCloneURL, tc.wantBaseClone)
			}
		})
	}
}
