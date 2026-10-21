package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

var _ forge.ReviewClient = (*AdminClient)(nil)

func TestGitLabCreatePullReview_DiscussionsAndNote(t *testing.T) {
	fastReadBack(t)
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
		storeDiscussion(w, r, &discussions)
	})
	mux.HandleFunc("POST /api/v4/projects/grp%2Fproj/merge_requests/9/notes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		notes = append(notes, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	serveDiscussionsReadBack(mux, "/api/v4/projects/grp%2Fproj/merge_requests/9/discussions", &discussions, 0)
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
	if res.CommentsPosted != 2 || res.SuggestionsPosted != 1 || !res.Verified || res.Fallback != "" || res.BodyMissing {
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

func TestGitLabCreatePullReview_SummaryNoteFailureIsBodyMissing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		noteStatus   int
		rejectAnchor bool
	}{
		{name: "note 403", noteStatus: http.StatusForbidden},
		{name: "note 429", noteStatus: http.StatusTooManyRequests},
		{name: "rejected anchor and note", noteStatus: http.StatusForbidden, rejectAnchor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastReadBack(t)
			mux := http.NewServeMux()
			serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/44")
			var stored []map[string]any
			creates := 0
			mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/44/discussions", func(w http.ResponseWriter, r *http.Request) {
				creates++
				if tc.rejectAnchor && creates == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"message":{"line_code":["can't be blank"]}}`))
					return
				}
				storeDiscussion(w, r, &stored)
			})
			noteCalls := 0
			mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/44/notes", func(w http.ResponseWriter, r *http.Request) {
				noteCalls++
				if tc.noteStatus == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "30")
				}
				w.WriteHeader(tc.noteStatus)
			})
			serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/44/discussions", &stored, 0)
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
			res, err := c.CreatePullReview(context.Background(), "g/p", 44, forge.NewReview{Body: "summary", Comments: []forge.ReviewComment{
				{Path: "a.go", Line: 1, Body: "first"},
				{Path: "b.go", Line: 2, Body: "second"},
			}})
			if err != nil {
				t.Fatalf("anchored comments landed, so the refused note stays a partial success: %v", err)
			}
			wantPosted := 2
			if tc.rejectAnchor {
				wantPosted = 1
			}
			if !res.BodyMissing {
				t.Errorf("the summary note was refused but BodyMissing is false; result = %+v", res)
			}
			if res.CommentsPosted != wantPosted || !res.Verified || res.Fallback != "partial" {
				t.Errorf("counts, verification and fallback must keep their existing meaning; result = %+v, want posted = %d", res, wantPosted)
			}
			if noteCalls != 1 {
				t.Errorf("a refused summary note must not be retried, calls = %d", noteCalls)
			}
			if tc.rejectAnchor && (len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "400")) {
				t.Errorf("the rejected anchor keeps its diagnostic, got %v", res.Diagnostics)
			}
		})
	}
}

func TestGitLabCreatePullReview_PartialFoldsFailures(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/2", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
	})
	calls := 0
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/2/discussions", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest) // unanchorable position
			return
		}
		storeDiscussion(w, r, &stored)
	})
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/2/discussions", &stored, 0)
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
	if res.CommentsPosted != 1 || res.Fallback != "partial" || res.BodyMissing {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(noteBody, "gone.go:1") {
		t.Fatalf("rejected comment must be folded into the summary note, got %q", noteBody)
	}
}

func TestGitLabCreatePullReview_TotalFailureSurfaces(t *testing.T) {
	fastReadBack(t)
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
	var none []map[string]any
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/2/discussions", &none, 0)
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

// gitlabStoredBody is what GitLab keeps of a posted note body: its
// quick-action extractor runs on every note, deleting carriage returns,
// removing a quick-action line (`/rebase` alone on a line) and stripping
// trailing whitespace. The fakes serve bodies through it so a key built from
// the body AS SENT is as wrong here as on the real forge.
func gitlabStoredBody(body string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r", ""), "\n") {
		if strings.HasPrefix(line, "/rebase") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimRight(strings.Join(kept, "\n"), " \t\n")
}

// discussionID is the id the fakes hand a stored discussion: its index.
func discussionID(i int) string { return fmt.Sprintf("d%d", i) }

// renderDiscussions lists stored discussions the way GitLab's discussion
// endpoint does: one entry per discussion, its id, each note carrying the
// stored body, its position and its resolved flag.
func renderDiscussions(stored []map[string]any) []map[string]any {
	all := make([]map[string]any, 0, len(stored))
	for i, d := range stored {
		body, _ := d["body"].(string)
		resolved, _ := d["resolved"].(bool)
		all = append(all, map[string]any{"id": discussionID(i), "notes": []map[string]any{{
			"id": i + 1, "type": "DiffNote", "body": gitlabStoredBody(body), "position": d["position"], "resolved": resolved,
		}}})
	}
	return all
}

// serveDiscussionsReadBack serves GET <path> the way GitLab lists a merge
// request's discussions. pageSize > 0 paginates with X-Next-Page (what a real
// instance does past 100 discussions); 0 serves everything on one page.
func serveDiscussionsReadBack(mux *http.ServeMux, path string, stored *[]map[string]any, pageSize int) {
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		all := renderDiscussions(*stored)
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscanf(p, "%d", &page)
		}
		if pageSize > 0 {
			start := (page - 1) * pageSize
			end := start + pageSize
			if start > len(all) {
				start = len(all)
			}
			if end > len(all) {
				end = len(all)
			}
			if end < len(all) {
				w.Header().Set("X-Next-Page", fmt.Sprint(page+1))
			} else {
				w.Header().Set("X-Next-Page", "")
			}
			all = all[start:end]
		}
		_ = json.NewEncoder(w).Encode(all)
	})
}

// storeDiscussion is the POST /discussions fake's happy answer: the body is
// persisted and the discussion id GitLab would answer with comes back.
func storeDiscussion(w http.ResponseWriter, r *http.Request, stored *[]map[string]any) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	*stored = append(*stored, body)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": discussionID(len(*stored) - 1)})
}

// fastReadBack removes the pause between two read-backs for the test's
// duration: the retry is the fact under test, never the second.
func fastReadBack(t *testing.T) {
	t.Helper()
	prev := readBackRetryDelay
	readBackRetryDelay = 0
	t.Cleanup(func() { readBackRetryDelay = prev })
	t.Setenv(ReadBackBudgetEnv, "")
}

// position anchors a stored discussion on the head the tests publish to.
func position(path string, line int) map[string]any {
	return map[string]any{"new_path": path, "new_line": line, "head_sha": "h"}
}

// The !220 shape, measured on a GitLab 17 instance: a positioned discussion
// POST answered an error while the discussion was persisted. Counting the
// create answers alone reported 0 inline comments, folded both findings into
// the summary a second time, and raised a "publish failed" banner over a
// merge request that held every thread. The follow-up read is the fact.
func TestGitLabCreatePullReview_CreateErrorButTheMergeRequestHoldsIt(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/3", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
	})
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/3/discussions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		stored = append(stored, body)                 // persisted…
		w.WriteHeader(http.StatusInternalServerError) // …behind an error answer
		_, _ = w.Write([]byte(`{"message":"500 Internal Server Error"}`))
	})
	var noteBody string
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/3/notes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		noteBody, _ = body["body"].(string)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/3/discussions", &stored, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 3, forge.NewReview{
		Body: "summary",
		Comments: []forge.ReviewComment{
			{Path: "a.ts", Line: 97, Body: "swallowed error", Suggestion: "throw err"},
			{Path: "a.ts", Line: 71, Body: "no transaction"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 2 || res.SuggestionsPosted != 1 || !res.Verified || res.Fallback != "" {
		t.Fatalf("the merge request holds both threads; result = %+v", res)
	}
	if strings.Contains(noteBody, "a.ts:97") || strings.Contains(noteBody, "a.ts:71") {
		t.Fatalf("a thread the merge request holds must not be folded into the summary a second time, got %q", noteBody)
	}
	if len(res.Diagnostics) != 2 {
		t.Fatalf("the operator's log must name both error answers, got %v", res.Diagnostics)
	}
	for _, d := range res.Diagnostics {
		if !strings.Contains(d, "500") || !strings.Contains(d, "Internal Server Error") || !strings.Contains(d, "anyway") {
			t.Fatalf("diagnostic must carry the status, the body's head and the read-back verdict, got %q", d)
		}
	}
}

// The reverse lie: a create that answered 2xx for a thread the merge request
// does not hold. The read-back, not the create answer, decides what is
// folded — a finding the pull request lost is a finding the summary carries.
func TestGitLabCreatePullReview_ReadBackContradictsACreate(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/4", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
	})
	var stored []map[string]any
	calls := 0
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/4/discussions", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			storeDiscussion(w, r, &stored) // only the first one really lands
			return
		}
		w.WriteHeader(http.StatusCreated) // a 2xx for a thread that is nowhere
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ghost"})
	})
	var noteBody string
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/4/notes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		noteBody, _ = body["body"].(string)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/4/discussions", &stored, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 4, forge.NewReview{
		Body:     "summary",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "first"}, {Path: "b.go", Line: 2, Body: "lost"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 1 || !res.Verified || res.Fallback != "partial" {
		t.Fatalf("the read-back holds one thread; result = %+v", res)
	}
	if !strings.Contains(noteBody, "b.go:2") || strings.Contains(noteBody, "a.go:1") {
		t.Fatalf("only the lost thread is folded into the summary, got %q", noteBody)
	}
}

// When the read-back itself fails, the create answers are the best count
// there is — reported as unverified, with the read-back's failure named for
// the operator, never dressed up as a confirmed write.
func TestGitLabCreatePullReview_ReadBackUnavailableReportsUnverified(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/5", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
	})
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/5/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "d0"})
	})
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/5/notes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/5/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 5, forge.NewReview{
		Body:     "summary",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}, {Path: "b.go", Line: 2, Body: "y"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 2 || res.Verified || res.Fallback != "" {
		t.Fatalf("unverified create count expected; result = %+v", res)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "502") {
		t.Fatalf("the read-back's own failure must be named, got %v", res.Diagnostics)
	}
}

// A real instance pages its discussion list; a review on a busy merge
// request lands past page one. Ignoring X-Next-Page would report the
// later threads as lost and fold them a second time.
func TestGitLabCreatePullReview_ReadBackFollowsPagination(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/6", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
	})
	// Three older human threads already on the merge request, then ours.
	stored := []map[string]any{
		{"body": "older 1", "position": map[string]any{"new_path": "z.go", "new_line": 1}},
		{"body": "older 2", "position": map[string]any{"new_path": "z.go", "new_line": 2}},
		{"body": "older 3", "position": map[string]any{"new_path": "z.go", "new_line": 3}},
	}
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/6/discussions", func(w http.ResponseWriter, r *http.Request) {
		storeDiscussion(w, r, &stored)
	})
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/6/notes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/6/discussions", &stored, 2)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 6, forge.NewReview{
		Body:     "summary",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "ours 1"}, {Path: "b.go", Line: 2, Body: "ours 2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 2 || !res.Verified || res.Fallback != "" {
		t.Fatalf("both threads sit past page one; result = %+v", res)
	}
}

// serveMR answers GET <path> with the diff refs the tests publish against.
func serveMR(mux *http.ServeMux, path string) {
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
	})
}

// captureNote accepts the summary note on POST <path> and keeps its body.
func captureNote(mux *http.ServeMux, path string) *string {
	var noteBody string
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		noteBody, _ = body["body"].(string)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	return &noteBody
}

// A 2xx create is recognised by the discussion id it answered with, whatever
// GitLab made of the body: a quick-action line its extractor removed leaves a
// stored body no key built from the body as sent can match. Counting that
// thread lost would fold the finding a second time and report "partial" over
// a merge request that holds it.
func TestGitLabCreatePullReview_ACreatedThreadIsRecognisedByItsID(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/7")
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/7/discussions", func(w http.ResponseWriter, r *http.Request) {
		storeDiscussion(w, r, &stored)
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/7/notes")
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/7/discussions", &stored, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 7, forge.NewReview{
		Body: "summary",
		Comments: []forge.ReviewComment{
			{Path: "a.go", Line: 3, Body: "finding\n/rebase\n"},
			{Path: "b.go", Line: 9, Body: "crlf\r\nfinding   ", Suggestion: "x := 1\r\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 2 || res.SuggestionsPosted != 1 || !res.Verified || res.Fallback != "" || len(res.Diagnostics) != 0 {
		t.Fatalf("both threads are on the merge request under their ids; result = %+v", res)
	}
	if strings.Contains(*noteBody, "could not be anchored") {
		t.Fatalf("nothing to fold, got %q", *noteBody)
	}
}

// The rescue of a create whose answer was lost compares bodies, and GitLab
// stores a body without its carriage returns and trailing whitespace: a
// CRLF finding persisted behind an error answer is still the thread on the
// merge request.
func TestGitLabCreatePullReview_TheRescueMatchesTheBodyAsGitLabStoresIt(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/8")
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/8/discussions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		stored = append(stored, body)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>502 Bad Gateway</html>`))
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/8/notes")
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/8/discussions", &stored, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 8, forge.NewReview{
		Body:     "summary",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 3, Body: "line one\r\nline two   "}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 1 || !res.Verified || res.Fallback != "" {
		t.Fatalf("the merge request holds the CRLF thread; result = %+v", res)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "502") || !strings.Contains(res.Diagnostics[0], "anyway") {
		t.Fatalf("diagnostics = %v", res.Diagnostics)
	}
	if strings.Contains(*noteBody, "a.go:3") {
		t.Fatalf("a held thread must not be folded, got %q", *noteBody)
	}
}

