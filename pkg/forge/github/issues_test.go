package github

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

var _ forge.IssueClient = (*AdminClient)(nil)

func TestGitHubListIssues_FiltersOutPRs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != "all" {
			t.Errorf("state = %q want all", got)
		}
		if got := r.URL.Query().Get("labels"); got != "bug,p1" {
			t.Errorf("labels = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"number": 1, "title": "a real issue", "state": "open",
				"html_url":  "https://github.com/o/r/issues/1",
				"labels":    []map[string]any{{"name": "bug"}},
				"assignees": []map[string]any{{"login": "alice"}},
				"user":      map[string]any{"login": "bob"},
			},
			{
				"number": 2, "title": "actually a PR", "state": "open",
				"html_url":     "https://github.com/o/r/pull/2",
				"pull_request": map[string]any{"url": "https://api.github.com/o/r/pulls/2"},
			},
		})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	issues, err := c.ListIssues(context.Background(), "o/r", forge.IssueListOptions{
		State: "all", Labels: []string{"bug", "p1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("want 2 items (PR flagged, not dropped), got %d", len(issues))
	}
	// Caller filters by IsPullRequest; verify the flag is set correctly.
	prCount := 0
	for _, i := range issues {
		if i.IsPullRequest {
			prCount++
		}
	}
	if prCount != 1 {
		t.Fatalf("want exactly 1 IsPullRequest, got %d", prCount)
	}
	if issues[0].IsPullRequest {
		t.Error("issue #1 should not be a PR")
	}
	if !issues[1].IsPullRequest {
		t.Error("issue #2 should be flagged as a PR")
	}
	if issues[0].Author != "bob" || len(issues[0].Labels) != 1 || issues[0].Labels[0] != "bug" {
		t.Errorf("issue #1 mapping wrong: %+v", issues[0])
	}
	if len(issues[0].Assignees) != 1 || issues[0].Assignees[0] != "alice" {
		t.Errorf("assignees wrong: %+v", issues[0].Assignees)
	}
}

func TestGitHubCreateIssue_RoundTrip(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/issues") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 42, "title": body["title"], "state": "open",
			"html_url": "https://github.com/o/r/issues/42",
		})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	got, err := c.CreateIssue(context.Background(), "o/r", forge.NewIssue{
		Title: "ship it", Body: "details", Labels: []string{"feat"}, Assignees: []string{"carol"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["title"] != "ship it" || body["body"] != "details" {
		t.Errorf("request body = %v", body)
	}
	if labels, _ := body["labels"].([]any); len(labels) != 1 || labels[0] != "feat" {
		t.Errorf("labels = %v", body["labels"])
	}
	if got.Number != 42 || got.State != "open" {
		t.Errorf("created = %+v", got)
	}
}

func TestGitHubUpdateIssue_RoundTrip(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/issues/7") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "title": "old", "state": body["state"],
			"html_url": "https://github.com/o/r/issues/7",
		})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	closed := "closed"
	got, err := c.UpdateIssue(context.Background(), "o/r", 7, forge.IssuePatch{State: &closed})
	if err != nil {
		t.Fatal(err)
	}
	if body["state"] != "closed" {
		t.Errorf("request state = %v", body["state"])
	}
	// Title was nil in the patch → must be absent from the request body.
	if _, present := body["title"]; present {
		t.Errorf("nil patch field leaked into body: %v", body)
	}
	if got.Number != 7 || got.State != "closed" {
		t.Errorf("updated = %+v", got)
	}
}

func TestGitHubCommentIssue_RoundTrip(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/issues/7/comments") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       123,
			"html_url": "https://github.com/o/r/issues/7#issuecomment-123",
			"body":     body["body"],
			"user":     map[string]any{"login": "bot"},
		})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	got, err := c.CommentIssue(context.Background(), "o/r", 7, "looks good")
	if err != nil {
		t.Fatal(err)
	}
	if body["body"] != "looks good" {
		t.Errorf("request body = %v", body)
	}
	if got.ID != "123" || got.Author != "bot" || got.Body != "looks good" {
		t.Errorf("comment = %+v", got)
	}
	if got.URL != "https://github.com/o/r/issues/7#issuecomment-123" {
		t.Errorf("url = %q", got.URL)
	}
}

func TestGitHubGetIssue_ErrorMapping(t *testing.T) {
	cases := map[int]error{
		http.StatusNotFound:     forge.ErrHookNotFound,
		http.StatusUnauthorized: forge.ErrUnauthorized,
		http.StatusForbidden:    forge.ErrForbidden,
	}
	for code, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
		_, err := c.GetIssue(context.Background(), "o/r", 1)
		if !errors.Is(err, want) {
			t.Errorf("status %d → err %v, want %v", code, err, want)
		}
		srv.Close()
	}
}
