package gitlab

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

func TestGitLabCreatePullReview_DiscussionsAndNote(t *testing.T) {
	var discussions []map[string]any
	var notes []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/grp%2Fproj/merge_requests/9", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web_url": "https://gitlab.example/grp/proj/-/merge_requests/9",
			"diff_refs": map[string]any{
				"base_sha": "b1", "head_sha": "h1", "start_sha": "s1",
			},
		})
	})
	mux.HandleFunc("POST /api/v4/projects/grp%2Fproj/merge_requests/9/discussions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		discussions = append(discussions, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "d"})
	})
	mux.HandleFunc("POST /api/v4/projects/grp%2Fproj/merge_requests/9/notes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		notes = append(notes, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "grp/proj", 9, forge.NewReview{
		Body: "summary",
		Comments: []forge.ReviewComment{
			{Path: "a.go", Line: 3, LineEnd: 5, Body: "span", Suggestion: "x := 1"},
			{Path: "b.go", Line: 7, Body: "plain"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 2 || res.SuggestionsPosted != 1 || !res.Verified || res.Fallback != "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.URL != "https://gitlab.example/grp/proj/-/merge_requests/9" {
		t.Fatalf("URL = %q", res.URL)
	}
	if len(discussions) != 2 || len(notes) != 1 {
		t.Fatalf("discussions=%d notes=%d", len(discussions), len(notes))
	}
	pos := discussions[0]["position"].(map[string]any)
	if pos["base_sha"] != "b1" || pos["head_sha"] != "h1" || pos["start_sha"] != "s1" || pos["new_path"] != "a.go" || pos["new_line"] != float64(3) {
		t.Fatalf("position wrong: %v", pos)
	}
	// GitLab multi-line suggestion fence extends N lines below the anchor.
	if !strings.Contains(discussions[0]["body"].(string), "```suggestion:-0+2\nx := 1\n```") {
		t.Fatalf("gitlab suggestion fence wrong: %v", discussions[0]["body"])
	}
}

func TestGitLabCreatePullReview_PartialFoldsFailures(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/2", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
	})
	calls := 0
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/2/discussions", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest) // unanchorable position
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "d"})
	})
	var noteBody string
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/2/notes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		noteBody, _ = body["body"].(string)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 2, forge.NewReview{
		Body: "summary",
		Comments: []forge.ReviewComment{
			{Path: "gone.go", Line: 1, Body: "rejected anchor"},
			{Path: "ok.go", Line: 2, Body: "accepted"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 1 || res.Fallback != "partial" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(noteBody, "gone.go:1") {
		t.Fatalf("rejected comment must be folded into the summary note, got %q", noteBody)
	}
}

func TestGitLabCreatePullReview_TotalFailureSurfaces(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/2", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
	})
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/2/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/2/notes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	if _, err := c.CreatePullReview(context.Background(), "g/p", 2, forge.NewReview{
		Body:     "summary",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}},
	}); err == nil {
		t.Fatal("nothing landed — must surface an error, never fake success")
	}
}

var _ forge.ReviewerAssigner = (*AdminClient)(nil)

// AddSelfAsPullReviewer is a read-modify-write: GitLab's reviewer_ids PUT
// replaces the whole set, so the humans already on it must ride along —
// and a bot already present must produce no write at all.
func TestGitLabAddSelfAsPullReviewer(t *testing.T) {
	reviewers := []map[string]any{{"id": float64(12), "username": "carol"}}
	var puts []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 575, "username": "iterion-bot"})
	})
	mux.HandleFunc("GET /api/v4/projects/grp%2Fproj/merge_requests/9", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"reviewers": reviewers})
	})
	mux.HandleFunc("PUT /api/v4/projects/grp%2Fproj/merge_requests/9", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		puts = append(puts, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"iid": 9})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	if err := c.AddSelfAsPullReviewer(context.Background(), "grp/proj", 9); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 1 {
		t.Fatalf("puts=%d", len(puts))
	}
	ids := puts[0]["reviewer_ids"].([]any)
	if len(ids) != 2 || ids[0] != float64(12) || ids[1] != float64(575) {
		t.Fatalf("reviewer union must keep carol and append the bot: %v", ids)
	}

	// Already a reviewer → idempotent no-op, no second PUT.
	reviewers = append(reviewers, map[string]any{"id": float64(575), "username": "iterion-bot"})
	if err := c.AddSelfAsPullReviewer(context.Background(), "grp/proj", 9); err != nil {
		t.Fatal(err)
	}
	if len(puts) != 1 {
		t.Fatalf("already-present must not write: puts=%d", len(puts))
	}
}