// A thread an earlier publish left on the merge request vouches for a
// finding raised now only while the reader can still see it where it was
// raised: resolved, the view collapses it; anchored on an earlier head, the
// current diff does not show it. A create that answered 429 without
// persisting anything is folded in both cases, never "held anyway".
func TestGitLabCreatePullReview_AResolvedOrOutdatedTwinDoesNotVouch(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/10")
	stored := []map[string]any{
		{"body": "old finding", "position": position("a.go", 1), "resolved": true},
		{"body": "old finding", "position": map[string]any{"new_path": "a.go", "new_line": 2, "head_sha": "h-before"}},
	}
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/10/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"429 Too Many Requests"}`))
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/10/notes")
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/10/discussions", &stored, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 10, forge.NewReview{
		Body:     "summary",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "old finding"}, {Path: "a.go", Line: 2, Body: "old finding"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 0 || !res.Verified || res.Fallback != "summary" {
		t.Fatalf("neither twin vouches; result = %+v", res)
	}
	if !strings.Contains(*noteBody, "a.go:1") || !strings.Contains(*noteBody, "a.go:2") {
		t.Fatalf("both findings must be folded, got %q", *noteBody)
	}
	if len(res.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %v", res.Diagnostics)
	}
	for _, d := range res.Diagnostics {
		if !strings.Contains(d, "429") || strings.Contains(d, "anyway") {
			t.Fatalf("diagnostic must name the refusal and claim no thread, got %q", d)
		}
	}
}

// A forge can finish persisting a create after the client gave up on its
// answer. One read taken at once would miss the thread and fold the finding
// the reader then sees twice: the read is repeated while a thread is
// missing, and the thread that appears on the second read is counted.
func TestGitLabCreatePullReview_AThreadPersistedAfterTheFirstReadIsFound(t *testing.T) {
	fastReadBack(t)
	run := func(t *testing.T, answer int) {
		t.Helper()
		mux := http.NewServeMux()
		serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/11")
		var pending []map[string]any // persisted, visible only from the LAST read on
		mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/11/discussions", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			pending = append(pending, body)
			w.WriteHeader(answer)
			if answer == http.StatusCreated {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": discussionID(len(pending) - 1)})
			}
		})
		noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/11/notes")
		gets := 0
		mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/11/discussions", func(w http.ResponseWriter, r *http.Request) {
			gets++
			if gets < 3 { // the last read the bound allows
				_ = json.NewEncoder(w).Encode([]map[string]any{})
				return
			}
			_ = json.NewEncoder(w).Encode(renderDiscussions(pending))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
		res, err := c.CreatePullReview(context.Background(), "g/p", 11, forge.NewReview{
			Body:     "summary",
			Comments: []forge.ReviewComment{{Path: "a.go", Line: 3, Body: "late"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.CommentsPosted != 1 || !res.Verified || res.Fallback != "" || gets != 3 {
			t.Fatalf("answer %d: the thread appeared on the last read; result = %+v, reads = %d", answer, res, gets)
		}
		if strings.Contains(*noteBody, "a.go:3") {
			t.Fatalf("answer %d: a thread found on a later read must not be folded, got %q", answer, *noteBody)
		}
		if answer == http.StatusCreated && len(res.Diagnostics) != 0 {
			t.Fatalf("a 2xx create found late is clean, got %v", res.Diagnostics)
		}
		if answer != http.StatusCreated && (len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "504") || !strings.Contains(res.Diagnostics[0], "anyway")) {
			t.Fatalf("diagnostics = %v", res.Diagnostics)
		}
	}
	t.Run("answer lost", func(t *testing.T) { run(t, http.StatusGatewayTimeout) })
	t.Run("answered 2xx, listed late", func(t *testing.T) { run(t, http.StatusCreated) })
}

// Two identical comments need two threads: one persisted thread cannot
// vouch for both, or the second finding — whose create failed — would be
// counted posted and never folded. Both ways a thread is taken (by its id,
// by its key) consume it.
func TestGitLabCreatePullReview_OneThreadVouchesForOneComment(t *testing.T) {
	fastReadBack(t)
	run := func(t *testing.T, firstAnswers int) {
		t.Helper()
		mux := http.NewServeMux()
		serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/12")
		var stored []map[string]any
		calls := 0
		mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/12/discussions", func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls > 1 {
				w.WriteHeader(http.StatusInternalServerError) // the twin is nowhere
				return
			}
			if firstAnswers == http.StatusCreated {
				storeDiscussion(w, r, &stored)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			stored = append(stored, body) // persisted behind an error answer
			w.WriteHeader(firstAnswers)
		})
		noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/12/notes")
		serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/12/discussions", &stored, 0)
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
		same := forge.ReviewComment{Path: "a.go", Line: 3, Body: "same finding"}
		res, err := c.CreatePullReview(context.Background(), "g/p", 12, forge.NewReview{Body: "summary", Comments: []forge.ReviewComment{same, same}})
		if err != nil {
			t.Fatal(err)
		}
		if res.CommentsPosted != 1 || !res.Verified || res.Fallback != "partial" {
			t.Fatalf("first answered %d: one thread, one comment posted; result = %+v", firstAnswers, res)
		}
		if strings.Count(*noteBody, "a.go:3") != 1 {
			t.Fatalf("first answered %d: exactly the twin must be folded, got %q", firstAnswers, *noteBody)
		}
	}
	t.Run("taken by id", func(t *testing.T) { run(t, http.StatusCreated) })
	t.Run("taken by key", func(t *testing.T) { run(t, http.StatusInternalServerError) })
}

// A review of nothing but a summary has no thread to look for: the
// discussion list is not read at all, so a read that would have failed —
// or taken the caller's deadline — can neither discredit nor delay a note
// that landed.
func TestGitLabCreatePullReview_NoInlineCommentsSkipsTheReadBack(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/13")
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/13/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.WriteHeader(http.StatusInternalServerError)
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/13/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 13, forge.NewReview{Body: "budget note"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified || res.Fallback != "" || len(res.Diagnostics) != 0 || res.CommentsPosted != 0 {
		t.Fatalf("a summary-only review is verified by its own answer; result = %+v", res)
	}
	if gets != 0 {
		t.Fatalf("nothing to read back, yet %d read(s)", gets)
	}
}

// The diagnostic quotes and bounds the path: it is the bot's own JSON, and
// a path with a line break in it must not write a line of its own into the
// operator's log, nor a kilobytes-long one flood it.
func TestGitLabCreatePullReview_DiagnosticsQuoteThePath(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/14")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/14/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/14/notes")
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/14/discussions", &[]map[string]any{}, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 14, forge.NewReview{
		Body: "summary",
		Comments: []forge.ReviewComment{
			{Path: "a.go\nWARN forged line", Line: 3, Body: "x"},
			{Path: strings.Repeat("a", 10000), Line: 4, Body: "y"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diagnostics) != 2 || strings.Contains(res.Diagnostics[0], "\n") || !strings.Contains(res.Diagnostics[0], `"a.go WARN forged line":3`) {
		t.Fatalf("diagnostics = %q", res.Diagnostics)
	}
	if len(res.Diagnostics[1]) > 400 {
		t.Fatalf("a 10 KB path must be bounded in the diagnostic, got %d bytes", len(res.Diagnostics[1]))
	}
}

// A create the forge refused (4xx) has its fate: the thread will not appear
// later, so a refusal earns no second read — the publish is not held back
// on a retry that cannot change anything — while a lost answer still is.
func TestGitLabCreatePullReview_ARefusedCreateEarnsNoSecondRead(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/15")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/15/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":{"base":["Position is invalid"]}}`))
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/15/notes")
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/15/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 15, forge.NewReview{
		Body:     "summary",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 3, Body: "unanchorable"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 0 || !res.Verified || res.Fallback != "summary" || gets != 1 {
		t.Fatalf("a refusal is final: one read, folded; result = %+v, reads = %d", res, gets)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "400") || !strings.Contains(res.Diagnostics[0], "Position is invalid") {
		t.Fatalf("diagnostics = %v", res.Diagnostics)
	}
}

