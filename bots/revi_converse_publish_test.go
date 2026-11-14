package bots

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/deeplink"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// publish_reply is revi-converse's only forge write: a deterministic node
// that posts the agent's draft in the conversation's thread with the mounted
// forge_token and re-reads the created note by its id. These tests run its
// command as the engine renders it, against a fake forge serving the REST
// shape each forge documents; the fake records every request, so "exactly
// one write" is the forge's own count, not the node's claim.

const (
	publishRunID  = "run-converse"
	publishRunURL = "https://iterion.example/studio/runs/run-converse"
	publishToken  = "forge-token-publish-7c1e"
	publishNoteID = 9001
	// The footer every post ends with (docs/agents/bots/run-link.md).
	publishFooter = "\n\n---\n🤖 [iterion](https://iterion.cloud) · run [`run-converse`](https://iterion.example/studio/runs/run-converse)"
)

// The answer's one model call is the agent's: posting it and judging the
// post are deterministic tool nodes, so publishing costs no LLM and cannot
// run out of tool steps. The workflow's nodes are exactly the drafting
// agent and the two tools, chained in that order.
func TestReviConversePublishesWithoutAModel(t *testing.T) {
	wf := compileBotFile(t, filepath.Join(botsDir(t), "revi-converse", "main.bot"))
	var ids []string
	for id, n := range wf.Nodes {
		switch n.(type) {
		case *ir.DoneNode, *ir.FailNode:
			continue // the terminals, which run nothing
		}
		ids = append(ids, id)
		switch id {
		case "converse_agent":
			if _, ok := n.(*ir.AgentNode); !ok {
				t.Errorf("converse_agent is %T, want the one *ir.AgentNode", n)
			}
		case "publish_reply", "converse_health":
			if _, ok := n.(*ir.ToolNode); !ok {
				t.Errorf("%s is %T, want a deterministic *ir.ToolNode — publishing must not spend a model call", id, n)
			}
		}
	}
	slices.Sort(ids)
	if want := []string{"converse_agent", "converse_health", "publish_reply"}; !slices.Equal(ids, want) {
		t.Errorf("revi-converse nodes = %v, want %v", ids, want)
	}
	var chain []string
	for _, e := range wf.Edges {
		chain = append(chain, e.From+"->"+e.To)
	}
	slices.Sort(chain)
	if want := []string{"converse_agent->publish_reply", "converse_health->done", "publish_reply->converse_health"}; !slices.Equal(chain, want) {
		t.Errorf("revi-converse edges = %v, want %v", chain, want)
	}
}

// renderReviConversePublish renders publish_reply's command with the engine's
// renderer: the edge's input, the run's vars and the run namespace. A preview
// rendering has no secret guard, so `{{secrets.forge_token.path}}` comes out
// as the secret's placeholder; the run's guard renders the mounted path
// there, and so does this helper (the chain test below renders through a
// real guard). Every other reference must resolve.
func renderReviConversePublish(t *testing.T, input, vars map[string]any, runURL, tokenPath string) string {
	t.Helper()
	wf := compileBotFile(t, filepath.Join(botsDir(t), "revi-converse", "main.bot"))
	tn, ok := wf.Nodes["publish_reply"].(*ir.ToolNode)
	if !ok {
		t.Fatalf("publish_reply is %T, want a deterministic *ir.ToolNode", wf.Nodes["publish_reply"])
	}
	shapes := model.WorkflowShapes(wf).WithInputSchema(wf.Schemas[tn.InputSchema])
	td := &model.TemplateData{RunID: publishRunID, Run: map[string]any{"id": publishRunID, "url": runURL}}
	var unresolved []string
	command := model.RenderCommand(tn.Command, tn.CommandRefs, input, vars, td, publishRunID, shapes, func(ref string) {
		unresolved = append(unresolved, ref)
	})
	if len(unresolved) != 0 {
		t.Fatalf("publish_reply left %v unresolved — the case does not drive the input it names", unresolved)
	}
	ph := secretguard.PlaceholderForName("forge_token")
	quoted := "'" + ph + "'"
	switch {
	case strings.Count(command, quoted) == 1:
		command = strings.Replace(command, quoted, shellSingleQuote(tokenPath), 1)
	case strings.Count(command, ph) == 1:
		command = strings.Replace(command, ph, shellSingleQuote(tokenPath), 1)
	default:
		t.Fatalf("publish_reply renders the forge_token reference %d times, want once", strings.Count(command, ph))
	}
	return command
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// noProxyEnv is this process's environment without its proxies: the fake
// forge listens on loopback and must be reached directly.
func noProxyEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasSuffix(strings.ToLower(name), "_proxy") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

type publishOut struct {
	Posted        *bool  `json:"posted"`
	Forge         string `json:"forge"`
	ReplyURL      string `json:"reply_url"`
	AnswerPreview string `json:"answer_preview"`
	SkippedReason string `json:"skipped_reason"`
	Summary       string `json:"summary"`
}

// runPublish executes a rendered command under shell and decodes its output.
// A command past 100000 bytes goes to the shell on stdin, as the executor
// streams it (toolNodeCommand): one exec argument is capped at 128 KiB.
func runPublish(t *testing.T, shell, command string, env []string) (publishOut, string) {
	t.Helper()
	cmd := exec.Command(shell, "-c", command)
	if len(command) > 100_000 {
		cmd = exec.Command(shell, "-s")
		cmd.Stdin = strings.NewReader(command)
	}
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: publish_reply failed: %v (out %q, stderr %q)", shell, err, out, stderr.String())
	}
	var res publishOut
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("%s: output is not publish_reply_output JSON: %v (%q, stderr %q)", shell, err, out, stderr.String())
	}
	if res.Posted == nil {
		t.Fatalf("%s: output carries no posted field: %q", shell, out)
	}
	if strings.Contains(string(out), publishToken) {
		t.Fatalf("%s: the forge token reached the node's output: %q", shell, out)
	}
	return res, string(out)
}

