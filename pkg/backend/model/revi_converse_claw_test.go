package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// reviConverseBundle is the shipped revi-converse bundle, from this package.
const reviConverseBundle = "../../../bots/revi-converse"

// TestReviConverseAnswersOnClawThroughItsDeclaredSurface proves the answer
// path of revi-converse's converse_agent on claw, where a routing cell moves
// it: the node as main.bot compiles it — its `tools:` list, its
// `tool_max_steps:` — runs through ClawExecutor with a launch override onto
// claw, and the real claw tools (bash, read_file) work a temp git workspace
// holding a diff and the bundle's real forge-reply skill, mirrored where the
// engine mirrors it. The model is scripted to do what the prompt and the
// skill ask, one tool call per turn: name the changed files, read the diff,
// read a changed source, read the skill, POST the answer with the token the
// shell reads from its 0600 file, VERIFY by re-fetching the thread, and
// report the note's URL from that response. A fake forge serves the GitHub
// and the GitLab shape of the thread.
//
// The witness is the forge's side, not the model's: exactly one reply POST
// carrying the answer, then the VERIFY GET, and the node's output naming the
// note the forge created, on the PR's host. A node holding no tool never
// posts; one whose loop keeps claw's default bound
// (DefaultClawToolMaxSteps) posts and never verifies.
func TestReviConverseAnswersOnClawThroughItsDeclaredSurface(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "git", "curl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH: the converse loop shells out to it", bin)
		}
	}
	u, err := unit.LoadDir(filepath.Join(reviConverseBundle, "main.bot"))
	if err != nil {
		t.Fatalf("load revi-converse: %v", err)
	}
	compiled := ir.Compile(u.Merged)
	if compiled.HasErrors() {
		t.Fatalf("compile revi-converse: %v", compiled.Diagnostics)
	}
	wf := compiled.Workflow
	node, ok := wf.Nodes["converse_agent"].(*ir.AgentNode)
	if !ok {
		t.Fatalf("converse_agent is %T, want *ir.AgentNode", wf.Nodes["converse_agent"])
	}
	skill, err := os.ReadFile(filepath.Join(reviConverseBundle, "skills", "forge-reply.md"))
	if err != nil {
		t.Fatalf("read the bundle's forge-reply skill: %v", err)
	}

	for _, shape := range []converseForgeShape{githubThread, gitlabThread} {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			ws := converseWorkspace(t, home, skill)
			token := "forge-token-" + shape.name
			tokenFile := filepath.Join(t.TempDir(), "forge_token")
			if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			forge := newConverseForge(t, shape, token)
			prURL := forge.srv.URL + shape.prPath
			answer := "Re: op — \"why does A return 2 now?\"\n\n" +
				"`A` returns 2 because the change rewrites its body (app.go:3: `return 2`); " +
				"nothing else in the diff touches it.\n\n---\n🤖 iterion · run `run-converse`"

			mdl := &converseModel{
				shape: shape, forgeURL: forge.srv.URL, prURL: prURL, ws: ws,
				tokenFile: tokenFile, answer: answer,
			}
			reg := NewRegistry()
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mdl, nil })
			tools := tool.NewRegistry()
			// The shell the agent gets: a HOME and a TMPDIR this test owns,
			// and the fake forge reached directly, not through a proxy the
			// environment may set.
			shellEnv := []string{
				"HOME=" + home, "TMPDIR=" + t.TempDir(),
				"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
			}
			if err := tool.RegisterClawBuiltinsWithEnv(tools, ws, shellEnv); err != nil {
				t.Fatal(err)
			}
			if err := tool.RegisterClawTodo(tools); err != nil {
				t.Fatal(err)
			}
			// The route re-targeted whole, as a routing cell does: backend,
			// model and provider. The node's own `anthropic` hint would make
			// claw build an Anthropic-direct client instead of asking the
			// registry, which serves the scripted model.
			var onClaw ModelOverrides
			onClaw.SetBackend(node.ID, delegate.BackendClaw)
			onClaw.SetModel(node.ID, "anthropic/claude-opus-5-5")
			onClaw.SetProvider(node.ID, "auto")
			e := newTestClawExecutor(reg, wf, WithWorkDir(ws), WithToolRegistry(tools),
				WithModelOverrides(onClaw), WithLogger(iterlog.Nop()))
			e.SetVars(map[string]any{
				"workspace_dir": ws, "base_ref": "main", "pr_url": prURL,
				"discussion_id": shape.discussion, "converse_question": "why does A return 2 now?",
				"trigger_note": "/revi why does A return 2 now?", "thread_context": "", "replier": "op",
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			out, err := e.Execute(ctx, node, map[string]any{})
			if err != nil {
				t.Fatalf("converse_agent on claw: %v", err)
			}

			offered, results := mdl.observed()
			for _, want := range node.Tools {
				if !slices.Contains(offered, want) {
					t.Errorf("the loop's model was offered %v, which lacks the node's %q", offered, want)
				}
			}
			for step, want := range map[string]string{
				"names": "app.go",
				"diff":  "+func A() int { return 2 }",
				"skill": shape.skillCall,
			} {
				if !strings.Contains(results[step], want) {
					t.Errorf("the %s step's tool result does not hold %q: %q", step, want, results[step])
				}
			}

			posts, log := forge.seen()
			if want := []string{"POST reply", "GET thread"}; !slices.Equal(log, want) {
				t.Errorf("the forge saw %v, want %v — one reply, then its VERIFY", log, want)
			}
			if len(posts) != 1 || posts[0] != answer {
				t.Errorf("the forge received reply bodies %q, want exactly the answer %q", posts, answer)
			}
			for _, body := range posts {
				if strings.HasPrefix(strings.TrimSpace(body), "/revi") {
					t.Errorf("a reply body opens with /revi, which re-fires the trigger: %q", body)
				}
			}
			if out["posted"] != true {
				t.Errorf("output posted = %v, want true (skipped_reason %q)", out["posted"], out["skipped_reason"])
			}
			replyURL, _ := out["reply_url"].(string)
			if want := forge.noteURL(prURL); replyURL != want {
				t.Errorf("output reply_url = %q, want the forge's note %q", replyURL, want)
			}
			if got, pr := urlHost(t, replyURL), urlHost(t, prURL); !strings.EqualFold(got, pr) {
				t.Errorf("output reply_url %q is on host %q, not the PR's %q", replyURL, got, pr)
			}
		})
	}
}

func urlHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// converseWorkspace is a git checkout on a feature branch one commit past
// main, with the bundle's skill mirrored under .claude/skills/ — where the
// engine mirrors a bundle's skills and where the system prompt sends the
// agent to read it.
func converseWorkspace(t *testing.T, home string, skill []byte) string {
	t.Helper()
	ws := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		// gittest.Cmd refuses git's auto-maintenance (it would keep writing
		// under .git/objects past the command and race t.TempDir()'s
		// removal) and cuts the operator's config off; HOME stays the
		// test's scratch home.
		cmd := gittest.Cmd(ws, args...)
		cmd.Env = append(cmd.Env, "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("app.go", "package app\n\nfunc A() int { return 1 }\n")
	git("add", "app.go")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "feat")
	write("app.go", "package app\n\nfunc A() int { return 2 }\n")
	git("commit", "-q", "-am", "change A")
	write(filepath.Join(".claude", "skills", "forge-reply.md"), string(skill))
	return ws
}

// converseForgeShape is one forge's reply thread: where the PR lives, the
// reply call as forge-reply.md documents it, and the REST paths and token
// header that call takes.
type converseForgeShape struct {
	name       string
	prPath     string // the PR's web path on the forge host
	discussion string // the thread the operator's note opened
	skillCall  string // the reply call, verbatim from the skill
	authHeader string
	authValue  func(token string) string
	postPath   string // the reply POST, escaped
	threadPath string // the VERIFY GET, escaped
}

var githubThread = converseForgeShape{
	name:       "github",
	prPath:     "/o/r/pull/42",
	discussion: "4242",
	skillCall:  "POST repos/:owner/:repo/pulls/:number/comments/:comment_id/replies",
	authHeader: "Authorization",
	authValue:  func(token string) string { return "Bearer " + token },
	postPath:   "/repos/o/r/pulls/42/comments/4242/replies",
	threadPath: "/repos/o/r/pulls/42/comments",
}

var gitlabThread = converseForgeShape{
	name:       "gitlab",
	prPath:     "/g/p/-/merge_requests/7",
	discussion: "6a1f0c",
	skillCall:  "POST projects/:id/merge_requests/:iid/discussions/:discussion_id/notes",
	authHeader: "PRIVATE-TOKEN",
	authValue:  func(token string) string { return token },
	postPath:   "/api/v4/projects/g%2Fp/merge_requests/7/discussions/6a1f0c/notes",
	threadPath: "/api/v4/projects/g%2Fp/merge_requests/7/discussions/6a1f0c",
}