// A later read that fails keeps the earlier verdict: the first read found
// the thread persisted behind a 500 and missed the ghost (so the fate stayed
// open), the second read answers 502. What the first read found is a fact;
// discarding it would fold the held thread a second time — the original
// miscount, back through the retry.
func TestGitLabCreatePullReview_ALaterFailedReadKeepsTheEarlierVerdict(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/21")
	var stored []map[string]any
	creates := 0
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/21/discussions", func(w http.ResponseWriter, r *http.Request) {
		creates++
		if creates == 1 {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			stored = append(stored, body) // persisted behind an error answer
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated) // a 2xx for a thread that is nowhere
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ghost"})
	})
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/21/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		if gets >= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(renderDiscussions(stored))
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/21/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 21, forge.NewReview{Body: "summary", Comments: []forge.ReviewComment{
		{Path: "a.go", Line: 3, Body: "persisted behind a 500"},
		{Path: "b.go", Line: 4, Body: "ghost"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified || res.CommentsPosted != 1 || res.Fallback != "partial" || gets != 2 {
		t.Fatalf("the first read's verdict stands; result = %+v, reads = %d", res, gets)
	}
	if strings.Contains(*noteBody, "a.go:3") || !strings.Contains(*noteBody, "b.go:4") {
		t.Fatalf("exactly the ghost is folded, got %q", *noteBody)
	}
}

// A first read that fails is reported, not retried: the retry exists for a
// thread that appears late, which only an answered read can tell. One read,
// unverified, the counts from the creates, the failure in the diagnostics.
func TestGitLabCreatePullReview_AFailedFirstReadIsNotRetried(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/23")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/23/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "d0"})
	})
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/23/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.WriteHeader(http.StatusBadGateway)
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/23/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 23, forge.NewReview{Body: "summary", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || res.CommentsPosted != 1 || gets != 1 || len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "502") {
		t.Fatalf("one failed read, reported; result = %+v, reads = %d", res, gets)
	}
}

