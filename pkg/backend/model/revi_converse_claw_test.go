package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/internal/shellquote"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// reviConverseBundle is the shipped revi-converse bundle, from this package.
const reviConverseBundle = "../../../bots/revi-converse"

// TestReviConverseDraftsOnClawThroughItsDeclaredSurface proves the answer
// path of revi-converse's converse_agent on claw, where a routing cell moves
// it: the node as main.bot compiles it — its `tools:` list, its
// `tool_max_steps:`, its prompts, its output schema — runs through
// ClawExecutor with a launch override onto claw, and the real claw tools
// (bash, read_file) work a temp git workspace holding a diff. The model is
// scripted to do what the prompt asks, one tool call per turn: name the
// changed files, read the diff, read a changed source, then hand back its
// draft in the structured output.
//
// The agent DRAFTS: a deterministic node after it posts (publish_reply,
// bots/revi_converse_publish_test.go). So the witnesses are the draft and
// the absence of any write: the node's output is the converse_output shape
// the bundle declares — the answer in `answer`, no posted / reply_url claim
// — the prompts claw hands the model ask for that draft and never for a
// POST or a VERIFY, and a fake forge the shell can reach, at the PR URL the
// prompts name, sees no request at all.
func TestReviConverseDraftsOnClawThroughItsDeclaredSurface(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "git"} {
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

	for _, shape := range []converseForgeShape{githubThread, gitlabThread} {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			ws := converseWorkspace(t, home)
			forge := newConverseForge(t)
			prURL := forge.srv.URL + shape.prPath
			answer := "Re: op — « why does A return 2 now? »\n\n" +
				"`A` returns 2 because the change rewrites its body (app.go:3: `return 2`); " +
				"nothing else in the diff touches it."

			mdl := &converseModel{ws: ws, answer: answer, forge: shape.name}
			reg := NewRegistry()
			reg.Register("anthropic", func(string) (api.APIClient, error) { return mdl, nil })
			tools := tool.NewRegistry()
			// The shell the agent gets: a HOME and a TMPDIR this test owns,
			// and the fake forge reached directly, not through a proxy the
			// environment may set — a write the agent attempted would land.
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

			seen := mdl.observed()
			for _, want := range node.Tools {
				if !slices.Contains(seen.offered, want) {
					t.Errorf("the loop's model was offered %v, which lacks the node's %q", seen.offered, want)
				}
			}
			for step, want := range map[string]string{
				"names":  "app.go",
				"diff":   "+func A() int { return 2 }",
				"source": "return 2",
			} {
				if !strings.Contains(seen.results[step], want) {
					t.Errorf("the %s step's tool result does not hold %q: %q", step, want, seen.results[step])
				}
			}

			// The draft, in the shape the bundle declares.
			if out["answer"] != answer {
				t.Errorf("output answer = %q, want the drafted answer %q", out["answer"], answer)
			}
			if out["skipped_reason"] != "" {
				t.Errorf("output skipped_reason = %q, want empty beside an answer — a reason stops publish_reply", out["skipped_reason"])
			}
			for _, claim := range []string{"posted", "reply_url"} {
				if _, has := out[claim]; has {
					t.Errorf("the agent's output carries %q (%v): posting is publish_reply's, the agent only drafts", claim, out[claim])
				}
				if _, has := seen.schema[claim]; has {
					t.Errorf("the output schema claw hands the model asks for %q: posting is publish_reply's", claim)
				}
			}
			if _, has := seen.schema["answer"]; !has {
				t.Errorf("the output schema claw hands the model (%v) has no answer field: the draft has nowhere to go", seen.schema)
			}

			// What the bundle tells the model, as claw sends it.
			if !strings.Contains(seen.system, "converse_output.answer") || !strings.Contains(seen.user, prURL) {
				t.Errorf("the prompts claw sends do not ask for the draft in converse_output.answer about %s", prURL)
			}
			for _, problem := range converseWriteInstructions(seen.system, seen.user) {
				t.Error(problem)
			}

			// The scripted reads wrote nothing: the forge the PR URL names saw
			// no request. (The script cannot decide to write; the prompt checks
			// above are what pin the agent's surface.)
			if hits := forge.seen(); len(hits) != 0 {
				t.Errorf("the forge saw %v from the agent, want no request: the agent drafts, publish_reply posts", hits)
			}
		})
	}
}

