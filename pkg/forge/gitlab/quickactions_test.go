package gitlab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// gitlabCommandLine is GitLab's command matcher (extractor.rb,
// commands_regex), ported — the oracle the write witnesses measure against.
// pkg/forge's own test carries the same port; a sample of command names is
// enough, the guard does not depend on the list.
var gitlabCommandLine = regexp.MustCompile(`(?mi)^/(?:merge|approve|close|reopen|label|assign|assign_reviewer|title|due|spend|draft|ready|rebase|lock|confidential|target_branch|shrug)(?: [^\n]*)?(?:\s*\n|$)`)

func gitlabExecutes(text string) []string {
	return gitlabCommandLine.FindAllString(strings.ReplaceAll(text, "\r", ""), -1)
}

// hostileText is what a prompt-injected model can put in anything iterion
// posts to GitLab; "keep me" is the prose that must survive.
const hostileText = "keep me\n/merge\r\n  /approve\n/label ~security\n/ſpend 1h"

// Every GitLab write that carries text GitLab parses for quick actions —
// notes on issues and merge requests, issue and merge-request descriptions
// on create and update — reaches the forge with no command GitLab's matcher
// would run, and the prose intact.
func TestGitLabWritesCarryNoQuickAction(t *testing.T) {
	t.Setenv(forge.GitLabQuickActionsEnv, "")
	if len(gitlabExecutes(hostileText)) == 0 {
		t.Fatal("oracle blind: GitLab's matcher finds no command in the hostile text")
	}
	var sent []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			raw, _ := io.ReadAll(r.Body)
			var payload map[string]any
			_ = json.Unmarshal(raw, &payload)
			for _, k := range []string{"body", "description"} {
				if v, ok := payload[k].(string); ok {
					sent = append(sent, v)
				}
			}
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "iid": 1, "web_url": "u", "source_project_id": 1, "target_project_id": 1})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	ctx := context.Background()
	body := hostileText
	writers := map[string]func() error{
		"CommentIssue":       func() error { _, err := c.CommentIssue(ctx, "g/p", 1, body); return err },
		"CommentPullRequest": func() error { _, err := c.CommentPullRequest(ctx, "g/p", 1, body); return err },
		"CreateIssue":        func() error { _, err := c.CreateIssue(ctx, "g/p", forge.NewIssue{Title: "t", Body: body}); return err },
		"UpdateIssue":        func() error { _, err := c.UpdateIssue(ctx, "g/p", 1, forge.IssuePatch{Body: &body}); return err },
		"CreatePull": func() error {
			_, err := c.CreatePull(ctx, "g/p", forge.NewPull{Title: "t", SourceBranch: "a", TargetBranch: "main", Body: body})
			return err
		},
		"UpdatePull": func() error { _, err := c.UpdatePull(ctx, "g/p", 1, forge.PullPatch{Body: &body}); return err },
	}
	for name, write := range writers {
		sent = nil
		if err := write(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(sent) != 1 {
			t.Fatalf("%s: %d text field(s) sent, want 1", name, len(sent))
		}
		if got := gitlabExecutes(sent[0]); len(got) != 0 || !strings.HasPrefix(sent[0], "keep me\n") {
			t.Fatalf("%s sent %q: GitLab would run %q", name, sent[0], got)
		}
	}
}