// The page cap bounds the read: a forge announcing a next page forever ends
// in exactly readBackPageCap reads and an unverified count that says so —
// never a loop bounded only by the caller's patience.
func TestGitLabCreatePullReview_ThePageCapBoundsTheRead(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/22")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/22/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "d0"})
	})
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/22/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.Header().Set("X-Next-Page", "999999")
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/22/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 22, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gets != readBackPageCap || res.Verified || len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "more than 100 pages") {
		t.Fatalf("the cap ends the read; reads = %d, result = %+v", gets, res)
	}
}

// A forge that announces the current page as the next one is a forge with
// nothing more to list: the read ends there.
func TestGitLabCreatePullReview_ASamePageAnnouncementEndsTheRead(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/24")
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/24/discussions", func(w http.ResponseWriter, r *http.Request) {
		storeDiscussion(w, r, &stored)
	})
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/24/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.Header().Set("X-Next-Page", r.URL.Query().Get("page"))
		_ = json.NewEncoder(w).Encode(renderDiscussions(stored))
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/24/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 24, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gets != 1 || !res.Verified || res.CommentsPosted != 1 {
		t.Fatalf("one page is the whole list; reads = %d, result = %+v", gets, res)
	}
}

// A rate-limited answer arrives from the transport as a typed error, not a
// status: it is still a 4xx the forge pronounced — a refusal that earns no
// second read — and its message, line breaks included, is flattened before
// it reaches the operator's log.
func TestGitLabCreatePullReview_ARateLimitedCreateIsARefusal(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/25")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/25/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("{\"message\":\"slow down\\nINFO forged line\"}"))
	})
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/25/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/25/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 25, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gets != 1 || res.CommentsPosted != 0 || !res.Verified || res.Fallback != "summary" || !strings.Contains(*noteBody, "a.go:1") {
		t.Fatalf("a rate limit is a refusal: one read, folded; reads = %d, result = %+v", gets, res)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "429") || strings.Contains(res.Diagnostics[0], "\n") || !strings.Contains(res.Diagnostics[0], "slow down INFO forged line") {
		t.Fatalf("diagnostics = %q", res.Diagnostics)
	}
}

