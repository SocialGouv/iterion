package bots

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// publish_reply is revi-converse's only forge write: a deterministic python
// script that posts the agent's draft in the conversation's thread with the
// mounted forge_token and re-reads the created note by its id. These tests
// run its script as the engine renders it, against a fake forge serving the
// REST shape each forge documents; the fake records every request, so
// "exactly one write" is the forge's own count, not the node's claim.

const (
	publishRunID  = "run-converse"
	publishRunURL = "https://iterion.example/studio/runs/run-converse"
	publishToken  = "forge-token-publish-7c1e"
	publishNoteID = 9001
	// The footer every post ends with (docs/agents/bots/run-link.md).
	publishFooter = "\n\n---\n🤖 [iterion](https://iterion.cloud) · run [`run-converse`](https://iterion.example/studio/runs/run-converse)"
	// The forge_token placeholder, and the form the engine rewrites it to
	// where the secret is not granted (secretguard.DefuseUngranted).
	tokenPlaceholder = "__ITERION_SECRET_forge_token__"
	tokenDefused     = "__ITERION_SECRET_forge_token_redacted__"
)

// The answer's one model call is the agent's: posting it and judging the
// post are deterministic tool nodes, so publishing costs no LLM and cannot
// run out of tool steps. The workflow's nodes are exactly the drafting
// agent and the two tools, chained in that order.
func TestReviConversePublishesWithoutAModel(t *testing.T) {
	wf := reviConverseWorkflow(t)
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

func reviConverseWorkflow(t *testing.T) *ir.Workflow {
	t.Helper()
	return compileBotFile(t, filepath.Join(botsDir(t), "revi-converse", "main.bot"))
}

// renderReviConversePublish renders publish_reply's script with the engine's
// renderer: the edge's input, the run's vars (the ones a case leaves out at
// their declared default) and the run namespace. A preview rendering has no
// secret guard, so `{{secrets.forge_token.path}}` comes out as the secret's
// placeholder; the run's guard renders the mounted path there, and so does
// this helper (the chain tests render through a real guard). Every reference
// must resolve but the ones a case names as left unresolved.
func renderReviConversePublish(t *testing.T, input, vars map[string]any, runURL, tokenPath string, wantUnresolved ...string) string {
	t.Helper()
	wf := reviConverseWorkflow(t)
	tn, ok := wf.Nodes["publish_reply"].(*ir.ToolNode)
	if !ok || tn.Script == "" || tn.Language != "py" {
		t.Fatalf("publish_reply is %T (language %q), want a python script tool node", wf.Nodes["publish_reply"], tn.Language)
	}
	all := map[string]any{"pr_url": "", "discussion_id": "", "forge_base_url": ""}
	for k, v := range vars {
		all[k] = v
	}
	td := &model.TemplateData{RunID: publishRunID, Run: map[string]any{"id": publishRunID, "url": runURL}}
	var unresolved []string
	script := model.RenderScript(tn.Script, tn.ScriptRefs, input, all, td, publishRunID, func(ref string) {
		unresolved = append(unresolved, ref)
	})
	slices.Sort(unresolved)
	if !slices.Equal(unresolved, wantUnresolved) {
		t.Fatalf("publish_reply left %v unresolved, want %v — the case does not drive the input it names", unresolved, wantUnresolved)
	}
	line := `tok_path = "` + secretguard.PlaceholderForName("forge_token") + `"`
	if strings.Count(script, line) != 1 {
		t.Fatalf("publish_reply renders the forge_token path line %d times, want once", strings.Count(script, line))
	}
	path, _ := json.Marshal(tokenPath)
	return strings.Replace(script, line, "tok_path = "+string(path), 1)
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

// tokenForms are the spellings of the token the node must neither post nor
// report: raw, base64 (standard and URL-safe, padded or not), hex in either
// case.
func tokenForms(tok string) []string {
	raw := []byte(tok)
	forms := []string{tok, hex.EncodeToString(raw), strings.ToUpper(hex.EncodeToString(raw))}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		e := enc.EncodeToString(raw)
		forms = append(forms, e, strings.TrimRight(e, "="))
	}
	return forms
}

// runPublish runs a rendered script the way the executor runs a `language:
// py` body — `python3 -I` on a temp file — and decodes its output.
func runPublish(t *testing.T, script string, env []string) (publishOut, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "publish_reply.py")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-I", file)
	cmd.Dir = dir
	cmd.Env = env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("publish_reply failed: %v (out %q, stderr %q)", err, out, stderr.String())
	}
	var res publishOut
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("output is not publish_reply_output JSON: %v (%q, stderr %q)", err, out, stderr.String())
	}
	if res.Posted == nil {
		t.Fatalf("output carries no posted field: %q", out)
	}
	for _, form := range tokenForms(publishToken) {
		if strings.Contains(string(out), form) {
			t.Fatalf("the forge token (%s form) reached the node's output: %q", form, out)
		}
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

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
}

// forgeHit is one request the fake forge received.
type forgeHit struct {
	Method, Path, Query, Auth, PrivateToken, Body string
}

type fakeNote struct {
	ID   int
	Body string
}

