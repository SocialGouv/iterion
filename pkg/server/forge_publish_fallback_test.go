package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	forgeforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
)

// Summary mode folds the request's comments into the review body in the
// handler, so the provider sees no inline comment and reports no fallback. The
// response must still say the findings are in the body, or a caller reading
// zero inline comments takes them for lost. A fold the handler did not make
// (nothing left to fold, or inline mode) is not claimed, and a provider's own
// report passes through untouched -- unless the handler dropped a comment.
// The fallback describes what the CALLER submitted, while the provider saw
// only what was kept: a comment dropped as unanchorable is in neither the
// body nor inline (dropped_comments names it), so fewer of the submitted
// comments are on the pull request than were submitted. That is "partial" in
// every mode, never "summary" -- not even when the provider folded every
// comment it was given (GitHub's and Forgejo's 422 path, GitLab's zero
// landed), and not "" when every kept one landed.
func TestForgePublishReview_SummaryModeReportsItsFold(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		providerFb   string
		wantFallback string
		wantFolded   bool
		wantDropped  int
	}{
		{
			name: "summary mode with comments reports the fold",
			body: `{"pr_url": "https://github.com/o/r/pull/42", "summary": "sum", "mode": "summary",
			        "comments": [{"path": "a.go", "line": 3, "body": "FINDING-ONE"}, {"path": "b.go", "line": 9, "body": "FINDING-TWO"}]}`,
			wantFallback: "summary", wantFolded: true,
		},
		{
			name: "summary mode with a comment dropped as unanchorable is partial",
			body: `{"pr_url": "https://github.com/o/r/pull/42", "summary": "sum", "mode": "summary",
			        "comments": [{"path": "a.go", "line": 3, "body": "FINDING-ONE"}, {"path": "b.go", "line": 0, "body": "FINDING-TWO"}]}`,
			wantFallback: "partial", wantFolded: true, wantDropped: 1,
		},
		{
			name: "inline mode with a comment dropped is partial even when the provider folded the rest",
			body: `{"pr_url": "https://github.com/o/r/pull/42", "summary": "sum", "mode": "inline",
			        "comments": [{"path": "a.go", "line": 3, "body": "FINDING-ONE"}, {"path": "b.go", "line": 0, "body": "FINDING-TWO"}]}`,
			providerFb: "summary", wantFallback: "partial", wantDropped: 1,
		},
		{
			name: "inline mode with a comment dropped is partial even when every kept one landed",
			body: `{"pr_url": "https://github.com/o/r/pull/42", "summary": "sum",
			        "comments": [{"path": "a.go", "line": 3, "body": "FINDING-ONE"}, {"path": "", "line": 9, "body": "FINDING-TWO"}]}`,
			wantFallback: "partial", wantDropped: 1,
		},
		{
			name:         "summary mode without comments folds nothing",
			body:         `{"pr_url": "https://github.com/o/r/pull/42", "summary": "sum", "mode": "summary"}`,
			wantFallback: "",
		},
		{
			name: "summary mode whose comments were all unanchorable folds nothing and is partial",
			body: `{"pr_url": "https://github.com/o/r/pull/42", "summary": "sum", "mode": "summary",
			        "comments": [{"path": "", "line": 3, "body": "FINDING-ONE"}]}`,
			wantFallback: "partial", wantDropped: 1,
		},
		{
			name: "inline mode keeps the provider's empty report",
			body: `{"pr_url": "https://github.com/o/r/pull/42", "summary": "sum",
			        "comments": [{"path": "a.go", "line": 3, "body": "FINDING-ONE"}]}`,
			wantFallback: "",
		},
		{
			name: "inline mode passes the provider's shortfall through",
			body: `{"pr_url": "https://github.com/o/r/pull/42", "summary": "sum",
			        "comments": [{"path": "a.go", "line": 3, "body": "FINDING-ONE"}]}`,
			providerFb: "partial", wantFallback: "partial",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake := newForgePublishTestServer(t)
			registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
			fake.res = forge.ReviewResult{URL: "https://github.com/o/r/pull/42#review-1", Verified: true, Fallback: tc.providerFb}

			w := httptest.NewRecorder()
			s.handleForgePublishReview(w, publishReq("tok1", tc.body))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			var resp publishReviewResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Fallback != tc.wantFallback {
				t.Errorf("response fallback = %q, want %q (response %s)", resp.Fallback, tc.wantFallback, w.Body.String())
			}
			if len(resp.DroppedComments) != tc.wantDropped {
				t.Errorf("dropped_comments = %q, want %d entr(ies)", resp.DroppedComments, tc.wantDropped)
			}
			folded := strings.Contains(fake.in.Body, "FINDING-ONE")
			if folded != tc.wantFolded {
				t.Errorf("review body carries the findings = %v, want %v (body %q)", folded, tc.wantFolded, fake.in.Body)
			}
			if tc.wantFolded && len(fake.in.Comments) != 0 {
				t.Errorf("a summary fold posts no inline comment: %+v", fake.in.Comments)
			}
			if inBody := strings.Contains(fake.in.Body, "FINDING-TWO"); tc.wantFolded && inBody != (tc.wantDropped == 0) {
				t.Errorf("FINDING-TWO in the body = %v; want it there exactly when nothing was dropped (body %q)", inBody, fake.in.Body)
			}
			if tc.wantDropped > 0 && strings.Contains(fake.in.Body, "FINDING-TWO") {
				t.Errorf("the dropped comment reached the body, so it is not lost: %q", fake.in.Body)
			}
			for _, c := range fake.in.Comments {
				if tc.wantDropped > 0 && strings.Contains(c.Body, "FINDING-TWO") {
					t.Errorf("the dropped comment reached the provider inline, so it is not lost: %+v", c)
				}
			}
		})
	}
}

