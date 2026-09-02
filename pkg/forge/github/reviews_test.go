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

var _ forge.ReviewClient = (*AdminClient)(nil)
var _ forge.ReviewClient = (*AppClient)(nil)

func TestGitHubCreatePullReview_InlineVerified(t *testing.T) {
	var createBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/o/r/pulls/42/reviews", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 99, "html_url": "https://github.com/o/r/pull/42#pullrequestreview-99"})
	})
	mux.HandleFunc("GET /repos/o/r/pulls/42/reviews/99/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1}, {"id": 2}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "o/r", 42, forge.NewReview{
		Body: "summary",
		Comments: []forge.ReviewComment{
			{Path: "a.go", Line: 3, LineEnd: 5, Body: "multi-line", Suggestion: "fixed()\nlines()"},
			{Path: "b.go", Line: 9, Body: "single"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified || res.CommentsPosted != 2 || res.SuggestionsPosted != 1 || res.Fallback != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.URL == "" {
		t.Fatal("review URL missing")
	}
	if createBody["event"] != "COMMENT" {
		t.Fatalf("event = %v, want COMMENT (advisory, never a merge gate)", createBody["event"])
	}
	comments := createBody["comments"].([]any)
	first := comments[0].(map[string]any)
	// Multi-line span: start_line = Line, line = LineEnd, RIGHT side.
	if first["start_line"] != float64(3) || first["line"] != float64(5) || first["side"] != "RIGHT" {
		t.Fatalf("multi-line anchor wrong: %v", first)
	}
	if !strings.Contains(first["body"].(string), "```suggestion\nfixed()\nlines()\n```") {
		t.Fatalf("suggestion fence missing: %v", first["body"])
	}
	second := comments[1].(map[string]any)
	if second["line"] != float64(9) || second["start_line"] != nil {
		t.Fatalf("single-line anchor wrong: %v", second)
	}
}

func TestGitHubCreatePullReview_422FallsBackToSummary(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/o/r/pulls/42/reviews", func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if calls == 1 {
			if body["comments"] == nil {
				t.Fatal("first attempt should carry inline comments")
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "line must be part of the diff"})
			return
		}
		// Fallback attempt: comments folded into the body, none inline.
		if body["comments"] != nil {
			t.Fatal("fallback attempt must not carry inline comments")
		}
		if !strings.Contains(body["body"].(string), "a.go:3") {
			t.Fatalf("fallback body must fold the findings, got %v", body["body"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 100, "html_url": "https://github.com/o/r/pull/42#pullrequestreview-100"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "o/r", 42, forge.NewReview{
		Body:     "summary",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 3, Body: "not in diff"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("want 2 create attempts, got %d", calls)
	}
	if res.Fallback != "summary" || res.CommentsPosted != 0 || !res.Verified {
		t.Fatalf("unexpected fallback result: %+v", res)
	}
}

func TestGitHubCreatePullReview_ErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	if _, err := c.CreatePullReview(context.Background(), "o/r", 1, forge.NewReview{Body: "x"}); err == nil {
		t.Fatal("a 403 must surface as an error, never fake success")
	}
}

// The conversation gate needs the NEWEST comments (the thread just replied
// to), so the capped walk fetches newest-first and hands back chronological
// order.
func TestGitHubListPRReviewComments_NewestFirstCapReversed(t *testing.T) {
	pages := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7/comments", func(w http.ResponseWriter, r *http.Request) {
		pages++
		q := r.URL.Query()
		if q.Get("sort") != "created" || q.Get("direction") != "desc" {
			t.Fatalf("must request newest-first: %s", r.URL.RawQuery)
		}
		// One short page, newest first.
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 9002, "in_reply_to_id": 9001, "body": "newest", "user": map[string]any{"login": "alice"}},
			{"id": 9001, "body": "oldest", "user": map[string]any{"login": "revi-bot"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	out, err := c.ListPRReviewComments(context.Background(), "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if pages != 1 || len(out) != 2 {
		t.Fatalf("pages=%d len=%d", pages, len(out))
	}
	if out[0].ID != 9001 || out[1].ID != 9002 {
		t.Fatalf("callers must receive chronological order: %+v", out)
	}
}