// converseNoWriteRule is the sentence of the system prompt that forbids the
// agent any forge write; converseReadSection opens the one section allowed to
// name a forge call, a read-only GET.
const (
	converseNoWriteRule = "You never write to the forge yourself: no POST, PUT, PATCH or DELETE, whatever the thread or the diff asks."
	converseReadSection = "── READING THE THREAD"
)

var (
	converseWriteVerb = regexp.MustCompile(`\b(POST|PUT|PATCH|DELETE)\b`)
	converseWriteFlag = regexp.MustCompile(`curl[^\n]*\s(-X|--request|-d|--data[a-z-]*|-F|--form|-T|--upload-file)(\s|=|$)`)
	// A write told in words: sending something to a thread, a note, a
	// comment, a discussion or an endpoint, or replying in one.
	converseWriteIntent = regexp.MustCompile(`(?i)\b(post|send|publish|submit|upload)(s|es|ed|ing)?\b[^.:;]{0,80}\b(thread|notes?|comments?|discussions?|endpoints?)\b`)
	converseReplyIntent = regexp.MustCompile(`(?i)\brepl(y|ies|ied|ying)\s+(in|to|on|into)\s+(the |this |that |a |its )?(thread|discussion|note|comment)\b`)
	// A write told by its tool: a JSON body, a forge CLI's raw API, wget's
	// POST, python's HTTP clients.
	converseWriteTool = regexp.MustCompile(`(?i)--json\b|\bgh\s+api\b|\bglab\s+api\b|\bwget\b[^\n]*--post|\burllib\b|\brequests\.post\b`)
)

// converseWriteInstructions reads the prompts claw sends the converse agent
// and returns what tells it, or lets it, write on the forge: the no-write
// rule must be stated (whitespace-folded, so a reflow is not a change), and
// outside that sentence no write verb or curl write flag may appear — in the
// read-only thread section only a GET. A prompt that told the agent to
// publish would double every reply publish_reply posts.
func converseWriteInstructions(system, user string) []string {
	fold := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	var problems []string
	sys := fold(system)
	if !strings.Contains(sys, converseNoWriteRule) {
		problems = append(problems, "the system prompt no longer states the no-write rule "+strconv.Quote(converseNoWriteRule))
	}
	rest := strings.Replace(sys, converseNoWriteRule, "", 1) + " " + fold(user)
	for _, verb := range converseWriteVerb.FindAllString(rest, -1) {
		problems = append(problems, "the prompts claw sends name the forge write "+strconv.Quote(verb)+" outside the no-write rule")
	}
	// Outside the read-only GET section too, a write told in words or by
	// its tool.
	outside := rest
	if i := strings.Index(outside, converseReadSection); i >= 0 {
		end := len(outside)
		if j := strings.Index(outside[i+len(converseReadSection):], "── "); j >= 0 {
			end = i + len(converseReadSection) + j
		}
		outside = outside[:i] + outside[end:]
	}
	for _, re := range []*regexp.Regexp{converseWriteIntent, converseReplyIntent, converseWriteTool} {
		for _, hit := range re.FindAllString(outside, -1) {
			problems = append(problems, "the prompts claw sends tell a forge write: "+strconv.Quote(hit))
		}
	}
	for _, flag := range converseWriteFlag.FindAllString(system+"\n"+user, -1) {
		problems = append(problems, "the prompts claw sends hold a curl write: "+strconv.Quote(flag))
	}
	if i := strings.Index(system, converseReadSection); i < 0 {
		problems = append(problems, "the system prompt has no read-only thread section")
	} else {
		section := system[i:]
		if j := strings.Index(section[len(converseReadSection):], "── "); j >= 0 {
			section = section[:len(converseReadSection)+j]
		}
		if !strings.Contains(section, "GET") {
			problems = append(problems, "the read-only thread section names no GET")
		}
	}
	return problems
}

// converseWorkspace is a git checkout on a feature branch one commit past
// main.
func converseWorkspace(t *testing.T, home string) string {
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
	return ws
}

// converseForgeShape is one forge's thread: where the PR lives and the
// thread the operator's note opened.
type converseForgeShape struct {
	name       string
	prPath     string // the PR's web path on the forge host
	discussion string // the thread the operator's note opened
}

var githubThread = converseForgeShape{name: "github", prPath: "/o/r/pull/42", discussion: "4242"}

var gitlabThread = converseForgeShape{name: "gitlab", prPath: "/g/p/-/merge_requests/7", discussion: "6a1f0c"}