// A review that landed nowhere (every create refused, the summary note too)
// is an error — and the diagnostics travel with it: what each create
// answered is the operator's only lead.
func TestGitLabCreatePullReview_TotalFailureStillCarriesTheDiagnostics(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/26")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/26/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":{"line_code":["can't be blank"]}}`))
	})
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/26/notes", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/26/discussions", &[]map[string]any{}, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 26, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	if err == nil {
		t.Fatalf("a review that landed nowhere must fail, got %+v", res)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "400") || !strings.Contains(res.Diagnostics[0], "line_code") {
		t.Fatalf("the diagnostics must travel with the error, got %v", res.Diagnostics)
	}
}

// A push during the publish makes GitLab trace a fresh note's position onto
// the new head — and the API exposes the traced position only. The read-back
// reads the merge request's head as it is now alongside the list, so the
// rescue of a create whose answer was lost still finds the thread.
func TestGitLabCreatePullReview_AThreadTracedOntoAPushedHeadIsStillFound(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	mrReads := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/27", func(w http.ResponseWriter, r *http.Request) {
		mrReads++
		head := "h"
		if mrReads > 1 {
			head = "h-after-push" // pushed between the anchoring read and the read-back
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": head, "start_sha": "s"}})
	})
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/27/discussions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["position"] = map[string]any{"new_path": "a.go", "new_line": 3, "head_sha": "h-after-push"}
		stored = append(stored, body)
		w.WriteHeader(http.StatusInternalServerError)
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/27/notes")
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/27/discussions", &stored, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 27, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 3, Body: "moved by a push"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 1 || !res.Verified || res.Fallback != "" || strings.Contains(*noteBody, "a.go:3") {
		t.Fatalf("the thread is on the merge request under the pushed head; result = %+v, note = %q", res, *noteBody)
	}
}

// The read-back runs under its own budget: a forge that never answers the
// discussion list costs the publish its verification, never the summary
// note — which follows on the caller's clock.
func TestGitLabCreatePullReview_TheReadBackHasItsOwnBudget(t *testing.T) {
	fastReadBack(t)
	prev := readBackBudget
	readBackBudget = 200 * time.Millisecond
	t.Cleanup(func() { readBackBudget = prev })
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/28")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/28/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "d0"})
	})
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/28/discussions", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // the client gave up
		case <-time.After(3 * time.Second):
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/28/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	start := time.Now()
	res, err := c.CreatePullReview(context.Background(), "g/p", 28, forge.NewReview{Body: "summary", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 2500*time.Millisecond { // the alternative is the 3 s hang
		t.Fatalf("the read-back overran its budget: %v", elapsed)
	}
	if res.Verified || res.CommentsPosted != 1 || *noteBody == "" || len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "budget") {
		t.Fatalf("unverified, the note posted, the budget named; result = %+v, note = %q", res, *noteBody)
	}
}

// The measured miscount on an instance whose list endpoint hiccups too:
// creates persisted behind error answers, and the first read fails. A
// create whose answer was lost keeps the fate open, so the failed read is
// repeated — the one shape where a second read still turns a duplicate into
// a count. Reported as such when every read fails.
func TestGitLabCreatePullReview_AFailedFirstReadIsRetriedWhileAFateIsOpen(t *testing.T) {
	fastReadBack(t)
	run := func(t *testing.T, readsFailing int) (forge.ReviewResult, int, string) {
		t.Helper()
		mux := http.NewServeMux()
		serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/29")
		var stored []map[string]any
		mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/29/discussions", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			stored = append(stored, body) // persisted behind an error answer
			w.WriteHeader(http.StatusInternalServerError)
		})
		gets := 0
		mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/29/discussions", func(w http.ResponseWriter, r *http.Request) {
			gets++
			if gets <= readsFailing {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(renderDiscussions(stored))
		})
		noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/29/notes")
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
		res, err := c.CreatePullReview(context.Background(), "g/p", 29, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{
			{Path: "a.ts", Line: 97, Body: "swallowed error"}, {Path: "a.ts", Line: 71, Body: "no transaction"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return res, gets, *noteBody
	}
	res, gets, note := run(t, 1)
	if res.CommentsPosted != 2 || !res.Verified || res.Fallback != "" || gets != 2 || strings.Contains(note, "a.ts:") {
		t.Fatalf("one hiccup: the second read counts both threads; result = %+v, reads = %d, note = %q", res, gets, note)
	}
	res, gets, _ = run(t, 1+readBackRetries)
	if res.Verified || res.CommentsPosted != 0 || gets != 1+readBackRetries || len(res.Diagnostics) != 3 || !strings.Contains(res.Diagnostics[2], "502") {
		t.Fatalf("every read failed: reported, bounded; result = %+v, reads = %d", res, gets)
	}
}

// A failed read is not repeated for creates whose fate is settled — a
// refusal here: nothing a second read could find.
func TestGitLabCreatePullReview_AFailedReadIsNotRetriedForSettledCreates(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/30")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/30/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/30/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.WriteHeader(http.StatusBadGateway)
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/30/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 30, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gets != 1 || res.Verified || res.CommentsPosted != 0 {
		t.Fatalf("a settled fate earns no second read; reads = %d, result = %+v", gets, res)
	}
}

// The budget ending in the pause keeps the verdict of the read that
// answered: read 1 found the thread persisted behind a 500 and missed the
// ghost (fate open, so a pause follows), and the budget ends inside the
// pause. Discarding read 1 would fold the held thread a second time.
func TestGitLabCreatePullReview_TheBudgetEndingInThePauseKeepsTheVerdict(t *testing.T) {
	fastReadBack(t)
	prevB, prevD := readBackBudget, readBackRetryDelay
	readBackBudget, readBackRetryDelay = 150*time.Millisecond, time.Second
	t.Cleanup(func() { readBackBudget, readBackRetryDelay = prevB, prevD })
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/31")
	var stored []map[string]any
	creates := 0
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/31/discussions", func(w http.ResponseWriter, r *http.Request) {
		creates++
		if creates == 1 {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			stored = append(stored, body) // persisted behind an error answer
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated) // a 2xx for a thread that is nowhere
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ghost"})
	})
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/31/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		_ = json.NewEncoder(w).Encode(renderDiscussions(stored))
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/31/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 31, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{
		{Path: "a.go", Line: 3, Body: "persisted behind a 500"},
		{Path: "b.go", Line: 4, Body: "ghost"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Verified || res.CommentsPosted != 1 || gets != 1 || strings.Contains(*noteBody, "a.go:3") {
		t.Fatalf("the answered read's verdict stands; result = %+v, reads = %d, note = %q", res, gets, *noteBody)
	}
}

// The summary note answering 429 (the likeliest refusal right after N
// creates) is a typed transport error — the OTHER return of the
// total-failure branch. The diagnostics travel with it too.
func TestGitLabCreatePullReview_ARateLimitedSummaryNoteStillCarriesTheDiagnostics(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/32")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/32/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":{"line_code":["can't be blank"]}}`))
	})
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/32/notes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"This endpoint has been requested too many times. Try again later."}`))
	})
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/32/discussions", &[]map[string]any{}, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 32, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	if err == nil {
		t.Fatalf("a review that landed nowhere must fail, got %+v", res)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "line_code") {
		t.Fatalf("err=%v: the diagnostics must travel with a typed note refusal too, got %v", err, res.Diagnostics)
	}
}

// A 2xx create is taken by its id whatever its note looks like now: GitLab
// traced it onto a newer head at create (a push raced the publish) and the
// API exposes the traced position only. Folding it would show the finding
// twice.
func TestGitLabCreatePullReview_ACreatedThreadIsTakenByIDWhateverItsPosition(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/33")
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/33/discussions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["position"] = map[string]any{"new_path": "a.go", "new_line": 7, "head_sha": "h-elsewhere"}
		stored = append(stored, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": discussionID(len(stored) - 1)})
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/33/notes")
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/33/discussions", &stored, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 33, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 3, Body: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 1 || !res.Verified || res.Fallback != "" || strings.Contains(*noteBody, "a.go:3") {
		t.Fatalf("the thread is ours by id; result = %+v, note = %q", res, *noteBody)
	}
}

// A proxy's error page is quoted as one bounded line: a multi-line HTML
// body must write neither lines of its own nor kilobytes into the log.
func TestGitLabCreatePullReview_AnErrorPageIsQuotedAsOneBoundedLine(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/34")
	page := "<html>\n<head><title>502 Bad Gateway</title></head>\n<body>\n" + strings.Repeat("<p>upstream timed out while reading response header</p>\n", 60) + "</body>\n</html>\n"
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/34/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(page))
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/34/notes")
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/34/discussions", &[]map[string]any{}, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 34, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diagnostics) != 1 || strings.Contains(res.Diagnostics[0], "\n") || len(res.Diagnostics[0]) > 400 {
		t.Fatalf("one bounded line expected, got %q", res.Diagnostics)
	}
}

// The budget label is the budget's alone: a read that answered 502 at once
// is a 502, and a caller giving up during the read-back is the caller's
// cancellation.
func TestGitLabCreatePullReview_OnlyTheBudgetIsLabelledTheBudget(t *testing.T) {
	fastReadBack(t)
	t.Run("a 502 is a 502", func(t *testing.T) {
		mux := http.NewServeMux()
		serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/35")
		mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/35/discussions", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "d0"})
		})
		mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/35/discussions", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		})
		captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/35/notes")
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
		res, err := c.CreatePullReview(context.Background(), "g/p", 35, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "502") || strings.Contains(res.Diagnostics[0], "budget") {
			t.Fatalf("diagnostics = %q", res.Diagnostics)
		}
	})
	t.Run("a caller cancellation is the caller's", func(t *testing.T) {
		mux := http.NewServeMux()
		serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/36")
		mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/36/discussions", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "d0"})
		})
		mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/36/discussions", func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})
		captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/36/notes")
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		res, _ := c.CreatePullReview(ctx, "g/p", 36, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
		for _, d := range res.Diagnostics {
			if strings.Contains(d, "budget") {
				t.Fatalf("the caller's deadline is not the read-back budget: %q", d)
			}
		}
	})
}

// ITERION_FORGE_READBACK_BUDGET is the operator's escape hatch on the
// read-back budget: a valid duration is the budget in force; a value that
// is not one leaves the default in force and says so in the diagnostics —
// never silently.
func TestGitLabCreatePullReview_TheReadBackBudgetHasAnOperatorOverride(t *testing.T) {
	fastReadBack(t)
	hang := func(mr int) (*AdminClient, func(), *string) {
		mux := http.NewServeMux()
		path := "/api/v4/projects/g%2Fp/merge_requests/" + strconv.Itoa(mr)
		serveMR(mux, path)
		mux.HandleFunc("POST "+path+"/discussions", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "d0"})
		})
		mux.HandleFunc("GET "+path+"/discussions", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		})
		noteBody := captureNote(mux, path+"/notes")
		srv := httptest.NewServer(mux)
		return &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}, srv.Close, noteBody
	}
	t.Run("a duration is the budget", func(t *testing.T) {
		t.Setenv(ReadBackBudgetEnv, "200ms")
		c, stop, _ := hang(37)
		defer stop()
		start := time.Now()
		res, err := c.CreatePullReview(context.Background(), "g/p", 37, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 2500*time.Millisecond || res.Verified || len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0], "budget of 200ms") {
			t.Fatalf("the override is the budget; elapsed = %v, result = %+v", elapsed, res)
		}
	})
	for _, bad := range []string{"soon", "0", "-1s", strings.Repeat("x", 10000)} {
		t.Run("not a positive duration: the default, said ("+bad+")", func(t *testing.T) {
			t.Setenv(ReadBackBudgetEnv, bad)
			prev := readBackBudget
			readBackBudget = 200 * time.Millisecond
			t.Cleanup(func() { readBackBudget = prev })
			c, stop, _ := hang(38)
			defer stop()
			res, err := c.CreatePullReview(context.Background(), "g/p", 38, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Diagnostics) != 2 || !strings.Contains(res.Diagnostics[0], ReadBackBudgetEnv) || !strings.Contains(res.Diagnostics[0], `"`+bad[:min(len(bad), 20)]) || !strings.Contains(res.Diagnostics[1], "budget of 200ms") {
				t.Fatalf("a bad override is named and the default applies; diagnostics = %q", res.Diagnostics)
			}
			if len(res.Diagnostics[0]) > 500 {
				t.Fatalf("the value is quoted bounded, got %d bytes", len(res.Diagnostics[0]))
			}
		})
	}
}