func writeToken(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "forge_token")
	if err := os.WriteFile(p, []byte(publishToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// publishShells are the shells the command must run under: bash is the
// executor's, sh proves the command stays POSIX.
func publishShells(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	var shells []string
	for _, sh := range []string{"bash", "sh"} {
		if _, err := exec.LookPath(sh); err == nil {
			shells = append(shells, sh)
		}
	}
	if len(shells) == 0 {
		t.Skip("no shell on PATH")
	}
	return shells
}

// forgeHit is one request the fake forge received.
type forgeHit struct {
	Method, Path, Auth, PrivateToken, Body string
}

// converseFakeForge creates a note on any POST and serves the last one on
// any GET; the tests assert the paths. Its knobs refuse the POST, drop the
// note's id, refuse the re-read, or redirect the POST.
type converseFakeForge struct {
	srv *httptest.Server

	htmlURL    string // the note's html_url ("" for GitLab, whose notes carry none)
	postStatus int    // non-zero: answer the POST with this status and postAnswer
	postAnswer string
	postNoID   bool
	readStatus int    // non-zero: answer every GET with this status
	readOther  bool   // answer every GET with another note than the one created
	redirectTo string // non-empty: answer the POST with a 302 to it

	mu   sync.Mutex
	hits []forgeHit
	last string
}

func newConverseFakeForge(t *testing.T) *converseFakeForge {
	t.Helper()
	f := &converseFakeForge{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *converseFakeForge) note(body string) map[string]any {
	n := map[string]any{"id": publishNoteID, "body": body}
	if f.htmlURL != "" {
		n["html_url"] = f.htmlURL
	}
	return n
}

func (f *converseFakeForge) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, forgeHit{
		Method: r.Method, Path: r.URL.EscapedPath(), Auth: r.Header.Get("Authorization"),
		PrivateToken: r.Header.Get("PRIVATE-TOKEN"), Body: string(raw),
	})
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodPost:
		if f.redirectTo != "" {
			w.Header().Set("Location", f.redirectTo)
			w.WriteHeader(http.StatusFound)
			return
		}
		if f.postStatus != 0 {
			w.WriteHeader(f.postStatus)
			_, _ = io.WriteString(w, f.postAnswer)
			return
		}
		var p struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(raw, &p); err != nil || p.Body == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"body is missing"}`)
			return
		}
		f.last = p.Body
		n := f.note(p.Body)
		if f.postNoID {
			delete(n, "id")
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(n)
	case http.MethodGet:
		if f.readStatus != 0 {
			w.WriteHeader(f.readStatus)
			_, _ = io.WriteString(w, `{"message":"404 Not found"}`)
			return
		}
		if f.last == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"404 Not found"}`)
			return
		}
		n := f.note(f.last)
		if f.readOther {
			n["id"] = publishNoteID - 1
		}
		_ = json.NewEncoder(w).Encode(n)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *converseFakeForge) seen() []forgeHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hits)
}

// writes are the requests that change the forge: everything but a GET.
func writes(hits []forgeHit) []forgeHit {
	var w []forgeHit
	for _, h := range hits {
		if h.Method != http.MethodGet {
			w = append(w, h)
		}
	}
	return w
}

func postedBody(t *testing.T, h forgeHit) string {
	t.Helper()
	var p struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(h.Body), &p); err != nil {
		t.Fatalf("the POST body is not {\"body\": …} JSON: %v (%q)", err, h.Body)
	}
	return p.Body
}

// publishAnswer carries a line GitLab would run as a quick action, quoted
// from a diff: posted verbatim where no forge runs it, made inert on GitLab.
const publishAnswer = "Re: op — « why does A return 2 now? »\n\n" +
	"`A` returns 2 because the change rewrites its body (app.go:3).\n" +
	"/approve\n" +
	"That line is quoted from the diff, not an order."

// publishForgeCase is one forge's reply lane: where the PR lives on the fake
// host, the thread, and what the forge must see.
type publishForgeCase struct {
	name      string
	prPath    string
	disc      string
	wantForge string
	wantPost  string // the reply POST, escaped
	wantRead  string // the re-read by id, escaped
	wantAuth  string // the Authorization header
	// htmlURL is the note's web URL as the forge names it, from the fake's
	// origin and the PR URL; empty on GitLab, whose notes carry none.
	htmlURL   func(origin, pr string) string
	wantReply func(origin, pr string) string
	// wantBody is the body before the footer.
	wantBody string
}

