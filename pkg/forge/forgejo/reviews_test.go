package forgejo

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

func TestForgejoCreatePullReview_InlineVerified(t *testing.T) {
	var createBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&createBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "html_url": "https://forge.example/o/r/pulls/7#issuecomment-5"})
	})
	mux.HandleFunc("GET /api/v1/repos/o/r/pulls/7/reviews/5/comments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1}, {"id": 2}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "o/r", 7, forge.NewReview{
		Body: "summary",
		Comments: []forge.ReviewComment{
			{Path: "a.go", Line: 3, Body: "single-line", Suggestion: "y := 2"},
			{Path: "b.go", Line: 4, LineEnd: 6, Body: "multi-line", Suggestion: "z()"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 2 || !res.Verified || res.Fallback != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Only the single-line suggestion is a one-click suggestion; the
	// multi-line span degrades to a plain fenced block.
	if res.SuggestionsPosted != 1 {
		t.Fatalf("SuggestionsPosted = %d, want 1", res.SuggestionsPosted)
	}
	comments := createBody["comments"].([]any)
	first := comments[0].(map[string]any)
	if first["new_position"] != float64(3) || !strings.Contains(first["body"].(string), "```suggestion\ny := 2\n```") {
		t.Fatalf("single-line comment wrong: %v", first)
	}
	second := comments[1].(map[string]any)
	if strings.Contains(second["body"].(string), "```suggestion") {
		t.Fatalf("multi-line span must not render a one-click suggestion: %v", second["body"])
	}
	if !strings.Contains(second["body"].(string), "lines 4-6") {
		t.Fatalf("multi-line replacement must state its span: %v", second["body"])
	}
}

func TestForgejoCreatePullReview_422FallsBackToSummary(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if calls == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		if body["comments"] != nil {
			t.Fatal("fallback attempt must not carry inline comments")
		}
		if !strings.Contains(body["body"].(string), "a.go:3") {
			t.Fatalf("fallback body must fold the findings: %v", body["body"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 6, "html_url": "u"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "o/r", 7, forge.NewReview{
		Body:     "summary",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 3, Body: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || res.Fallback != "summary" || res.CommentsPosted != 0 {
		t.Fatalf("unexpected result (calls=%d): %+v", calls, res)
	}
}