// The read-back's own failure is quoted on one line too: a typed answer
// carries the forge's message, line breaks included.
func TestGitLabCreatePullReview_AReadBackFailureIsOneLine(t *testing.T) {
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/39")
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/39/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "d0"})
	})
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/39/discussions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("{\"message\":\"too many\\nINFO forged line\"}"))
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/39/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 39, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || len(res.Diagnostics) != 1 || strings.Contains(res.Diagnostics[0], "\n") || !strings.Contains(res.Diagnostics[0], "429") || !strings.Contains(res.Diagnostics[0], "too many INFO forged line") {
		t.Fatalf("diagnostics = %q", res.Diagnostics)
	}
}

// The budget ending in the pause BEFORE any read answered reports the last
// read's failure: a "verified" result from a read-back no read answered
// would fold the held threads again under a clean name.
func TestGitLabCreatePullReview_TheBudgetEndingInThePauseBeforeAnyAnswerIsReported(t *testing.T) {
	fastReadBack(t)
	prevB, prevD := readBackBudget, readBackRetryDelay
	readBackBudget, readBackRetryDelay = 150*time.Millisecond, time.Second
	t.Cleanup(func() { readBackBudget, readBackRetryDelay = prevB, prevD })
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/40")
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/40/discussions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		stored = append(stored, body) // persisted behind an error answer: the fate is open
		w.WriteHeader(http.StatusInternalServerError)
	})
	gets := 0
	mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/40/discussions", func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.WriteHeader(http.StatusBadGateway)
	})
	captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/40/notes")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 40, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 3, Body: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || gets != 1 || res.CommentsPosted != 0 || len(res.Diagnostics) != 2 || !strings.Contains(res.Diagnostics[1], "502") || !strings.Contains(res.Diagnostics[1], "budget") {
		t.Fatalf("no read answered: unverified, the 502 and the budget named; result = %+v, reads = %d", res, gets)
	}
}