func publishForgeCases() []publishForgeCase {
	gitlabBody := forge.InertGitLabQuickActions(publishAnswer)
	return []publishForgeCase{
		{
			name: "github issue comment (the /revi <question> lane)", prPath: "/o/r/pull/42", wantForge: "github",
			wantPost: "/api/v3/repos/o/r/issues/42/comments", wantRead: "/api/v3/repos/o/r/issues/comments/9001",
			wantAuth:  "Bearer " + publishToken,
			htmlURL:   func(_, pr string) string { return pr + "#issuecomment-9001" },
			wantReply: func(_, pr string) string { return pr + "#issuecomment-9001" },
			wantBody:  publishAnswer,
		},
		{
			name: "github review-thread reply", prPath: "/o/r/pull/42", disc: "4242", wantForge: "github",
			wantPost: "/api/v3/repos/o/r/pulls/42/comments/4242/replies", wantRead: "/api/v3/repos/o/r/pulls/comments/9001",
			wantAuth:  "Bearer " + publishToken,
			htmlURL:   func(_, pr string) string { return pr + "#discussion_r9001" },
			wantReply: func(_, pr string) string { return pr + "#discussion_r9001" },
			wantBody:  publishAnswer,
		},
		{
			name: "gitlab discussion note", prPath: "/g/sub/p/-/merge_requests/7", disc: "6a1f0c", wantForge: "gitlab",
			wantPost: "/api/v4/projects/g%2Fsub%2Fp/merge_requests/7/discussions/6a1f0c/notes", wantRead: "/api/v4/projects/g%2Fsub%2Fp/merge_requests/7/notes/9001",
			wantAuth:  "Bearer " + publishToken,
			htmlURL:   func(string, string) string { return "" },
			wantReply: func(_, pr string) string { return pr + "#note_9001" },
			wantBody:  gitlabBody,
		},
		{
			name: "gitlab merge request note (no discussion id)", prPath: "/g/p/-/merge_requests/7", wantForge: "gitlab",
			wantPost: "/api/v4/projects/g%2Fp/merge_requests/7/notes", wantRead: "/api/v4/projects/g%2Fp/merge_requests/7/notes/9001",
			wantAuth:  "Bearer " + publishToken,
			htmlURL:   func(string, string) string { return "" },
			wantReply: func(_, pr string) string { return pr + "#note_9001" },
			wantBody:  gitlabBody,
		},
		{
			name: "forgejo issue comment", prPath: "/o/r/pulls/7", wantForge: "forgejo",
			wantPost: "/api/v1/repos/o/r/issues/7/comments", wantRead: "/api/v1/repos/o/r/issues/comments/9001",
			wantAuth:  "token " + publishToken,
			htmlURL:   func(_, pr string) string { return pr + "#issuecomment-9001" },
			wantReply: func(_, pr string) string { return pr + "#issuecomment-9001" },
			wantBody:  publishAnswer,
		},
	}
}

// Each forge's lane: exactly one write, at the reply endpoint the forge
// documents, with that forge's auth header, carrying the answer and the run
// footer; then the re-read of the created note by its id, and reply_url the
// note's web URL on the PR's host. On GitLab the `/approve` the answer
// quotes reaches the forge inert (the server's own predicate reads the
// posted body); elsewhere the body is the answer byte for byte.
func TestReviConversePublishReplyPostsOnEachForge(t *testing.T) {
	shells := publishShells(t)
	for _, tc := range publishForgeCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, shell := range shells {
				f := newConverseFakeForge(t)
				pr := f.srv.URL + tc.prPath
				f.htmlURL = tc.htmlURL(f.srv.URL, pr)
				cmd := renderReviConversePublish(t,
					map[string]any{"answer": publishAnswer, "skipped_reason": ""},
					map[string]any{"pr_url": pr, "discussion_id": tc.disc},
					publishRunURL, writeToken(t))
				res, raw := runPublish(t, shell, cmd, noProxyEnv())

				if !*res.Posted || res.SkippedReason != "" {
					t.Fatalf("%s: posted = %v, skipped_reason %q, want a posted reply (%s)", shell, *res.Posted, res.SkippedReason, raw)
				}
				if res.Forge != tc.wantForge {
					t.Errorf("%s: forge = %q, want %q", shell, res.Forge, tc.wantForge)
				}
				if want := tc.wantReply(f.srv.URL, pr); res.ReplyURL != want {
					t.Errorf("%s: reply_url = %q, want the forge's note %q", shell, res.ReplyURL, want)
				}
				hits := f.seen()
				w := writes(hits)
				if len(w) != 1 {
					t.Fatalf("%s: the forge saw %d writes, want exactly one: %+v", shell, len(w), hits)
				}
				if w[0].Method != http.MethodPost || w[0].Path != tc.wantPost {
					t.Errorf("%s: the write was %s %s, want POST %s", shell, w[0].Method, w[0].Path, tc.wantPost)
				}
				for _, h := range hits {
					if h.Auth != tc.wantAuth || h.PrivateToken != "" {
						t.Errorf("%s: %s %s authenticated with Authorization %q / PRIVATE-TOKEN %q, want Authorization %q", shell, h.Method, h.Path, h.Auth, h.PrivateToken, tc.wantAuth)
					}
				}
				var reads []string
				for _, h := range hits {
					if h.Method == http.MethodGet {
						reads = append(reads, h.Path)
					}
				}
				if !slices.Equal(reads, []string{tc.wantRead}) {
					t.Errorf("%s: the re-reads were %v, want one GET %s", shell, reads, tc.wantRead)
				}
				body := postedBody(t, w[0])
				if want := tc.wantBody + publishFooter; body != want {
					t.Errorf("%s: posted body =\n%q\nwant\n%q", shell, body, want)
				}
				if tc.wantForge == "gitlab" && forge.GitLabRunsQuickActionLine(body) {
					t.Errorf("%s: the GitLab body still holds a line GitLab runs as a quick action: %q", shell, body)
				}
				if cmd, _ := webhooks.ParseSlashCommand(body); cmd != "" {
					t.Errorf("%s: the posted body reads as the command /%s", shell, cmd)
				}
			}
		})
	}
}