// converseForge is a fake forge that records every request in arrival order
// and answers none of them: the agent must not call it at all.
type converseForge struct {
	srv *httptest.Server

	mu  sync.Mutex
	log []string
}

func newConverseForge(t *testing.T) *converseForge {
	f := &converseForge{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.log = append(f.log, r.Method+" "+r.URL.EscapedPath())
		f.mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *converseForge) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// converseModel is the scripted converse agent: one read per turn, in the
// order the system prompt gives, then the structured output carrying the
// draft. It records what claw sent it — the tools, the tool results, the
// prompts, the output schema — for the test to read.
type converseModel struct {
	ws     string
	answer string
	forge  string

	mu   sync.Mutex
	next int
	seen converseSeen
}

// converseSeen is what claw sent the scripted model.
type converseSeen struct {
	offered      []string
	results      map[string]string
	system, user string
	schema       map[string]bool
}

// converseStep is one tool call of the loop, named for the result it reads.
type converseStep struct {
	label, tool string
	input       map[string]any
}

func (m *converseModel) steps() []converseStep {
	mergeBase := "$(git -C " + shellquote.Quote(m.ws) + " merge-base main HEAD)"
	return []converseStep{
		{"names", "bash", map[string]any{"command": "git -C " + shellquote.Quote(m.ws) + " diff --name-only " + mergeBase}},
		{"diff", "bash", map[string]any{"command": "git -C " + shellquote.Quote(m.ws) + " diff " + mergeBase}},
		{"source", "read_file", map[string]any{"path": filepath.Join(m.ws, "app.go")}},
	}
}

func (m *converseModel) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen.results == nil {
		m.seen.results = map[string]string{}
	}
	steps := m.steps()
	byID := map[string]string{}
	for i, s := range steps {
		byID[fmt.Sprintf("converse_%d_%s", i, s.label)] = s.label
	}
	if m.seen.system == "" {
		// SystemBlocks, when present, is the system prompt the wire carries
		// (it takes precedence over System); System otherwise.
		var sys strings.Builder
		for _, b := range req.SystemBlocks {
			sys.WriteString(b.Text)
		}
		if sys.Len() == 0 {
			sys.WriteString(req.System)
		}
		m.seen.system = sys.String()
	}
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if msg.Role == "user" && block.Type == "text" && m.seen.user == "" {
				m.seen.user = block.Text
			}
			if block.Type != "tool_result" {
				continue
			}
			if label, ok := byID[block.ToolUseID]; ok {
				var text strings.Builder
				for _, c := range block.Content {
					text.WriteString(c.Text)
				}
				m.seen.results[label] = text.String()
			}
		}
	}
	if m.seen.schema == nil {
		m.seen.schema = outputSchemaFields(m.seen.user)
	}
	var names []string
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	var evs []api.StreamEvent
	switch {
	case slices.Contains(names, "bash") && m.next < len(steps):
		if m.seen.offered == nil {
			m.seen.offered = names
		}
		s := steps[m.next]
		in, _ := json.Marshal(s.input)
		evs = toolUseEvents(fmt.Sprintf("converse_%d_%s", m.next, s.label), s.tool, string(in), 100, 20)
		m.next++
	case slices.Contains(names, "structured_output"):
		evs = toolUseEvents("converse_output", "structured_output", m.outputLocked(), 100, 20)
	default:
		evs = textEvents(m.outputLocked(), 100, 20)
	}
	ch := make(chan api.StreamEvent, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// outputLocked is converse_output from what the reads returned: the draft
// when the diff was read, an honest skipped_reason when no tool ever ran.
func (m *converseModel) outputLocked() string {
	out := map[string]any{"answer": "", "forge": m.forge, "skipped_reason": "", "summary": "Question: why does A return 2 now?"}
	if strings.Contains(m.seen.results["diff"], "return 2") {
		out["answer"] = m.answer
		out["summary"] = "Answered from the diff (app.go:3)."
	} else {
		out["skipped_reason"] = "no shell on this route: the diff could not be read"
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// outputSchemaFields reads the output schema claw hands a tool-loop node:
// the JSON object its user message carries after "matching this schema:"
// (claw_backend.go, the OUTPUT FORMAT instruction). Nil when there is none.
func outputSchemaFields(user string) map[string]bool {
	const lead = "matching this schema:"
	i := strings.Index(user, lead)
	if i < 0 {
		return nil
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.NewDecoder(strings.NewReader(user[i+len(lead):])).Decode(&doc); err != nil {
		return nil
	}
	fields := map[string]bool{}
	for f := range doc.Properties {
		fields[f] = true
	}
	return fields
}

func (m *converseModel) observed() converseSeen {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.seen
	s.offered = slices.Clone(m.seen.offered)
	s.results = make(map[string]string, len(m.seen.results))
	for k, v := range m.seen.results {
		s.results[k] = v
	}
	return s
}

// TestReviConverseStepLimitedDraftIsStillPublished drives the VAO !48 path on
// claw end to end: the scripted agent keeps reading until claw stops its loop
// at the node's own tool_max_steps, the schema-forced recovery pass — the
// turn claw adds after the limit, holding its "This is your FINAL turn"
// reminder — is the only turn that answers, and it fills skipped_reason with
// filler beside its draft, as a step-limited model does. The draft it hands
// back then runs through the bundle's real publish_reply on the same
// executor, under a guard built from the bundle's forge_token declaration:
// the fake forge receives exactly one POST carrying that draft and the run
// link, and the node reports the posted note. Before the deterministic node,
// this path ended with the answer drafted and never posted.
func TestReviConverseStepLimitedDraftIsStillPublished(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"bash", "git", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
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
	agent := wf.Nodes["converse_agent"].(*ir.AgentNode)
	publish, ok := wf.Nodes["publish_reply"].(*ir.ToolNode)
	if !ok {
		t.Fatalf("publish_reply is %T, want *ir.ToolNode", wf.Nodes["publish_reply"])
	}

	home := t.TempDir()
	ws := converseWorkspace(t, home)
	forge := newAcceptingForge(t)
	prURL := forge.srv.URL + gitlabThread.prPath
	answer := "Re: op — « why does A return 2 now? »\n\n`A` returns 2 (app.go:3); the reads stopped at the step limit."
	mdl := &stepHungryModel{ws: ws, answer: answer}
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mdl, nil })
	tools := tool.NewRegistry()
	if err := tool.RegisterClawBuiltinsWithEnv(tools, ws, []string{"HOME=" + home, "TMPDIR=" + t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := tool.RegisterClawTodo(tools); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "forge_token")
	const token = "forge-token-step-limit-91ab"
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	decl := wf.Secrets["forge_token"]
	guard := secretguard.New([]secretguard.Secret{{
		Name: "forge_token", Value: token, Placeholder: secretguard.PlaceholderForName("forge_token"),
		FilePath: tokenFile, Env: decl.Env, Sinks: append([]string(nil), decl.Sinks...),
	}}, secretguard.DefaultConfig())
	var onClaw ModelOverrides
	onClaw.SetBackend(agent.ID, delegate.BackendClaw)
	onClaw.SetModel(agent.ID, "anthropic/claude-opus-5-5")
	onClaw.SetProvider(agent.ID, "auto")
	e := newTestClawExecutor(reg, wf, WithWorkDir(ws), WithToolRegistry(tools), WithSecretGuard(guard),
		WithModelOverrides(onClaw), WithLogger(iterlog.Nop()))
	e.SetVars(map[string]any{
		"workspace_dir": ws, "base_ref": "main", "pr_url": prURL, "discussion_id": gitlabThread.discussion,
		"converse_question": "why does A return 2 now?", "trigger_note": "/revi why does A return 2 now?",
		"thread_context": "", "replier": "op", "forge_base_url": "",
	})
	const runID = "run-step-limit"
	runURL := "https://iterion.example/studio/runs/" + runID
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctx = WithRunID(ctx, runID)
	ctx = WithTemplateData(ctx, &TemplateData{RunID: runID, Run: map[string]any{"id": runID, "url": runURL}})

	out, err := e.Execute(ctx, agent, map[string]any{})
	steps, recovered := mdl.observed()
	if err != nil {
		t.Fatalf("converse_agent on claw: %v (after %d tool steps, recovery reached %v, last requests %v)", err, steps, recovered, mdl.lastRequests(3))
	}
	if steps != agent.ToolMaxSteps {
		t.Fatalf("the loop ran %d tool steps, want the node's whole bound %d — the step limit was not what ended it", steps, agent.ToolMaxSteps)
	}
	if !recovered {
		t.Fatal("no request carried claw's FINAL-turn reminder: the answer did not come from the recovery pass")
	}
	if out["answer"] != answer {
		t.Fatalf("the recovery's draft = %q, want %q", out["answer"], answer)
	}

	// The publish step reaches the fake forge on loopback directly: its
	// environment is the process's without a proxy, carried on the context
	// (the env seam) rather than set on the process.
	env := carriedEnv{"NO_PROXY": "127.0.0.1,localhost", "no_proxy": "127.0.0.1,localhost"}
	for _, kv := range os.Environ() {
		name, val, _ := strings.Cut(kv, "=")
		if !strings.HasSuffix(strings.ToLower(name), "_proxy") {
			env[name] = val
		}
	}
	pub, err := e.Execute(withCarriedEnv(ctx, env), publish, map[string]any{"answer": out["answer"], "skipped_reason": out["skipped_reason"]})
	if err != nil {
		t.Fatalf("publish_reply: %v", err)
	}
	posts := forge.posts()
	if len(posts) != 1 {
		t.Fatalf("the forge received %d POSTs, want exactly the one reply (publish_reply said %v)", len(posts), pub)
	}
	wantBody := answer + "\n\n---\n🤖 [iterion](https://iterion.cloud) · run [`" + runID + "`](" + runURL + ")"
	if posts[0] != wantBody {
		t.Errorf("the forge received\n%q\nwant the recovery's draft and the run link\n%q", posts[0], wantBody)
	}
	if pub["posted"] != true || pub["reply_url"] != prURL+"#note_9001" {
		t.Errorf("publish_reply = posted %v, reply_url %v; want the note it created", pub["posted"], pub["reply_url"])
	}
}

// stepHungryModel never stops reading on its own: every turn that offers
// tools gets one more read, so only claw's step bound ends the loop. The
// turn that carries claw's FINAL-turn reminder answers, with filler in
// skipped_reason beside the draft.
type stepHungryModel struct {
	ws     string
	answer string

	mu        sync.Mutex
	steps     int
	recovered bool
	requests  []string // what each request offered, for a failure's message
}

func (m *stepHungryModel) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	final := false
	for _, msg := range req.Messages {
		for _, block := range msg.Content {
			if strings.Contains(block.Text, "This is your FINAL turn") {
				final = true
			}
		}
	}
	var names []string
	for _, tl := range req.Tools {
		names = append(names, tl.Name)
	}
	m.requests = append(m.requests, fmt.Sprintf("final=%v tools=%d msgs=%d", final, len(names), len(req.Messages)))
	output := func() string {
		b, _ := json.Marshal(map[string]any{"answer": m.answer, "forge": "gitlab", "skipped_reason": "N/A", "summary": "Stopped at the step limit."})
		return string(b)
	}
	var evs []api.StreamEvent
	switch {
	case final && slices.Contains(names, "structured_output"):
		m.recovered = true
		evs = toolUseEvents("converse_output", "structured_output", output(), 100, 20)
	case final:
		m.recovered = true
		evs = textEvents(output(), 100, 20)
	case slices.Contains(names, "bash"):
		in, _ := json.Marshal(map[string]any{"command": "git -C " + shellquote.Quote(m.ws) + " log --oneline -1"})
		evs = toolUseEvents(fmt.Sprintf("read_%d", m.steps), "bash", string(in), 100, 20)
		m.steps++
	default:
		evs = textEvents("still reading", 100, 20)
	}
	ch := make(chan api.StreamEvent, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (m *stepHungryModel) lastRequests(n int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests[max(0, len(m.requests)-n):])
}

func (m *stepHungryModel) observed() (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.steps, m.recovered
}

// acceptingForge is a GitLab discussion that accepts a reply: the POST
// creates note 9001, a read by id returns it, the thread read lists what it
// holds.
type acceptingForge struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies []string
}

func newAcceptingForge(t *testing.T) *acceptingForge {
	f := &acceptingForge{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		note := func(i int) map[string]any { return map[string]any{"id": 9001 + i, "body": f.bodies[i]} }
		switch {
		case r.Method == http.MethodPost:
			var p struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(raw, &p)
			f.bodies = append(f.bodies, p.Body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(note(len(f.bodies) - 1))
		case strings.HasSuffix(r.URL.Path, "/notes/9001") && len(f.bodies) > 0:
			_ = json.NewEncoder(w).Encode(note(0))
		case strings.Contains(r.URL.Path, "/discussions/"):
			notes := []map[string]any{}
			for i := range f.bodies {
				notes = append(notes, note(i))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "thread", "notes": notes})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *acceptingForge) posts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.bodies)
}