// The head read serves the key path alone, and a head read that fails
// never narrows what an earlier one accepted: a thread GitLab traced onto
// the pushed head stays counted when the head read fails on the last read;
// a publish whose creates all answered an id reads no head at all, so a
// hanging merge-request endpoint cannot cost it its verification; and when
// the budget runs out ON the head read, the head read is what the
// diagnostic blames.
func TestGitLabCreatePullReview_TheHeadReadServesTheKeyPathAlone(t *testing.T) {
	fastReadBack(t)
	t.Run("a failed head read keeps the last head", func(t *testing.T) {
		mux := http.NewServeMux()
		mrReads := 0
		mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/41", func(w http.ResponseWriter, r *http.Request) {
			mrReads++
			switch mrReads {
			case 1: // anchoring
				_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
			case 2: // the first read-back sees the push
				_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h-after-push", "start_sha": "s"}})
			default: // later head reads fail
				w.WriteHeader(http.StatusBadGateway)
			}
		})
		var stored []map[string]any
		creates := 0
		mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/41/discussions", func(w http.ResponseWriter, r *http.Request) {
			creates++
			if creates == 1 {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				body["position"] = map[string]any{"new_path": "a.go", "new_line": 3, "head_sha": "h-after-push"}
				stored = append(stored, body) // persisted, traced onto the pushed head
			}
			w.WriteHeader(http.StatusInternalServerError) // the second is lost: the fate stays open, every read runs
		})
		noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/41/notes")
		serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/41/discussions", &stored, 0)
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
		res, err := c.CreatePullReview(context.Background(), "g/p", 41, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{
			{Path: "a.go", Line: 3, Body: "traced onto the pushed head"}, {Path: "b.go", Line: 4, Body: "lost"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if res.CommentsPosted != 1 || !res.Verified || res.Fallback != "partial" || strings.Contains(*noteBody, "a.go:3") || mrReads != 4 {
			t.Fatalf("the pushed head read once keeps counting; result = %+v, note = %q, head reads = %d", res, *noteBody, mrReads)
		}
	})
	t.Run("all creates answered an id: no head read", func(t *testing.T) {
		prev := readBackBudget
		readBackBudget = 300 * time.Millisecond
		t.Cleanup(func() { readBackBudget = prev })
		mux := http.NewServeMux()
		mrReads := 0
		mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/42", func(w http.ResponseWriter, r *http.Request) {
			mrReads++
			if mrReads > 1 {
				select { // the merge-request endpoint hangs after the anchoring read
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
		})
		var stored []map[string]any
		mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/42/discussions", func(w http.ResponseWriter, r *http.Request) {
			storeDiscussion(w, r, &stored)
		})
		captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/42/notes")
		serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/42/discussions", &stored, 0)
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
		res, err := c.CreatePullReview(context.Background(), "g/p", 42, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}, {Path: "b.go", Line: 2, Body: "y"}}})
		if err != nil {
			t.Fatal(err)
		}
		// The fact, not the clock: one merge-request read (the anchoring
		// one) and a verified count — a head read would hang into the
		// budget and leave the count unverified.
		if !res.Verified || res.CommentsPosted != 2 || mrReads != 1 {
			t.Fatalf("ids need no head: verified at once; result = %+v, head reads = %d", res, mrReads)
		}
	})
	t.Run("the budget running out on the head read blames the head read", func(t *testing.T) {
		prev := readBackBudget
		readBackBudget = 200 * time.Millisecond
		t.Cleanup(func() { readBackBudget = prev })
		mux := http.NewServeMux()
		mrReads := 0
		mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/43", func(w http.ResponseWriter, r *http.Request) {
			mrReads++
			if mrReads > 1 {
				select {
				case <-r.Context().Done():
				case <-time.After(3 * time.Second):
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"web_url": "u", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "h", "start_sha": "s"}})
		})
		mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/43/discussions", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError) // no id: the key path, and the head read with it
		})
		gets := 0
		mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/43/discussions", func(w http.ResponseWriter, r *http.Request) {
			gets++
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		})
		noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/43/notes")
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
		res, err := c.CreatePullReview(context.Background(), "g/p", 43, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{{Path: "a.go", Line: 1, Body: "x"}}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Verified || gets != 0 || *noteBody == "" || len(res.Diagnostics) != 2 || !strings.Contains(res.Diagnostics[1], "merge request head read") || !strings.Contains(res.Diagnostics[1], "budget") || strings.Contains(res.Diagnostics[1], "page 1") {
			t.Fatalf("the head read is blamed, the note posted; result = %+v, list reads = %d", res, gets)
		}
	})
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