// The bot's own reply must never read as a command: an answer that opens on
// `/revi` (after blank lines, quote lines, or whitespace the command grammar
// trims) is posted with that slash escaped, on every forge. The webhook's
// own grammar (webhooks.ParseSlashCommand) reads the posted body.
func TestReviConversePublishReplyNeverPostsACommand(t *testing.T) {
	shells := publishShells(t)
	answers := map[string]string{
		"leading command":        "/revi run it again please",
		"after blank lines":      "\n\n  \n/revi run it again",
		"after a quote":          "> /revi earlier\n\n/approve this",
		"indented":               "   /revi again",
		"after a no-break space": " /revi again",
		"a bare slash":           "/ and then prose",
	}
	for name, answer := range answers {
		for _, prPath := range []string{"/o/r/pull/1", "/g/p/-/merge_requests/1", "/o/r/pulls/1"} {
			t.Run(name+" "+prPath, func(t *testing.T) {
				for _, shell := range shells {
					f := newConverseFakeForge(t)
					pr := f.srv.URL + prPath
					f.htmlURL = pr + "#issuecomment-9001"
					cmd := renderReviConversePublish(t,
						map[string]any{"answer": answer, "skipped_reason": ""},
						map[string]any{"pr_url": pr, "discussion_id": ""},
						publishRunURL, writeToken(t))
					res, raw := runPublish(t, shell, cmd, noProxyEnv())
					if !*res.Posted {
						t.Fatalf("%s: nothing posted: %s", shell, raw)
					}
					w := writes(f.seen())
					if len(w) != 1 {
						t.Fatalf("%s: %d writes, want 1", shell, len(w))
					}
					body := postedBody(t, w[0])
					if cmd, args := webhooks.ParseSlashCommand(body); cmd != "" {
						t.Errorf("%s: the posted body %q reads as the command /%s %s — the bot's reply would re-fire a trigger", shell, body, cmd, args)
					}
					if !strings.HasSuffix(body, publishFooter) {
						t.Errorf("%s: the posted body lost its run footer: %q", shell, body)
					}
				}
			})
		}
	}
}

// Nothing to post, nothing written: an empty answer, the agent's own
// skipped_reason, no mounted token, a pr_url this step cannot reply on, a
// discussion id that is not one, an answer the edge never delivered. Each
// is posted=false with a reason converse_health shows, and the forge sees
// no request at all.
func TestReviConversePublishReplyPostsNothingWhenItCannot(t *testing.T) {
	shells := publishShells(t)
	type tc struct {
		name       string
		input      map[string]any
		prPath     string // appended to the fake's origin; a full URL when it has a scheme
		disc       string
		noToken    bool
		wantReason string
	}
	answer := map[string]any{"answer": publishAnswer, "skipped_reason": ""}
	for _, c := range []tc{
		{name: "empty answer", input: map[string]any{"answer": "", "skipped_reason": ""}, prPath: "/o/r/pull/1", wantReason: "empty answer"},
		{name: "blank answer", input: map[string]any{"answer": " \n\t", "skipped_reason": ""}, prPath: "/o/r/pull/1", wantReason: "empty answer"},
		{name: "the agent skipped", input: map[string]any{"answer": "", "skipped_reason": "no question in the note"}, prPath: "/o/r/pull/1", wantReason: "no question in the note"},
		{name: "the agent skipped beside a draft", input: map[string]any{"answer": publishAnswer, "skipped_reason": "the diff could not be read"}, prPath: "/o/r/pull/1", wantReason: "the diff could not be read"},
		{name: "no token mounted", input: answer, prPath: "/o/r/pull/1", noToken: true, wantReason: "forge_token is not mounted"},
		{name: "pr_url empty", input: answer, prPath: "", wantReason: "pr_url is empty"},
		{name: "pr_url an issue", input: answer, prPath: "/o/r/issues/1", wantReason: "not a pull request URL"},
		{name: "pr_url a dot segment", input: answer, prPath: "/o/../pull/1", wantReason: "not a pull request URL"},
		{name: "pr_url not http", input: answer, prPath: "ftp://forge.example/o/r/pull/1", wantReason: "not an http(s) URL"},
		{name: "pr_url with userinfo", input: answer, prPath: "https://user:pw@forge.example/o/r/pull/1", wantReason: "userinfo"},
		{name: "github discussion id not a comment id", input: answer, prPath: "/o/r/pull/1", disc: "../../x", wantReason: "not a GitHub review-comment id"},
		{name: "gitlab discussion id not one", input: answer, prPath: "/g/p/-/merge_requests/1", disc: "a/b", wantReason: "not a GitLab discussion id"},
		{name: "the answer never reached the node", input: map[string]any{"skipped_reason": ""}, prPath: "/o/r/pull/1", wantReason: "input is unmapped"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, shell := range shells {
				f := newConverseFakeForge(t)
				pr := c.prPath
				if pr != "" && !strings.Contains(pr, "://") {
					pr = f.srv.URL + pr
				}
				tokenPath := writeToken(t)
				if c.noToken {
					tokenPath = filepath.Join(t.TempDir(), "absent", "forge_token")
				}
				var cmd string
				if _, has := c.input["answer"]; has {
					cmd = renderReviConversePublish(t, c.input, map[string]any{"pr_url": pr, "discussion_id": c.disc}, publishRunURL, tokenPath)
				} else {
					cmd = renderPublishLeavingAnswer(t, c.input, map[string]any{"pr_url": pr, "discussion_id": c.disc}, tokenPath)
				}
				res, raw := runPublish(t, shell, cmd, noProxyEnv())
				if *res.Posted || res.ReplyURL != "" {
					t.Errorf("%s: posted = %v, reply_url %q — want nothing posted (%s)", shell, *res.Posted, res.ReplyURL, raw)
				}
				if !strings.Contains(res.SkippedReason, c.wantReason) {
					t.Errorf("%s: skipped_reason %q does not name %q", shell, res.SkippedReason, c.wantReason)
				}
				if hits := f.seen(); len(hits) != 0 {
					t.Errorf("%s: the forge saw %+v, want no request", shell, hits)
				}
			}
		})
	}
}