// converseForge is the fake forge: it accepts a reply on the thread's POST
// path, serves the thread on its GET path, and records every request in
// arrival order.
type converseForge struct {
	shape converseForgeShape
	token string
	srv   *httptest.Server

	mu    sync.Mutex
	posts []string
	log   []string
}

const converseFirstNoteID = 9001

func newConverseForge(t *testing.T, shape converseForgeShape, token string) *converseForge {
	f := &converseForge{shape: shape, token: token}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *converseForge) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.EscapedPath()
	if r.Header.Get(f.shape.authHeader) != f.shape.authValue(f.token) {
		f.log = append(f.log, "UNAUTHORIZED "+r.Method+" "+path)
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && path == f.shape.postPath:
		var payload struct {
			Body string `json:"body"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &payload); err != nil || payload.Body == "" {
			f.log = append(f.log, "BAD POST "+path)
			http.Error(w, `{"message":"body is missing"}`, http.StatusBadRequest)
			return
		}
		f.posts = append(f.posts, payload.Body)
		f.log = append(f.log, "POST reply")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(f.noteLocked(len(f.posts)-1, payload.Body))
	case r.Method == http.MethodGet && path == f.shape.threadPath:
		f.log = append(f.log, "GET thread")
		notes := []map[string]any{}
		for i, body := range f.posts {
			notes = append(notes, f.noteLocked(i, body))
		}
		if f.shape.name == "gitlab" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": f.shape.discussion, "notes": notes})
			return
		}
		_ = json.NewEncoder(w).Encode(notes)
	default:
		f.log = append(f.log, "UNEXPECTED "+r.Method+" "+path)
		http.NotFound(w, r)
	}
}

// noteLocked is the forge's record of reply i: GitHub's review comment
// carries its own html_url, GitLab's note an id the PR URL anchors.
func (f *converseForge) noteLocked(i int, body string) map[string]any {
	id := converseFirstNoteID + i
	if f.shape.name == "gitlab" {
		return map[string]any{"id": id, "body": body}
	}
	parent, _ := strconv.Atoi(f.shape.discussion)
	return map[string]any{
		"id": id, "in_reply_to_id": parent, "body": body,
		"html_url": fmt.Sprintf("%s%s#discussion_r%d", f.srv.URL, f.shape.prPath, id),
	}
}

// noteURL is the URL of the first reply, as the forge names it.
func (f *converseForge) noteURL(prURL string) string {
	if f.shape.name == "gitlab" {
		return fmt.Sprintf("%s#note_%d", prURL, converseFirstNoteID)
	}
	return fmt.Sprintf("%s%s#discussion_r%d", f.srv.URL, f.shape.prPath, converseFirstNoteID)
}

func (f *converseForge) seen() (posts, log []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.posts), slices.Clone(f.log)
}

// converseModel is the scripted converse agent: one tool call per turn, in
// the order the system prompt and the skill give, then the structured
// output. It reports what its tools actually returned — posted, and the
// note's URL, only when its VERIFY read the reply back from the forge — so a
// loop cut short, or a route with no tool, ends in an honest posted=false.
type converseModel struct {
	shape     converseForgeShape
	forgeURL  string
	prURL     string
	ws        string
	tokenFile string
	answer    string

	mu      sync.Mutex
	next    int
	offered []string
	results map[string]string
}

// converseStep is one tool call of the loop, named for the result it reads.
type converseStep struct {
	label, tool string
	input       map[string]any
}

func (m *converseModel) steps() []converseStep {
	mergeBase := "$(git -C " + shellquote.Quote(m.ws) + " merge-base main HEAD)"
	token := "$(cat " + shellquote.Quote(m.tokenFile) + ")"
	payload, _ := json.Marshal(map[string]string{"body": m.answer})
	return []converseStep{
		{"names", "bash", map[string]any{"command": "git -C " + shellquote.Quote(m.ws) + " diff --name-only " + mergeBase}},
		{"diff", "bash", map[string]any{"command": "git -C " + shellquote.Quote(m.ws) + " diff " + mergeBase}},
		{"source", "read_file", map[string]any{"path": filepath.Join(m.ws, "app.go")}},
		{"skill", "read_file", map[string]any{"path": filepath.Join(m.ws, ".claude", "skills", "forge-reply.md")}},
		{"post", "bash", map[string]any{"command": "curl -sS -H " + m.authArg(token) +
			" -H 'Content-Type: application/json' -X POST " + shellquote.Quote(m.forgeURL+m.shape.postPath) +
			" -d @- <<'ITERION_ANSWER'\n" + string(payload) + "\nITERION_ANSWER"}},
		{"verify", "bash", map[string]any{"command": "curl -sS -H " + m.authArg(token) + " " + shellquote.Quote(m.forgeURL+m.shape.threadPath)}},
	}
}

// authArg is the forge's token header as one shell word, the token read by
// the shell from its file — never written into the command itself.
func (m *converseModel) authArg(tokenExpr string) string {
	return `"` + m.shape.authHeader + ": " + m.shape.authValue(tokenExpr) + `"`
}

func (m *converseModel) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.results == nil {
		m.results = map[string]string{}
	}
	steps := m.steps()
	byID := map[string]string{}
	for i, s := range steps {
		byID[fmt.Sprintf("converse_%d_%s", i, s.label)] = s.label
	}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if block.Type != "tool_result" {
				continue
			}
			if label, ok := byID[block.ToolUseID]; ok {
				var text strings.Builder
				for _, c := range block.Content {
					text.WriteString(c.Text)
				}
				m.results[label] = text.String()
			}
		}
	}
	var names []string
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	var evs []api.StreamEvent
	switch {
	case slices.Contains(names, "bash") && m.next < len(steps):
		if m.offered == nil {
			m.offered = names
		}
		s := steps[m.next]
		in, _ := json.Marshal(s.input)
		evs = toolUseEvents(fmt.Sprintf("converse_%d_%s", m.next, s.label), s.tool, string(in), 100, 20)
		m.next++
	case slices.Contains(names, "structured_output"):
		evs = toolUseEvents("converse_output", "structured_output", m.outputLocked(slices.Contains(names, "bash") || m.next > 0), 100, 20)
	default:
		evs = textEvents(m.outputLocked(slices.Contains(names, "bash") || m.next > 0), 100, 20)
	}
	ch := make(chan api.StreamEvent, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// outputLocked is converse_output from what the tools returned.
func (m *converseModel) outputLocked(hadTools bool) string {
	out := map[string]any{
		"posted": false, "forge": m.shape.name, "reply_url": "", "answer_preview": "",
		"summary": "Question: why does A return 2 now?",
	}
	switch reply, ok := m.verifiedReplyLocked(); {
	case ok:
		out["posted"], out["reply_url"], out["skipped_reason"] = true, reply, ""
		out["answer_preview"] = m.answer[:min(len(m.answer), 200)]
		out["summary"] = "Answered from the diff (app.go:3); POSTed in the thread and VERIFIED by re-fetching it: " + reply
	case !hadTools:
		out["skipped_reason"] = "no shell on this route: could neither read the diff nor post"
	default:
		out["skipped_reason"] = "the tool loop ended before the reply was verified"
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// verifiedReplyLocked reads the VERIFY step's response the way the skill
// says: the reply carrying the answer, its URL as the forge names it.
func (m *converseModel) verifiedReplyLocked() (string, bool) {
	raw, ok := m.results["verify"]
	if !ok {
		return "", false
	}
	type note struct {
		ID          int    `json:"id"`
		InReplyToID int    `json:"in_reply_to_id"`
		Body        string `json:"body"`
		HTMLURL     string `json:"html_url"`
	}
	var notes []note
	if m.shape.name == "gitlab" {
		var thread struct {
			Notes []note `json:"notes"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &thread) != nil {
			return "", false
		}
		notes = thread.Notes
	} else if json.Unmarshal([]byte(strings.TrimSpace(raw)), &notes) != nil {
		return "", false
	}
	for _, n := range notes {
		if n.Body != m.answer {
			continue
		}
		if m.shape.name == "gitlab" {
			return fmt.Sprintf("%s#note_%d", m.prURL, n.ID), true
		}
		if strconv.Itoa(n.InReplyToID) == m.shape.discussion && n.HTMLURL != "" {
			return n.HTMLURL, true
		}
	}
	return "", false
}

func (m *converseModel) observed() (offered []string, results map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	results = make(map[string]string, len(m.results))
	for k, v := range m.results {
		results[k] = v
	}
	return slices.Clone(m.offered), results
}