// A review posts no command either — not in a finding, not in the summary —
// while a replacement that is plain code stays a byte-exact one-click
// suggestion. A replacement with a command-shaped line cannot be made inert
// without changing the code the click applies: it is shown as text, inert,
// and not counted as a suggestion.
func TestGitLabCreatePullReviewPostsNoQuickAction(t *testing.T) {
	t.Setenv(forge.GitLabQuickActionsEnv, "")
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/50")
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/50/discussions", func(w http.ResponseWriter, r *http.Request) {
		storeDiscussion(w, r, &stored)
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/50/notes")
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/50/discussions", &stored, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 50, forge.NewReview{
		Body: "summary\n/approve",
		Comments: []forge.ReviewComment{
			{Path: "a.go", Line: 3, Body: "finding\n/merge", Suggestion: "// fixed\nx := 1\n"},
			{Path: "b.go", Line: 9, Body: "plain", Suggestion: "routes:\n/merge now\n"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 2 || res.SuggestionsPosted != 1 || !res.Verified {
		t.Fatalf("two threads, one one-click suggestion; result = %+v", res)
	}
	first, _ := stored[0]["body"].(string)
	second, _ := stored[1]["body"].(string)
	for _, posted := range []string{first, second, *noteBody} {
		if got := gitlabExecutes(posted); len(got) != 0 {
			t.Fatalf("GitLab would run %q from %q", got, posted)
		}
	}
	if !strings.Contains(first, "```suggestion:-0+0\n// fixed\nx := 1\n```") {
		t.Fatalf("a plain-code replacement must stay byte-exact and one-click, got %q", first)
	}
	if strings.Contains(second, "```suggestion") || !strings.Contains(second, `\/merge now`) {
		t.Fatalf("a replacement with a command-shaped line is shown as text, inert, got %q", second)
	}
	if !strings.HasPrefix(*noteBody, "summary\n") {
		t.Fatalf("the summary's prose must survive, got %q", *noteBody)
	}
}

// A finding's own text can carry a suggestion fence, which GitLab makes
// applicable like the one the client appends. When the guard rewrites a
// line of that text, a fence it carries would apply the rewritten code: it
// becomes a plain `text` block — never a bare fence, which could close an
// outer block and swallow the appended suggestion. Text the guard leaves
// alone keeps its fence, indented code in a list item or a footnote
// included; a replacement whose slash lines are indented — never run by
// GitLab — stays one-click.
func TestGitLabCommentBody_NoRewrittenCodeIsOneClickAway(t *testing.T) {
	t.Setenv(forge.GitLabQuickActionsEnv, "")
	rewritten, _ := renderGitLabCommentBody(forge.ReviewComment{Path: "a", Line: 1,
		Body: "ignore these:\n```suggestion:-0+1\n/node_modules\n/dist\n```"})
	if strings.Contains(rewritten, "```suggestion") || !strings.Contains(rewritten, "```text:-0+1\n\\/node_modules\n\\/dist\n```") {
		t.Fatalf("a rewritten fence must turn into a text block, got %q", rewritten)
	}
	// A backtick line whose info string holds a backtick opens no block; the
	// rename keeps every byte but the word, so it still opens none and the
	// appended suggestion stays a fence of its own.
	opener, openerFenced := renderGitLabCommentBody(forge.ReviewComment{Path: "a", Line: 1,
		Body:       "/etc/hosts is world-writable\n```suggestion `x` is how GitLab fences a fix.\n\nMore prose.",
		Suggestion: "fixed()\n"})
	if !openerFenced || !strings.Contains(opener, "\n```text `x` is how GitLab fences a fix.\n\nMore prose.\n\n```suggestion:-0+0\nfixed()\n```") {
		t.Fatalf("the demotion must rename the word only, got %q", opener)
	}
	nested, nestedFenced := renderGitLabCommentBody(forge.ReviewComment{Path: "a", Line: 1,
		Body:       "The doc's example is wrong:\n```markdown\nUse:\n```suggestion:-0+1\n/approve this\n```\n",
		Suggestion: "fixed()\n"})
	if !nestedFenced || !strings.Contains(nested, "```text:-0+1\n\\/approve this") || !strings.HasSuffix(nested, "```suggestion:-0+0\nfixed()\n```") {
		t.Fatalf("a demoted fence nested in a block keeps an info string, the appended suggestion stays whole, got %q", nested)
	}
	for _, contained := range []string{
		"- ```suggestion:-0+0\n  /node_modules\n  ```",
		"* first point\n\n    ```suggestion\n    /node_modules\n    ```",
		"see[^1]\n\n[^1]: note\n\n    ```suggestion\n    /node_modules\n    ```",
		"term\n: ```suggestion\n  /node_modules\n  ```",
	} {
		if got, _ := renderGitLabCommentBody(forge.ReviewComment{Path: "a", Line: 1, Body: contained}); got != contained {
			t.Fatalf("indented code never runs and must keep its bytes:\n sent %q\n want %q", got, contained)
		}
	}
	kept, _ := renderGitLabCommentBody(forge.ReviewComment{Path: "a", Line: 1,
		Body: "ignore these:\n```suggestion:-0+1\nnode_modules/\n```"})
	if !strings.Contains(kept, "```suggestion:-0+1\nnode_modules/\n```") {
		t.Fatalf("text the guard leaves alone keeps its fence, got %q", kept)
	}
	indented, fenced := renderGitLabCommentBody(forge.ReviewComment{Path: "a", Line: 1, Body: "fix",
		Suggestion: "paths:\n  /users/{id}:\n    get: {}\n"})
	if !fenced || !strings.Contains(indented, "```suggestion:-0+0\npaths:\n  /users/{id}:\n    get: {}\n```") {
		t.Fatalf("an indented slash line never runs: the replacement stays byte-exact and one-click, got %q", indented)
	}
}

// GitLab strips a quick-action line from the note it stores, so a body sent
// with one could never match its stored twin on the read-back's rescue path
// (a create whose answer was lost). Made inert, the line is stored as sent,
// and the rescue finds the thread instead of folding it a second time.
func TestGitLabCreatePullReview_AnInertBodyIsRescuedByTheReadBack(t *testing.T) {
	t.Setenv(forge.GitLabQuickActionsEnv, "")
	fastReadBack(t)
	mux := http.NewServeMux()
	serveMR(mux, "/api/v4/projects/g%2Fp/merge_requests/51")
	var stored []map[string]any
	mux.HandleFunc("POST /api/v4/projects/g%2Fp/merge_requests/51/discussions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		stored = append(stored, body) // persisted behind an error answer
		w.WriteHeader(http.StatusInternalServerError)
	})
	noteBody := captureNote(mux, "/api/v4/projects/g%2Fp/merge_requests/51/notes")
	serveDiscussionsReadBack(mux, "/api/v4/projects/g%2Fp/merge_requests/51/discussions", &stored, 0)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), BaseURL: srv.URL, Token: "t"}
	res, err := c.CreatePullReview(context.Background(), "g/p", 51, forge.NewReview{Body: "s", Comments: []forge.ReviewComment{
		{Path: "a.go", Line: 3, Body: "finding\n/rebase"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.CommentsPosted != 1 || !res.Verified || res.Fallback != "" || strings.Contains(*noteBody, "a.go:3") {
		t.Fatalf("the stored thread is the one sent; result = %+v, note = %q", res, *noteBody)
	}
}