// renderPublishLeavingAnswer renders the command with `answer` absent from
// the input, as an unmapped edge leaves it: the reference stays as written.
func renderPublishLeavingAnswer(t *testing.T, input, vars map[string]any, tokenPath string) string {
	t.Helper()
	wf := compileBotFile(t, filepath.Join(botsDir(t), "revi-converse", "main.bot"))
	tn := wf.Nodes["publish_reply"].(*ir.ToolNode)
	shapes := model.WorkflowShapes(wf).WithInputSchema(wf.Schemas[tn.InputSchema])
	td := &model.TemplateData{RunID: publishRunID, Run: map[string]any{"id": publishRunID, "url": publishRunURL}}
	var unresolved []string
	command := model.RenderCommand(tn.Command, tn.CommandRefs, input, vars, td, publishRunID, shapes, func(ref string) {
		unresolved = append(unresolved, ref)
	})
	if !slices.Equal(unresolved, []string{"input.answer"}) {
		t.Fatalf("unresolved = %v, want only input.answer", unresolved)
	}
	ph := "'" + secretguard.PlaceholderForName("forge_token") + "'"
	if strings.Count(command, ph) != 1 {
		t.Fatalf("the forge_token reference renders %d times", strings.Count(command, ph))
	}
	return strings.Replace(command, ph, shellSingleQuote(tokenPath), 1)
}

// The forge's refusal is the reason: a 4xx or 5xx on the POST, or a
// redirect (never followed — the token would ride it to the host it names),
// leaves posted=false with the status and the forge's message, after the
// one write, with no re-read. The token never reaches the reason.
func TestReviConversePublishReplyReportsTheForgesRefusal(t *testing.T) {
	shells := publishShells(t)
	for _, c := range []struct {
		name       string
		status     int
		answer     string
		redirect   bool
		wantReason []string
	}{
		{name: "403", status: http.StatusForbidden, answer: `{"message":"Resource not accessible by integration"}`, wantReason: []string{"HTTP 403", "Resource not accessible by integration"}},
		{name: "404", status: http.StatusNotFound, answer: `{"message":"404 Discussion Not Found"}`, wantReason: []string{"HTTP 404", "404 Discussion Not Found"}},
		{name: "502", status: http.StatusBadGateway, answer: "<html>bad gateway</html>", wantReason: []string{"HTTP 502", "bad gateway"}},
		{name: "a message echoing the token", status: http.StatusUnauthorized, answer: `{"message":"token ` + publishToken + ` is revoked"}`, wantReason: []string{"HTTP 401", "[redacted]"}},
		{name: "a redirect", redirect: true, wantReason: []string{"HTTP 302"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, shell := range shells {
				f := newConverseFakeForge(t)
				other := newConverseFakeForge(t)
				f.postStatus, f.postAnswer = c.status, c.answer
				if c.redirect {
					f.redirectTo = other.srv.URL + "/stolen"
				}
				pr := f.srv.URL + "/g/p/-/merge_requests/7"
				cmd := renderReviConversePublish(t,
					map[string]any{"answer": publishAnswer, "skipped_reason": ""},
					map[string]any{"pr_url": pr, "discussion_id": "6a1f0c"},
					publishRunURL, writeToken(t))
				res, raw := runPublish(t, shell, cmd, noProxyEnv())
				if *res.Posted || res.ReplyURL != "" {
					t.Errorf("%s: posted = %v, reply_url %q, want nothing posted (%s)", shell, *res.Posted, res.ReplyURL, raw)
				}
				for _, want := range c.wantReason {
					if !strings.Contains(res.SkippedReason, want) {
						t.Errorf("%s: skipped_reason %q does not name %q", shell, res.SkippedReason, want)
					}
				}
				hits := f.seen()
				if len(hits) != 1 || hits[0].Method != http.MethodPost {
					t.Errorf("%s: the forge saw %+v, want the one POST and no re-read", shell, hits)
				}
				if got := other.seen(); len(got) != 0 {
					t.Errorf("%s: the redirect target saw %+v — the token followed a redirect", shell, got)
				}
			}
		})
	}
}