// fakeForgeReviewAPI answers a review create with 200 and the created review's
// comment listing with an empty array: the re-fetch that finds fewer comments
// than were sent. It records every create body.
type fakeForgeReviewAPI struct {
	mu    sync.Mutex
	posts []string
}

func (f *fakeForgeReviewAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
		f.mu.Lock()
		f.posts = append(f.posts, string(body))
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"id":7,"html_url":"https://github.com/o/r/pull/42#review-7"}`)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/reviews/7/comments"):
		_, _ = io.WriteString(w, `[]`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeForgeReviewAPI) createBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...)
}

// What the real GitHub and Forgejo clients answer behind the handler. A
// re-fetch that lists fewer comments than were sent is "partial" with nothing
// folded: the body the forge received is the summary as submitted, and with
// zero comments stored the response carries comments_posted 0 — the shape
// review-pr's publish_health must read as MISSING, never as a fold (the bot
// side, TestReviewPRPublishChainReadsOnlyARealFold, feeds it this answer).
// Summary mode is "summary", with the findings in the body the forge received.
func TestForgePublishReview_FallbackFromRealClients(t *testing.T) {
	const inline = `{"pr_url": "https://github.com/o/r/pull/42", "summary": "SUMMARY-BODY", "mode": "inline",
	  "comments": [{"path": "a.go", "line": 1, "body": "FINDING-ONE"}, {"path": "b.go", "line": 9, "body": "FINDING-TWO"}]}`
	const summary = `{"pr_url": "https://github.com/o/r/pull/42", "summary": "SUMMARY-BODY", "mode": "summary",
	  "comments": [{"path": "a.go", "line": 1, "body": "FINDING-ONE"}, {"path": "b.go", "line": 9, "body": "FINDING-TWO"}]}`
	clients := []struct {
		name string
		mk   func(srv *httptest.Server) forge.ReviewClient
	}{
		{"github", func(srv *httptest.Server) forge.ReviewClient {
			return &forgegithub.AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "x"}
		}},
		{"forgejo", func(srv *httptest.Server) forge.ReviewClient {
			return &forgeforgejo.AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "x"}
		}},
	}
	for _, c := range clients {
		for _, tc := range []struct {
			name         string
			body         string
			wantFallback string
			wantInBody   bool
		}{
			{name: "inline, re-fetch lists none", body: inline, wantFallback: "partial"},
			{name: "summary mode", body: summary, wantFallback: "summary", wantInBody: true},
		} {
			t.Run(c.name+"/"+tc.name, func(t *testing.T) {
				api := &fakeForgeReviewAPI{}
				srv := httptest.NewServer(api)
				defer srv.Close()
				rc := c.mk(srv)
				s, _ := newForgePublishTestServer(t)
				s.forgeReviewClientFor = func(context.Context, forge.Connection) (forge.ReviewClient, error) { return rc, nil }
				gc := &fakeGateClient{headSHA: "abc"}
				s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
				registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

				w := httptest.NewRecorder()
				s.handleForgePublishReview(w, publishReq("tok1", tc.body))
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", w.Code, w.Body.String())
				}
				var resp publishReviewResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if !resp.Published || resp.CommentsPosted != 0 || resp.BodyMissing || resp.Fallback != tc.wantFallback {
					t.Fatalf("response = %s; want published, comments_posted 0, body present, fallback %q", w.Body.String(), tc.wantFallback)
				}

				posts := api.createBodies()
				if len(posts) != 1 {
					t.Fatalf("forge received %d review create(s), want 1: %q", len(posts), posts)
				}
				var sent struct {
					Body     string            `json:"body"`
					Comments []json.RawMessage `json:"comments"`
				}
				if err := json.Unmarshal([]byte(posts[0]), &sent); err != nil {
					t.Fatalf("create body is not JSON: %v (%q)", err, posts[0])
				}
				inBody := strings.Contains(sent.Body, "FINDING-ONE") && strings.Contains(sent.Body, "FINDING-TWO")
				if inBody != tc.wantInBody {
					t.Errorf("review body carries the findings = %v, want %v (body %q)", inBody, tc.wantInBody, sent.Body)
				}
				if !tc.wantInBody && (sent.Body != "SUMMARY-BODY" || len(sent.Comments) != 2) {
					t.Errorf("a shortfall folds nothing: body %q (want the summary as submitted), %d inline comment(s) sent (want 2)", sent.Body, len(sent.Comments))
				}
				if tc.wantInBody && len(sent.Comments) != 0 {
					t.Errorf("summary mode posts no inline comment, sent %d", len(sent.Comments))
				}
			})
		}
	}
}