// converseFakeForge holds a thread: a POST appends a note, a GET whose last
// path segment is a number reads that note, any other GET lists the thread
// (a GitLab discussion as {"id", "notes"}, everything else as an array). The
// tests assert the paths. Its knobs refuse the POST, drop the created note's
// id, refuse the re-read or answer it with another note, refuse the list, or
// redirect the POST.
type converseFakeForge struct {
	srv *httptest.Server

	htmlFor    func(id int) string // the note's html_url; nil: notes carry none (GitLab)
	postStatus int
	postAnswer string
	postNoID   bool
	readStatus int
	readOther  bool
	listStatus int
	redirectTo string

	mu     sync.Mutex
	hits   []forgeHit
	notes  []fakeNote
	nextID int
}

func newConverseFakeForge(t *testing.T) *converseFakeForge {
	t.Helper()
	f := &converseFakeForge{nextID: publishNoteID}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// hold seeds the thread with a note an earlier execution left there.
func (f *converseFakeForge) hold(id int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, fakeNote{ID: id, Body: body})
}

func (f *converseFakeForge) noteJSON(n fakeNote) map[string]any {
	out := map[string]any{"id": n.ID, "body": n.Body}
	if f.htmlFor != nil {
		out["html_url"] = f.htmlFor(n.ID)
	}
	return out
}