// A post the forge accepted but the re-read cannot find is not an answer
// the operator can see: posted=false, the reason naming the failed GET; the
// same for a re-read that answers with another note, and for a created note
// that came back with no id to read it by. Still exactly one write.
func TestReviConversePublishReplyNeedsTheReReadToFindTheNote(t *testing.T) {
	shells := publishShells(t)
	for _, c := range []struct {
		name       string
		noID       bool
		other      bool
		wantReason string
		wantReads  int
	}{
		{name: "the re-read misses", wantReason: "re-reading it failed: GET", wantReads: 2},
		{name: "the re-read finds another note", other: true, wantReason: "re-reading it failed: GET", wantReads: 2},
		{name: "no note id", noID: true, wantReason: "with no note id", wantReads: 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, shell := range shells {
				f := newConverseFakeForge(t)
				f.postNoID, f.readOther = c.noID, c.other
				if !c.noID && !c.other {
					f.readStatus = http.StatusNotFound
				}
				pr := f.srv.URL + "/o/r/pull/42"
				f.htmlURL = pr + "#issuecomment-9001"
				cmd := renderReviConversePublish(t,
					map[string]any{"answer": publishAnswer, "skipped_reason": ""},
					map[string]any{"pr_url": pr, "discussion_id": ""},
					publishRunURL, writeToken(t))
				res, raw := runPublish(t, shell, cmd, noProxyEnv())
				if *res.Posted || res.ReplyURL != "" {
					t.Errorf("%s: posted = %v, reply_url %q — a note the re-read did not find is not posted (%s)", shell, *res.Posted, res.ReplyURL, raw)
				}
				if !strings.Contains(res.SkippedReason, c.wantReason) {
					t.Errorf("%s: skipped_reason %q does not name %q", shell, res.SkippedReason, c.wantReason)
				}
				hits := f.seen()
				if w := writes(hits); len(w) != 1 {
					t.Errorf("%s: %d writes, want exactly one", shell, len(w))
				}
				if reads := len(hits) - len(writes(hits)); reads != c.wantReads {
					t.Errorf("%s: %d re-reads, want %d", shell, reads, c.wantReads)
				}
			}
		})
	}
}

func utf16Units(s string) int { return len(utf16.Encode([]rune(s))) }

// A body past GitHub's 65536-character comment limit would be refused whole.
// The node cuts the ANSWER, says so in the body, closes a code fence the cut
// left open, and keeps the run footer: the link is the one part never cut.
// An answer that leaves a fence open untruncated is closed too, so the
// footer renders as a link and not as code.
func TestReviConversePublishReplyTruncationKeepsTheRunLink(t *testing.T) {
	shells := publishShells(t)
	fence := "```"
	for _, c := range []struct {
		name          string
		answer        string
		wantTruncated bool
	}{
		{name: "ascii prose", answer: "Re: op — « long »\n\n" + strings.Repeat("word ", 20000), wantTruncated: true},
		{name: "astral characters", answer: "Re: op\n\n" + strings.Repeat("😀", 40000), wantTruncated: true},
		{name: "cut inside a fence", answer: "Re: op\n\n" + fence + "go\n" + strings.Repeat("x := 1\n", 12000), wantTruncated: true},
		{name: "an unclosed fence, short", answer: "Re: op\n\n" + fence + "go\nx := 1", wantTruncated: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, shell := range shells {
				f := newConverseFakeForge(t)
				pr := f.srv.URL + "/o/r/pull/42"
				f.htmlURL = pr + "#issuecomment-9001"
				cmd := renderReviConversePublish(t,
					map[string]any{"answer": c.answer, "skipped_reason": ""},
					map[string]any{"pr_url": pr, "discussion_id": ""},
					publishRunURL, writeToken(t))
				res, raw := runPublish(t, shell, cmd, noProxyEnv())
				if !*res.Posted {
					t.Fatalf("%s: nothing posted: %s", shell, raw)
				}
				w := writes(f.seen())
				if len(w) != 1 {
					t.Fatalf("%s: %d writes, want 1", shell, len(w))
				}
				body := postedBody(t, w[0])
				if !strings.HasSuffix(body, publishFooter) {
					t.Errorf("%s: the posted body does not end with the run footer: …%q", shell, body[max(0, len(body)-200):])
				}
				if n := utf16Units(body); n > 65536 {
					t.Errorf("%s: the posted body is %d UTF-16 units, past GitHub's 65536", shell, n)
				}
				marked := strings.Contains(body, "answer truncated to fit the forge comment limit")
				if marked != c.wantTruncated || strings.Contains(res.Summary, "truncated") != c.wantTruncated {
					t.Errorf("%s: truncation marker in body %v, in summary %v, want %v", shell, marked, strings.Contains(res.Summary, "truncated"), c.wantTruncated)
				}
				opens := 0
				for _, line := range strings.Split(strings.TrimSuffix(body, publishFooter), "\n") {
					if strings.HasPrefix(line, fence) {
						opens++
					}
				}
				if opens%2 != 0 {
					t.Errorf("%s: the body leaves a code fence open before the footer (%d fence lines)", shell, opens)
				}
			}
		})
	}
}