// Two decode traps around the read-modify-write, both of which must refuse
// loudly instead of writing:
//   - a 200 /user body without an `id` decodes to 0, and GitLab reads a 0 in
//     reviewer_ids as "add nobody" — the PUT would be a silent no-op logged
//     as success;
//   - a merge-request body without a `reviewers` field means the read half
//     saw nothing, and the replace-PUT would wipe reviewers it couldn't see.
func TestGitLabAddSelfAsPullReviewer_RefusesBlindWrites(t *testing.T) {
	run := func(userBody, mrBody string) (puts int, err error) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v4/user", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(userBody))
		})
		mux.HandleFunc("GET /api/v4/projects/g%2Fp/merge_requests/1", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(mrBody))
		})
		mux.HandleFunc("PUT /api/v4/projects/g%2Fp/merge_requests/1", func(w http.ResponseWriter, r *http.Request) {
			puts++
			_, _ = w.Write([]byte(`{}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
		err = c.AddSelfAsPullReviewer(context.Background(), "g/p", 1)
		return puts, err
	}

	if puts, err := run(`{"message":"401 Unauthorized"}`, `{"reviewers":[]}`); err == nil || puts != 0 {
		t.Fatalf("id-less /user body must refuse, not PUT a 0: puts=%d err=%v", puts, err)
	}
	if puts, err := run(`{"id":575,"username":"bot"}`, `{}`); err == nil || puts != 0 {
		t.Fatalf("reviewers-less MR body must refuse the replace-write: puts=%d err=%v", puts, err)
	}
	if puts, err := run(`{"id":575,"username":"bot"}`, `{"reviewers":null}`); err == nil || puts != 0 {
		t.Fatalf("null reviewers must refuse the replace-write: puts=%d err=%v", puts, err)
	}
	// And the honest empty set still writes.
	if puts, err := run(`{"id":575,"username":"bot"}`, `{"reviewers":[]}`); err != nil || puts != 1 {
		t.Fatalf("empty reviewer set is a real read — must write: puts=%d err=%v", puts, err)
	}
}