func (f *converseFakeForge) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.EscapedPath()
	f.hits = append(f.hits, forgeHit{
		Method: r.Method, Path: path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
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
		n := fakeNote{ID: f.nextID, Body: p.Body}
		f.nextID++
		f.notes = append(f.notes, n)
		out := f.noteJSON(n)
		if f.postNoID {
			delete(out, "id")
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	case http.MethodGet:
		last := path[strings.LastIndex(path, "/")+1:]
		if id, err := strconv.Atoi(last); err == nil {
			if f.readStatus != 0 {
				w.WriteHeader(f.readStatus)
				_, _ = io.WriteString(w, `{"message":"404 Not found"}`)
				return
			}
			for _, n := range f.notes {
				if n.ID == id {
					out := f.noteJSON(n)
					if f.readOther {
						out["id"] = id - 1
					}
					_ = json.NewEncoder(w).Encode(out)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"404 Not found"}`)
			return
		}
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			_, _ = io.WriteString(w, `{"message":"list refused"}`)
			return
		}
		list := []map[string]any{}
		for _, n := range f.notes {
			list = append(list, f.noteJSON(n))
		}
		if strings.Contains(path, "/discussions/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": last, "notes": list})
			return
		}
		_ = json.NewEncoder(w).Encode(list)
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

// gets are the GET paths, in order.
func gets(hits []forgeHit) []string {
	var g []string
	for _, h := range hits {
		if h.Method == http.MethodGet {
			g = append(g, h.Path)
		}
	}
	return g
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
	base      string // forge_base_url, appended to the fake's origin when set
	wantForge string
	wantList  string // the duplicate check's read of the thread, escaped
	wantPost  string // the reply POST, escaped
	wantRead  string // the re-read by id, escaped
	wantAuth  string // the Authorization header
	// htmlURL names a note's web URL from the PR URL; nil on GitLab, whose
	// notes carry none (reply_url is pr_url#note_<id> there).
	htmlURL func(pr string, id int) string
	// wantBody is the body before the footer.
	wantBody string
}

func (c publishForgeCase) wantReply(pr string, id int) string {
	if c.htmlURL == nil {
		return fmt.Sprintf("%s#note_%d", pr, id)
	}
	return c.htmlURL(pr, id)
}

func issueComment(pr string, id int) string { return fmt.Sprintf("%s#issuecomment-%d", pr, id) }
func reviewComment(pr string, id int) string {
	return fmt.Sprintf("%s#discussion_r%d", pr, id)
}

func publishForgeCases() []publishForgeCase {
	gitlabBody := forge.InertGitLabQuickActions(publishAnswer)
	return []publishForgeCase{
		{
			name: "github issue comment (the /revi <question> lane)", prPath: "/o/r/pull/42", wantForge: "github",
			wantList: "/api/v3/repos/o/r/issues/42/comments",
			wantPost: "/api/v3/repos/o/r/issues/42/comments", wantRead: "/api/v3/repos/o/r/issues/comments/9001",
			wantAuth: "Bearer " + publishToken, htmlURL: issueComment, wantBody: publishAnswer,
		},
		{
			name: "github review-thread reply", prPath: "/o/r/pull/42", disc: "4242", wantForge: "github",
			wantList: "/api/v3/repos/o/r/pulls/42/comments",
			wantPost: "/api/v3/repos/o/r/pulls/42/comments/4242/replies", wantRead: "/api/v3/repos/o/r/pulls/comments/9001",
			wantAuth: "Bearer " + publishToken, htmlURL: reviewComment, wantBody: publishAnswer,
		},
		{
			name: "gitlab discussion note", prPath: "/g/sub/p/-/merge_requests/7", disc: "6a1f0c", wantForge: "gitlab",
			wantList: "/api/v4/projects/g%2Fsub%2Fp/merge_requests/7/discussions/6a1f0c",
			wantPost: "/api/v4/projects/g%2Fsub%2Fp/merge_requests/7/discussions/6a1f0c/notes", wantRead: "/api/v4/projects/g%2Fsub%2Fp/merge_requests/7/notes/9001",
			wantAuth: "Bearer " + publishToken, wantBody: gitlabBody,
		},
		{
			name: "gitlab merge request note (no discussion id)", prPath: "/g/p/-/merge_requests/7", wantForge: "gitlab",
			wantList: "/api/v4/projects/g%2Fp/merge_requests/7/notes",
			wantPost: "/api/v4/projects/g%2Fp/merge_requests/7/notes", wantRead: "/api/v4/projects/g%2Fp/merge_requests/7/notes/9001",
			wantAuth: "Bearer " + publishToken, wantBody: gitlabBody,
		},
		{
			name: "forgejo issue comment", prPath: "/o/r/pulls/7", wantForge: "forgejo",
			wantList: "/api/v1/repos/o/r/issues/7/comments",
			wantPost: "/api/v1/repos/o/r/issues/7/comments", wantRead: "/api/v1/repos/o/r/issues/comments/9001",
			wantAuth: "token " + publishToken, htmlURL: issueComment, wantBody: publishAnswer,
		},
		{
			name: "gitlab under a sub-path, the webhook's forge base", prPath: "/gitlab/g/p/-/merge_requests/5", disc: "abc123", base: "/gitlab", wantForge: "gitlab",
			wantList: "/gitlab/api/v4/projects/g%2Fp/merge_requests/5/discussions/abc123",
			wantPost: "/gitlab/api/v4/projects/g%2Fp/merge_requests/5/discussions/abc123/notes", wantRead: "/gitlab/api/v4/projects/g%2Fp/merge_requests/5/notes/9001",
			wantAuth: "Bearer " + publishToken, wantBody: gitlabBody,
		},
		{
			name: "forgejo under a sub-path, the webhook's forge base", prPath: "/forgejo/o/r/pulls/7", base: "/forgejo/", wantForge: "forgejo",
			wantList: "/forgejo/api/v1/repos/o/r/issues/7/comments",
			wantPost: "/forgejo/api/v1/repos/o/r/issues/7/comments", wantRead: "/forgejo/api/v1/repos/o/r/issues/comments/9001",
			wantAuth: "token " + publishToken, htmlURL: issueComment, wantBody: publishAnswer,
		},
		{
			name: "github enterprise, the webhook's forge base", prPath: "/o/r/pull/42", base: "/", wantForge: "github",
			wantList: "/api/v3/repos/o/r/issues/42/comments",
			wantPost: "/api/v3/repos/o/r/issues/42/comments", wantRead: "/api/v3/repos/o/r/issues/comments/9001",
			wantAuth: "Bearer " + publishToken, htmlURL: issueComment, wantBody: publishAnswer,
		},
	}
}

// publishVars are the vars a forge case runs with.
func publishVars(c publishForgeCase, origin, pr string) map[string]any {
	vars := map[string]any{"pr_url": pr, "discussion_id": c.disc}
	if c.base != "" {
		vars["forge_base_url"] = origin + c.base
	}
	return vars
}

func newCaseForge(t *testing.T, c publishForgeCase) (*converseFakeForge, string) {
	t.Helper()
	f := newConverseFakeForge(t)
	pr := f.srv.URL + c.prPath
	if c.htmlURL != nil {
		f.htmlFor = func(id int) string { return c.htmlURL(pr, id) }
	}
	return f, pr
}

// Each forge's lane: one bounded read of the thread (the duplicate check),
// exactly one write, at the reply endpoint the forge documents, with that
// forge's auth header, carrying the answer and the run footer; then the
// re-read of the created note by its id, and reply_url the note's web URL on
// the PR's host. Under the webhook's forge base the API is the base's, so a
// GitLab or a Forgejo served under a sub-path is reached where it lives. On
// GitLab the `/approve` the answer quotes reaches the forge inert (the
// server's own predicate reads the posted body); elsewhere the body is the
// answer byte for byte.
func TestReviConversePublishReplyPostsOnEachForge(t *testing.T) {
	requirePython(t)
	for _, tc := range publishForgeCases() {
		t.Run(tc.name, func(t *testing.T) {
			f, pr := newCaseForge(t, tc)
			script := renderReviConversePublish(t,
				map[string]any{"answer": publishAnswer, "skipped_reason": ""},
				publishVars(tc, f.srv.URL, pr), publishRunURL, writeToken(t))
			res, raw := runPublish(t, script, noProxyEnv())

			if !*res.Posted || res.SkippedReason != "" {
				t.Fatalf("posted = %v, skipped_reason %q, want a posted reply (%s)", *res.Posted, res.SkippedReason, raw)
			}
			if res.Forge != tc.wantForge {
				t.Errorf("forge = %q, want %q", res.Forge, tc.wantForge)
			}
			if want := tc.wantReply(pr, publishNoteID); res.ReplyURL != want {
				t.Errorf("reply_url = %q, want the forge's note %q", res.ReplyURL, want)
			}
			hits := f.seen()
			w := writes(hits)
			if len(w) != 1 {
				t.Fatalf("the forge saw %d writes, want exactly one: %+v", len(w), hits)
			}
			if w[0].Method != http.MethodPost || w[0].Path != tc.wantPost {
				t.Errorf("the write was %s %s, want POST %s", w[0].Method, w[0].Path, tc.wantPost)
			}
			for _, h := range hits {
				if h.Auth != tc.wantAuth || h.PrivateToken != "" {
					t.Errorf("%s %s authenticated with Authorization %q / PRIVATE-TOKEN %q, want Authorization %q", h.Method, h.Path, h.Auth, h.PrivateToken, tc.wantAuth)
				}
			}
			if got, want := gets(hits), []string{tc.wantList, tc.wantRead}; !slices.Equal(got, want) {
				t.Errorf("the reads were %v, want the thread %s then the note %s", got, tc.wantList, tc.wantRead)
			}
			body := postedBody(t, w[0])
			if want := tc.wantBody + publishFooter; body != want {
				t.Errorf("posted body =\n%q\nwant\n%q", body, want)
			}
			if tc.wantForge == "gitlab" && forge.GitLabRunsQuickActionLine(body) {
				t.Errorf("the GitLab body still holds a line GitLab runs as a quick action: %q", body)
			}
			if cmd, _ := webhooks.ParseSlashCommand(body); cmd != "" {
				t.Errorf("the posted body reads as the command /%s", cmd)
			}
		})
	}
}

// publish_reply is safe to execute twice: a resume that re-runs the node
// after its POST landed finds the note this run already wrote — an exact
// copy of the body it would post, whose footer names the run — and reports
// it as posted, with that note's URL, writing nothing. A note that only
// borrows this run's footer is not this run's reply, and a thread the check
// cannot read does not stop the post.
func TestReviConversePublishReplyDoesNotPostTwice(t *testing.T) {
	requirePython(t)
	const heldID = 7001
	for _, tc := range publishForgeCases()[:5] {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range []struct {
				name       string
				held       string
				listStatus int
				wantWrites int
				wantReply  func(pr string) string
				wantNote   string
			}{
				{name: "already answered", held: tc.wantBody + publishFooter, wantWrites: 0, wantReply: func(pr string) string { return tc.wantReply(pr, heldID) }},
				{name: "a stranger's note under this run's footer", held: "LGTM, nothing to see here." + publishFooter, wantWrites: 1, wantReply: func(pr string) string { return tc.wantReply(pr, publishNoteID) }},
				{name: "the thread cannot be read", listStatus: http.StatusBadGateway, wantWrites: 1, wantReply: func(pr string) string { return tc.wantReply(pr, publishNoteID) }, wantNote: "duplicate check could not read the thread"},
			} {
				t.Run(c.name, func(t *testing.T) {
					f, pr := newCaseForge(t, tc)
					f.listStatus = c.listStatus
					if c.held != "" {
						f.hold(heldID, c.held)
					}
					script := renderReviConversePublish(t,
						map[string]any{"answer": publishAnswer, "skipped_reason": ""},
						publishVars(tc, f.srv.URL, pr), publishRunURL, writeToken(t))
					res, raw := runPublish(t, script, noProxyEnv())
					if !*res.Posted {
						t.Fatalf("posted = false (%s)", raw)
					}
					if w := writes(f.seen()); len(w) != c.wantWrites {
						t.Errorf("the forge saw %d writes, want %d: %+v", len(w), c.wantWrites, w)
					}
					if want := c.wantReply(pr); res.ReplyURL != want {
						t.Errorf("reply_url = %q, want %q", res.ReplyURL, want)
					}
					if c.wantNote != "" && !strings.Contains(res.Summary, c.wantNote) {
						t.Errorf("summary %q does not say %q", res.Summary, c.wantNote)
					}
				})
			}
		})
	}
}

// The bot's own reply must never read as a command: an answer that opens on
// `/revi` (after blank lines, quote lines, or whitespace the command grammar
// trims) is posted with that slash escaped, on every forge. The webhook's
// own grammar (webhooks.ParseSlashCommand) reads the posted body.
func TestReviConversePublishReplyNeverPostsACommand(t *testing.T) {
	requirePython(t)
	answers := map[string]string{
		"leading command":        "/revi run it again please",
		"after blank lines":      "\n\n  \n/revi run it again",
		"after a quote":          "> /revi earlier\n\n/approve this",
		"indented":               "   /revi again",
		"after a no-break space": " /revi again",
		"a bare slash":           "/ and then prose",
	}
	for name, answer := range answers {
		for _, prPath := range []string{"/o/r/pull/1", "/g/p/-/merge_requests/1", "/o/r/pulls/1"} {
			t.Run(name+" "+prPath, func(t *testing.T) {
				f := newConverseFakeForge(t)
				pr := f.srv.URL + prPath
				f.htmlFor = func(id int) string { return issueComment(pr, id) }
				script := renderReviConversePublish(t,
					map[string]any{"answer": answer, "skipped_reason": ""},
					map[string]any{"pr_url": pr}, publishRunURL, writeToken(t))
				res, raw := runPublish(t, script, noProxyEnv())
				if !*res.Posted {
					t.Fatalf("nothing posted: %s", raw)
				}
				w := writes(f.seen())
				if len(w) != 1 {
					t.Fatalf("%d writes, want 1", len(w))
				}
				body := postedBody(t, w[0])
				if cmd, args := webhooks.ParseSlashCommand(body); cmd != "" {
					t.Errorf("the posted body %q reads as the command /%s %s — the bot's reply would re-fire a trigger", body, cmd, args)
				}
				if !strings.HasSuffix(body, publishFooter) {
					t.Errorf("the posted body lost its run footer: %q", body)
				}
			})
		}
	}
}

// A drafted answer is posted whatever the skipped_reason beside it says: the
// schema requires the field, and a model at its step limit fills it ("N/A",
// "none", a half-reason). Only an empty draft is not posted.
func TestReviConversePublishReplyPostsADraftWhateverItsSkippedReasonSays(t *testing.T) {
	requirePython(t)
	for _, skipped := range []string{"N/A", "none", "not skipped", "the diff could not be read in full"} {
		t.Run(skipped, func(t *testing.T) {
			f := newConverseFakeForge(t)
			pr := f.srv.URL + "/o/r/pull/42"
			f.htmlFor = func(id int) string { return issueComment(pr, id) }
			script := renderReviConversePublish(t,
				map[string]any{"answer": publishAnswer, "skipped_reason": skipped},
				map[string]any{"pr_url": pr}, publishRunURL, writeToken(t))
			res, raw := runPublish(t, script, noProxyEnv())
			if !*res.Posted || res.SkippedReason != "" {
				t.Fatalf("posted = %v, skipped_reason %q — a drafted answer was dropped for its skipped_reason (%s)", *res.Posted, res.SkippedReason, raw)
			}
			if w := writes(f.seen()); len(w) != 1 || postedBody(t, w[0]) != publishAnswer+publishFooter {
				t.Errorf("the forge saw %+v, want the one POST of the draft", w)
			}
		})
	}
}

// Whatever bytes the draft holds, the node runs and decides: a NUL (which no
// exec argument can carry) is dropped from what is posted, an answer that
// opens on a template delimiter is an answer like any other, and a
// skipped_reason past the size of an environment entry is reported, capped
// — it rides converse_health's environment next.
func TestReviConversePublishReplySurvivesAnyBytes(t *testing.T) {
	requirePython(t)
	t.Run("a NUL in the answer", func(t *testing.T) {
		f := newConverseFakeForge(t)
		pr := f.srv.URL + "/o/r/pull/42"
		f.htmlFor = func(id int) string { return issueComment(pr, id) }
		script := renderReviConversePublish(t,
			map[string]any{"answer": "Re: op\n\nA NUL\x00 sits here (app.go:3).", "skipped_reason": ""},
			map[string]any{"pr_url": pr}, publishRunURL, writeToken(t))
		res, raw := runPublish(t, script, noProxyEnv())
		if !*res.Posted {
			t.Fatalf("nothing posted: %s", raw)
		}
		w := writes(f.seen())
		if len(w) != 1 || postedBody(t, w[0]) != "Re: op\n\nA NUL sits here (app.go:3)."+publishFooter {
			t.Errorf("the forge saw %+v, want the answer without its NUL", w)
		}
	})
	t.Run("an answer opening on a template delimiter", func(t *testing.T) {
		f := newConverseFakeForge(t)
		pr := f.srv.URL + "/o/r/pull/42"
		f.htmlFor = func(id int) string { return issueComment(pr, id) }
		answer := "{{ .Values.image }} is read from values.yaml (chart.yaml:3)."
		script := renderReviConversePublish(t,
			map[string]any{"answer": answer, "skipped_reason": ""},
			map[string]any{"pr_url": pr}, publishRunURL, writeToken(t))
		res, raw := runPublish(t, script, noProxyEnv())
		if !*res.Posted {
			t.Fatalf("an answer opening on {{ was refused: %s", raw)
		}
		if w := writes(f.seen()); len(w) != 1 || postedBody(t, w[0]) != answer+publishFooter {
			t.Errorf("the forge saw %+v, want the answer as written", w)
		}
	})
	t.Run("a skipped_reason past an environment entry", func(t *testing.T) {
		f := newConverseFakeForge(t)
		pr := f.srv.URL + "/o/r/pull/42"
		reason := strings.Repeat("the diff could not be read; ", 5000)
		script := renderReviConversePublish(t,
			map[string]any{"answer": "", "skipped_reason": reason},
			map[string]any{"pr_url": pr}, publishRunURL, writeToken(t))
		res, raw := runPublish(t, script, noProxyEnv())
		if *res.Posted || !strings.HasPrefix(res.SkippedReason, "the diff could not be read") {
			t.Fatalf("posted = %v, skipped_reason %.80q (%d bytes of output)", *res.Posted, res.SkippedReason, len(raw))
		}
		if n := len(res.SkippedReason); n > 2100 {
			t.Errorf("skipped_reason is %d bytes, want it capped near 2000 — converse_health reads it from its environment", n)
		}
		if hits := f.seen(); len(hits) != 0 {
			t.Errorf("the forge saw %+v, want no request", hits)
		}
	})
}

// Nothing to post, nothing sent: an empty draft (the agent's own reason,
// when it gave one, is the reason), no mounted token, a pr_url this step
// cannot reply on or that is not on the webhook's forge, a discussion id that
// is not one, an answer the edge never delivered. Each is posted=false with a
// reason converse_health shows, and the forge sees no request at all.
func TestReviConversePublishReplyPostsNothingWhenItCannot(t *testing.T) {
	requirePython(t)
	type tc struct {
		name       string
		input      map[string]any
		prPath     string // appended to the fake's origin; a full URL when it has a scheme
		disc       string
		base       string // forge_base_url, appended to the fake's origin; a full URL when it has a scheme
		noToken    bool
		unmapped   bool
		wantReason string
	}
	answer := map[string]any{"answer": publishAnswer, "skipped_reason": ""}
	for _, c := range []tc{
		{name: "empty answer", input: map[string]any{"answer": "", "skipped_reason": ""}, prPath: "/o/r/pull/1", wantReason: "empty answer"},
		{name: "blank answer", input: map[string]any{"answer": " \n\t\x00", "skipped_reason": ""}, prPath: "/o/r/pull/1", wantReason: "empty answer"},
		{name: "the agent skipped", input: map[string]any{"answer": "", "skipped_reason": "no question in the note"}, prPath: "/o/r/pull/1", wantReason: "no question in the note"},
		{name: "no token mounted", input: answer, prPath: "/o/r/pull/1", noToken: true, wantReason: "forge_token is not mounted"},
		{name: "pr_url empty", input: answer, prPath: "", wantReason: "pr_url is empty"},
		{name: "pr_url an issue", input: answer, prPath: "/o/r/issues/1", wantReason: "not a pull request URL"},
		{name: "pr_url a dot segment", input: answer, prPath: "/o/../pull/1", wantReason: "not a pull request URL"},
		{name: "pr_url not http", input: answer, prPath: "ftp://forge.example/o/r/pull/1", wantReason: "not an http(s) URL"},
		{name: "pr_url with userinfo", input: answer, prPath: "https://user:pw@forge.example/o/r/pull/1", wantReason: "userinfo"},
		{name: "pr_url off the webhook's forge", input: answer, prPath: "/o/r/pull/1", base: "https://forge.example", wantReason: "is not the configured forge"},
		{name: "pr_url outside the forge base's path", input: answer, prPath: "/other/g/p/-/merge_requests/1", base: "/gitlab", wantReason: "not under the configured forge base"},
		{name: "github discussion id not a comment id", input: answer, prPath: "/o/r/pull/1", disc: "../../x", wantReason: "not a GitHub review-comment id"},
		{name: "gitlab discussion id not one", input: answer, prPath: "/g/p/-/merge_requests/1", disc: "a/b", wantReason: "not a GitLab discussion id"},
		{name: "the answer never reached the node", input: map[string]any{"skipped_reason": ""}, prPath: "/o/r/pull/1", unmapped: true, wantReason: "input is unmapped"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newConverseFakeForge(t)
			pr := c.prPath
			if pr != "" && !strings.Contains(pr, "://") {
				pr = f.srv.URL + pr
			}
			vars := map[string]any{"pr_url": pr, "discussion_id": c.disc}
			if c.base != "" {
				vars["forge_base_url"] = c.base
				if !strings.Contains(c.base, "://") {
					vars["forge_base_url"] = f.srv.URL + c.base
				}
			}
			tokenPath := writeToken(t)
			if c.noToken {
				tokenPath = filepath.Join(t.TempDir(), "absent", "forge_token")
			}
			var unresolved []string
			if c.unmapped {
				unresolved = []string{"input.answer"}
			}
			script := renderReviConversePublish(t, c.input, vars, publishRunURL, tokenPath, unresolved...)
			res, raw := runPublish(t, script, noProxyEnv())
			if *res.Posted || res.ReplyURL != "" {
				t.Errorf("posted = %v, reply_url %q — want nothing posted (%s)", *res.Posted, res.ReplyURL, raw)
			}
			if !strings.Contains(res.SkippedReason, c.wantReason) {
				t.Errorf("skipped_reason %q does not name %q", res.SkippedReason, c.wantReason)
			}
			if hits := f.seen(); len(hits) != 0 {
				t.Errorf("the forge saw %+v, want no request", hits)
			}
		})
	}
}

// The forge token never leaves in a body, whatever put it in the draft — a
// prompt-injected agent that read the mounted file, a placeholder the engine
// left as written. The script itself refuses a text carrying the token
// raw or encoded (percent, base64, hex, its characters spread apart) or the
// token's placeholder raw or defused: nothing is sent, not even the
// duplicate check's read, and the reason names what the answer carried
// without carrying it. This is the body's own guard, rendered here with no
// engine guard in front of it.
func TestReviConversePublishReplyRefusesToPostTheForgeToken(t *testing.T) {
	requirePython(t)
	raw := []byte(publishToken)
	spread := strings.Join(strings.Split(publishToken, ""), " ")
	for _, c := range []struct {
		name, carried, wantWhat string
	}{
		{name: "raw", carried: "the key is " + publishToken, wantWhat: "the forge token"},
		{name: "single-quoted", carried: "the key is '" + publishToken + "'", wantWhat: "the forge token"},
		{name: "base64", carried: "base64: " + base64.StdEncoding.EncodeToString(raw), wantWhat: "the forge token"},
		{name: "base64 url-safe unpadded", carried: "b64: " + strings.TrimRight(base64.URLEncoding.EncodeToString(raw), "="), wantWhat: "the forge token"},
		{name: "hex", carried: "hex " + hex.EncodeToString(raw), wantWhat: "the forge token"},
		{name: "hex upper", carried: "HEX " + strings.ToUpper(hex.EncodeToString(raw)), wantWhat: "the forge token"},
		{name: "spread apart", carried: "spelled out: " + spread, wantWhat: "the forge token, spread apart"},
		{name: "zero-width joined", carried: "split: " + strings.Join(strings.Split(publishToken, ""), "\u200b"), wantWhat: "the forge token, spread apart"},
		{name: "the placeholder", carried: "the key is " + tokenPlaceholder, wantWhat: "the forge token's placeholder"},
		{name: "the defused placeholder", carried: "the key is " + tokenDefused, wantWhat: "the forge token's placeholder"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newConverseFakeForge(t)
			pr := f.srv.URL + "/g/p/-/merge_requests/7"
			script := renderReviConversePublish(t,
				map[string]any{"answer": "Re: op — « what is the key? »\n\n" + c.carried + " (app.go:3).", "skipped_reason": ""},
				map[string]any{"pr_url": pr, "discussion_id": "6a1f0c"}, publishRunURL, writeToken(t))
			res, out := runPublish(t, script, noProxyEnv())
			if *res.Posted || res.ReplyURL != "" {
				t.Fatalf("posted = %v, reply_url %q — an answer carrying %s was posted (%s)", *res.Posted, res.ReplyURL, c.wantWhat, out)
			}
			if want := "the answer carries " + c.wantWhat + ";"; !strings.Contains(res.SkippedReason, want) {
				t.Errorf("skipped_reason %q does not say %q", res.SkippedReason, want)
			}
			if hits := f.seen(); len(hits) != 0 {
				t.Errorf("the forge saw %+v, want no request at all", hits)
			}
			if strings.Contains(out, spread) {
				t.Errorf("the token, spread apart, reached the node's output: %s", out)
			}
		})
	}
}

// The forge's refusal is the reason: a 4xx or 5xx on the POST, or a
// redirect (never followed — the token would ride it to the host it names),
// leaves posted=false with the status and the forge's message, after the
// one write, with no re-read. The token never reaches the reason.
func TestReviConversePublishReplyReportsTheForgesRefusal(t *testing.T) {
	requirePython(t)
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
			f := newConverseFakeForge(t)
			other := newConverseFakeForge(t)
			f.postStatus, f.postAnswer = c.status, c.answer
			if c.redirect {
				f.redirectTo = other.srv.URL + "/stolen"
			}
			pr := f.srv.URL + "/g/p/-/merge_requests/7"
			script := renderReviConversePublish(t,
				map[string]any{"answer": publishAnswer, "skipped_reason": ""},
				map[string]any{"pr_url": pr, "discussion_id": "6a1f0c"}, publishRunURL, writeToken(t))
			res, raw := runPublish(t, script, noProxyEnv())
			if *res.Posted || res.ReplyURL != "" {
				t.Errorf("posted = %v, reply_url %q, want nothing posted (%s)", *res.Posted, res.ReplyURL, raw)
			}
			for _, want := range c.wantReason {
				if !strings.Contains(res.SkippedReason, want) {
					t.Errorf("skipped_reason %q does not name %q", res.SkippedReason, want)
				}
			}
			hits := f.seen()
			if w := writes(hits); len(w) != 1 || w[0].Method != http.MethodPost {
				t.Errorf("the forge saw writes %+v, want the one POST", w)
			}
			if g := gets(hits); !slices.Equal(g, []string{"/api/v4/projects/g%2Fp/merge_requests/7/discussions/6a1f0c"}) {
				t.Errorf("the reads were %v, want the duplicate check alone and no re-read", g)
			}
			if got := other.seen(); len(got) != 0 {
				t.Errorf("the redirect target saw %+v — the token followed a redirect", got)
			}
		})
	}
}

// A post the forge accepted but the re-read cannot find is not an answer
// the operator can see: posted=false, the reason naming the failed GET; the
// same for a re-read that answers with another note, and for a created note
// that came back with no id to read it by. Still exactly one write.
func TestReviConversePublishReplyNeedsTheReReadToFindTheNote(t *testing.T) {
	requirePython(t)
	for _, c := range []struct {
		name       string
		noID       bool
		other      bool
		wantReason string
		wantReads  int
	}{
		{name: "the re-read misses", wantReason: "re-reading it failed: GET", wantReads: 3},
		{name: "the re-read finds another note", other: true, wantReason: "re-reading it failed: GET", wantReads: 3},
		{name: "no note id", noID: true, wantReason: "with no note id", wantReads: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newConverseFakeForge(t)
			f.postNoID, f.readOther = c.noID, c.other
			if !c.noID && !c.other {
				f.readStatus = http.StatusNotFound
			}
			pr := f.srv.URL + "/o/r/pull/42"
			f.htmlFor = func(id int) string { return issueComment(pr, id) }
			script := renderReviConversePublish(t,
				map[string]any{"answer": publishAnswer, "skipped_reason": ""},
				map[string]any{"pr_url": pr}, publishRunURL, writeToken(t))
			res, raw := runPublish(t, script, noProxyEnv())
			if *res.Posted || res.ReplyURL != "" {
				t.Errorf("posted = %v, reply_url %q — a note the re-read did not find is not posted (%s)", *res.Posted, res.ReplyURL, raw)
			}
			if !strings.Contains(res.SkippedReason, c.wantReason) {
				t.Errorf("skipped_reason %q does not name %q", res.SkippedReason, c.wantReason)
			}
			hits := f.seen()
			if w := writes(hits); len(w) != 1 {
				t.Errorf("%d writes, want exactly one", len(w))
			}
			if reads := len(gets(hits)); reads != c.wantReads {
				t.Errorf("%d reads, want %d (the duplicate check, then the re-reads)", reads, c.wantReads)
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
	requirePython(t)
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
			f := newConverseFakeForge(t)
			pr := f.srv.URL + "/o/r/pull/42"
			f.htmlFor = func(id int) string { return issueComment(pr, id) }
			script := renderReviConversePublish(t,
				map[string]any{"answer": c.answer, "skipped_reason": ""},
				map[string]any{"pr_url": pr}, publishRunURL, writeToken(t))
			res, raw := runPublish(t, script, noProxyEnv())
			if !*res.Posted {
				t.Fatalf("nothing posted: %.300s", raw)
			}
			w := writes(f.seen())
			if len(w) != 1 {
				t.Fatalf("%d writes, want 1", len(w))
			}
			body := postedBody(t, w[0])
			if !strings.HasSuffix(body, publishFooter) {
				t.Errorf("the posted body does not end with the run footer: …%q", body[max(0, len(body)-200):])
			}
			if n := utf16Units(body); n > 65536 {
				t.Errorf("the posted body is %d UTF-16 units, past GitHub's 65536", n)
			}
			marked := strings.Contains(body, "answer truncated to fit the forge comment limit")
			if marked != c.wantTruncated || strings.Contains(res.Summary, "truncated") != c.wantTruncated {
				t.Errorf("truncation marker in body %v, in summary %v, want %v", marked, strings.Contains(res.Summary, "truncated"), c.wantTruncated)
			}
			opens := 0
			for _, line := range strings.Split(strings.TrimSuffix(body, publishFooter), "\n") {
				if strings.HasPrefix(line, fence) {
					opens++
				}
			}
			if opens%2 != 0 {
				t.Errorf("the body leaves a code fence open before the footer (%d fence lines)", opens)
			}
		})
	}
}

// The footer follows docs/agents/bots/run-link.md whatever the run gives
// it: no address prints the id alone, never a host; a run id that is not
// plain is never pasted into a link label.
func TestReviConversePublishReplyFooterFollowsTheRunLinkRules(t *testing.T) {
	requirePython(t)
	for _, c := range []struct {
		name, runURL, wantTail string
	}{
		{name: "a run url", runURL: publishRunURL, wantTail: publishFooter},
		{name: "no public address", runURL: "", wantTail: "\n\n---\n🤖 [iterion](https://iterion.cloud) · run `run-converse`"},
		{name: "an engine predating run.url", runURL: "{{run.url}}", wantTail: "\n\n---\n🤖 [iterion](https://iterion.cloud) · run `run-converse`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newConverseFakeForge(t)
			pr := f.srv.URL + "/o/r/pull/42"
			f.htmlFor = func(id int) string { return issueComment(pr, id) }
			script := renderReviConversePublish(t,
				map[string]any{"answer": publishAnswer, "skipped_reason": ""},
				map[string]any{"pr_url": pr}, c.runURL, writeToken(t))
			res, raw := runPublish(t, script, noProxyEnv())
			if !*res.Posted {
				t.Fatalf("nothing posted: %s", raw)
			}
			w := writes(f.seen())
			if len(w) != 1 {
				t.Fatalf("%d writes, want 1", len(w))
			}
			body := postedBody(t, w[0])
			if !strings.HasSuffix(body, c.wantTail) {
				t.Errorf("the body ends %q, want the footer %q", body[max(0, len(body)-len(c.wantTail)-40):], c.wantTail)
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
// logs the CONNECT and refuses it shows where the requests were going; the
// reason names the URL the POST was going to.
func TestReviConversePublishReplyDerivesTheProductionAPIBase(t *testing.T) {
	requirePython(t)
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
			proxy, seen := connectLogger(t)
			script := renderReviConversePublish(t,
				map[string]any{"answer": publishAnswer, "skipped_reason": ""},
				map[string]any{"pr_url": c.pr, "discussion_id": c.disc}, publishRunURL, writeToken(t))
			res, raw := runPublish(t, script, noProxyEnv("HTTPS_PROXY="+proxy, "https_proxy="+proxy))
			if *res.Posted {
				t.Fatalf("posted through a proxy that refuses every tunnel: %s", raw)
			}
			got := seen()
			if len(got) == 0 {
				t.Fatalf("the proxy saw no CONNECT, want %s", c.wantConnect)
			}
			for _, line := range got {
				if line != c.wantConnect+" HTTP/1.1" && line != c.wantConnect+" HTTP/1.0" {
					t.Errorf("the proxy saw %q, want only %s", line, c.wantConnect)
				}
			}
			if !strings.Contains(res.SkippedReason, c.wantURL) {
				t.Errorf("skipped_reason %q does not name %s", res.SkippedReason, c.wantURL)
			}
		})
	}
}