// The footer follows docs/agents/bots/run-link.md whatever the run gives
// it: no address prints the id alone, never a host; a run id that is not
// plain is never pasted into a link label.
func TestReviConversePublishReplyFooterFollowsTheRunLinkRules(t *testing.T) {
	shells := publishShells(t)
	for _, c := range []struct {
		name, runURL, wantTail string
	}{
		{name: "a run url", runURL: publishRunURL, wantTail: publishFooter},
		{name: "no public address", runURL: "", wantTail: "\n\n---\n🤖 [iterion](https://iterion.cloud) · run `run-converse`"},
		{name: "an engine predating run.url", runURL: "{{run.url}}", wantTail: "\n\n---\n🤖 [iterion](https://iterion.cloud) · run `run-converse`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, shell := range shells {
				f := newConverseFakeForge(t)
				pr := f.srv.URL + "/o/r/pull/42"
				f.htmlURL = pr + "#issuecomment-9001"
				cmd := renderReviConversePublish(t,
					map[string]any{"answer": publishAnswer, "skipped_reason": ""},
					map[string]any{"pr_url": pr, "discussion_id": ""},
					c.runURL, writeToken(t))
				res, raw := runPublish(t, shell, cmd, noProxyEnv())
				if !*res.Posted {
					t.Fatalf("%s: nothing posted: %s", shell, raw)
				}
				w := writes(f.seen())
				if len(w) != 1 {
					t.Fatalf("%s: %d writes, want 1", shell, len(w))
				}
				body := postedBody(t, w[0])
				if !strings.HasSuffix(body, c.wantTail) {
					t.Errorf("%s: the body ends %q, want the footer %q", shell, body[max(0, len(body)-len(c.wantTail)-40):], c.wantTail)
				}
			}
		})
	}
}

// connectLogger is an HTTPS proxy that records each CONNECT target and
// refuses it: the address a request was headed to, with no network.
func connectLogger(t *testing.T) (string, func() []string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var mu sync.Mutex
	var seen []string
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				line, _ := bufio.NewReader(c).ReadString('\n')
				mu.Lock()
				seen = append(seen, strings.TrimSpace(line))
				mu.Unlock()
				_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			}(c)
		}
	}()
	return "http://" + l.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// The production derivation, on real host names and no network: github.com
// posts to api.github.com, a GitHub Enterprise host to its own /api/v3, a
// GitLab host to /api/v4, a Forgejo host to /api/v1. An HTTPS proxy that
// logs the CONNECT and refuses it shows where the request was going; the
// reason names the URL it was going to.
func TestReviConversePublishReplyDerivesTheProductionAPIBase(t *testing.T) {
	shells := publishShells(t)
	for _, c := range []struct {
		pr, disc, wantConnect, wantURL string
	}{
		{pr: "https://github.com/o/r/pull/42", wantConnect: "CONNECT api.github.com:443", wantURL: "https://api.github.com/repos/o/r/issues/42/comments"},
		{pr: "https://github.com/o/r/pull/42", disc: "77", wantConnect: "CONNECT api.github.com:443", wantURL: "https://api.github.com/repos/o/r/pulls/42/comments/77/replies"},
		{pr: "https://ghe.example/o/r/pull/42", wantConnect: "CONNECT ghe.example:443", wantURL: "https://ghe.example/api/v3/repos/o/r/issues/42/comments"},
		{pr: "https://gitlab.example:8443/g/p/-/merge_requests/7", disc: "abc123", wantConnect: "CONNECT gitlab.example:8443", wantURL: "https://gitlab.example:8443/api/v4/projects/g%2Fp/merge_requests/7/discussions/abc123/notes"},
		{pr: "https://codeberg.example/o/r/pulls/7", wantConnect: "CONNECT codeberg.example:443", wantURL: "https://codeberg.example/api/v1/repos/o/r/issues/7/comments"},
	} {
		t.Run(c.wantURL, func(t *testing.T) {
			for _, shell := range shells {
				proxy, seen := connectLogger(t)
				cmd := renderReviConversePublish(t,
					map[string]any{"answer": publishAnswer, "skipped_reason": ""},
					map[string]any{"pr_url": c.pr, "discussion_id": c.disc},
					publishRunURL, writeToken(t))
				res, raw := runPublish(t, shell, cmd, noProxyEnv("HTTPS_PROXY="+proxy, "https_proxy="+proxy))
				if *res.Posted {
					t.Fatalf("%s: posted through a proxy that refuses every tunnel: %s", shell, raw)
				}
				if got := seen(); !slices.Contains(got, c.wantConnect+" HTTP/1.1") && !slices.Contains(got, c.wantConnect+" HTTP/1.0") {
					t.Errorf("%s: the proxy saw %v, want %s", shell, got, c.wantConnect)
				}
				if !strings.Contains(res.SkippedReason, c.wantURL) {
					t.Errorf("%s: skipped_reason %q does not name %s", shell, res.SkippedReason, c.wantURL)
				}
			}
		})
	}
}

// converseChainExec answers converse_agent with a draft shaped by the
// converse_output schema and runs every other node — publish_reply and
// converse_health — on a real ClawExecutor, under the run's secret guard.
type converseChainExec struct {
	real  *model.ClawExecutor
	agent map[string]any

	mu   sync.Mutex
	outs map[string]map[string]any
}

func (c *converseChainExec) SetVars(v map[string]any) { c.real.SetVars(v) }

func (c *converseChainExec) Execute(ctx context.Context, n ir.Node, input map[string]any) (map[string]any, error) {
	var out map[string]any
	var err error
	if n.NodeID() == "converse_agent" {
		out = map[string]any{}
		for k, v := range c.agent {
			out[k] = v
		}
	} else {
		out, err = c.real.Execute(ctx, n, input)
	}
	c.mu.Lock()
	if c.outs == nil {
		c.outs = map[string]map[string]any{}
	}
	c.outs[n.NodeID()] = out
	c.mu.Unlock()
	return out, err
}

func (c *converseChainExec) output(id string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outs[id]
}

// The chain as the engine runs it: the agent's draft, in the shape of its
// real schema, crosses the edge into publish_reply, which posts it with the
// token the run's guard mounts and the run's own link; its result crosses
// the edge into converse_health, which reads OK on a posted reply and
// degraded on every way it can fall short.
func TestReviConverseChainPostsTheDraftAndGatesIt(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("no_proxy", "127.0.0.1,localhost")
	t.Setenv(deeplink.PublicBaseEnv, "https://iterion.example")

	wf := compileBotFile(t, filepath.Join(botsDir(t), "revi-converse", "main.bot"))
	var fields []string
	for _, f := range wf.Schemas["converse_output"].Fields {
		fields = append(fields, f.Name)
	}
	slices.Sort(fields)
	if want := []string{"answer", "forge", "skipped_reason", "summary"}; !slices.Equal(fields, want) {
		t.Fatalf("converse_output fields = %v, want %v — the agent drafts; posting is publish_reply's", fields, want)
	}
	draft := func(answer, skipped string) map[string]any {
		return map[string]any{"answer": answer, "forge": "gitlab", "skipped_reason": skipped, "summary": "Question: why does A return 2?"}
	}

	for _, c := range []struct {
		name         string
		agent        map[string]any
		noToken      bool
		refuse       int
		missRead     bool
		wantWrites   int
		wantDegraded bool
		wantBanner   string
	}{
		{name: "posted", agent: draft(publishAnswer, ""), wantWrites: 1, wantBanner: "forge reply OK: "},
		{name: "the agent skipped", agent: draft("", "no question in the note"), wantDegraded: true, wantBanner: "forge reply skipped: no question in the note"},
		{name: "an empty draft", agent: draft("", ""), wantDegraded: true, wantBanner: "forge reply skipped: the agent returned an empty answer"},
		{name: "no token", agent: draft(publishAnswer, ""), noToken: true, wantDegraded: true, wantBanner: "forge reply skipped: forge_token is not mounted"},
		{name: "the forge refuses", agent: draft(publishAnswer, ""), refuse: http.StatusForbidden, wantWrites: 1, wantDegraded: true, wantBanner: "HTTP 403"},
		{name: "the re-read misses", agent: draft(publishAnswer, ""), missRead: true, wantWrites: 1, wantDegraded: true, wantBanner: "re-reading it failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newConverseFakeForge(t)
			f.postStatus, f.postAnswer = c.refuse, `{"message":"403 Forbidden"}`
			if c.missRead {
				f.readStatus = http.StatusNotFound
			}
			pr := f.srv.URL + "/g/p/-/merge_requests/7"
			var secrets []secretguard.Secret
			if !c.noToken {
				secrets = append(secrets, secretguard.Secret{Name: "forge_token", Value: publishToken, FilePath: writeToken(t)})
			}
			guard := secretguard.New(secrets, secretguard.DefaultConfig())
			ex := &converseChainExec{
				real:  model.NewClawExecutor(model.NewRegistry(), wf, model.WithWorkDir(t.TempDir()), model.WithSecretGuard(guard)),
				agent: c.agent,
			}
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			const runID = "chain-run-1"
			vars := map[string]any{
				"workspace_dir": t.TempDir(), "base_ref": "main", "pr_url": pr, "discussion_id": "6a1f0c",
				"trigger_note": "/revi why does A return 2?", "converse_question": "why does A return 2?",
				"thread_context": "", "replier": "op",
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := runtime.New(wf, st, ex).Run(ctx, runID, vars); err != nil {
				t.Fatalf("the run failed: %v — publish_reply and converse_health inform, they never fail it", err)
			}
			health := ex.output("converse_health")
			if health == nil {
				t.Fatal("converse_health never ran")
			}
			if health["degraded"] != c.wantDegraded {
				t.Errorf("degraded = %v, want %v (banner %q, publish_reply %v)", health["degraded"], c.wantDegraded, health["banner"], ex.output("publish_reply"))
			}
			banner, _ := health["banner"].(string)
			if !strings.Contains(banner, c.wantBanner) {
				t.Errorf("banner %q does not name %q", banner, c.wantBanner)
			}
			hits := f.seen()
			w := writes(hits)
			if len(w) != c.wantWrites {
				t.Fatalf("the forge saw %d writes, want %d: %+v", len(w), c.wantWrites, hits)
			}
			if c.wantWrites == 0 {
				return
			}
			if w[0].Auth != "Bearer "+publishToken {
				t.Errorf("the POST authenticated with %q — the guard's mounted token did not reach it", w[0].Auth)
			}
			wantFooter := "\n\n---\n🤖 [iterion](https://iterion.cloud) · run [`" + runID + "`](" + deeplink.Run("https://iterion.example", runID) + ")"
			if body, want := postedBody(t, w[0]), forge.InertGitLabQuickActions(publishAnswer)+wantFooter; body != want {
				t.Errorf("the posted body is not the agent's draft, guarded, then this run's link:\n%q\nwant\n%q", body, want)
			}
			if !c.wantDegraded {
				if want := "forge reply OK: " + pr + "#note_9001"; banner != want {
					t.Errorf("banner = %q, want %q", banner, want)
				}
			}
		})
	}
}
