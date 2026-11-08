package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	iterruntime "github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// cleanCardInput is the card a legitimate planner files: no reserved key.
const cleanCardInput = `{"title":"PLANNED","state":"ready","bot":"feature-dev","bot_args":{"target_branch":"main"}}`

// hostCapStub is an Anthropic Messages endpoint for the host-capability bench.
// The agent turn is the first request that offers read_file (every node of the
// bench declares it) and carries no tool result yet: it is answered with ONE
// call to `call`, by name, whether or not that tool was offered — a fork's
// model can name a tool it was never handed, and the bench must see that go
// nowhere. Every other request gets a plain end_turn. The union of the offered
// tool names and every tool result the executor sent back are kept.
type hostCapStub struct {
	call  string
	input string

	mu      sync.Mutex
	offered map[string]bool
	results []string
}

func (s *hostCapStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(body, &req)
	agentTurn := false
	s.mu.Lock()
	if s.offered == nil {
		s.offered = map[string]bool{}
	}
	for _, tl := range req.Tools {
		s.offered[tl.Name] = true
		if tl.Name == "read_file" {
			agentTurn = true
		}
	}
	hasResult := strings.Contains(string(body), `"tool_result"`)
	if i := strings.Index(string(body), `"tool_result"`); i >= 0 {
		s.results = append(s.results, string(body)[i:])
	}
	s.mu.Unlock()
	first := agentTurn && !hasResult && s.call != ""

	w.Header().Set("Content-Type", "text/event-stream")
	ev := func(name, data string) { _, _ = io.WriteString(w, "event: "+name+"\ndata: "+data+"\n\n") }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-bench","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}`)
	if first {
		partial, _ := json.Marshal(s.input)
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_hostcap","name":"`+s.call+`","input":{}}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(partial)+`}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":10}}`)
	} else {
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`)
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`)
	}
	ev("message_stop", `{"type":"message_stop"}`)
}

// offeredWith returns the offered tool names containing any of the needles,
// sorted.
func (s *hostCapStub) offeredWith(needles ...string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for name := range s.offered {
		for _, n := range needles {
			if strings.Contains(name, n) {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// hostCapRun is what one engine run of the bench left behind.
type hostCapRun struct {
	dispatcherDir string
	events        []*store.Event
	err           error
}

// withdrawals returns the workspace_trust_withdrawal events of the run.
func (r hostCapRun) withdrawals() []*store.Event {
	var out []*store.Event
	for _, e := range r.events {
		if e.Type == store.EventWorkspaceTrustWithdrawal {
			out = append(out, e)
		}
	}
	return out
}

// cards opens the dispatcher store the run's board tools write — the directory
// `iterion dispatch` polls — and returns what landed there and how many of
// them the dispatcher's own entry point would launch.
func (r hostCapRun) cards(t *testing.T) (titles []string, launchable int) {
	t.Helper()
	ns, err := native.NewStore(r.dispatcherDir)
	if err != nil {
		t.Fatalf("open the dispatcher store %s: %v", r.dispatcherDir, err)
	}
	defer func() { _ = ns.Close() }()
	issues, err := ns.List(native.ListFilter{})
	if err != nil {
		t.Fatalf("list cards: %v", err)
	}
	for _, iss := range issues {
		titles = append(titles, iss.Title)
	}
	cands, err := native.NewAdapter(ns).ListCandidates(context.Background())
	if err != nil {
		t.Fatalf("dispatcher ListCandidates: %v", err)
	}
	return titles, len(cands)
}

// hostDriver is the sandbox the bench's fork rows run under: main's
// admission floor (#1773) refuses an untrusted run with no per-run sandbox,
// so the fork rows promise one — a driver that answers the probe with a
// container's name and runs its commands on the host. The shape is
// pkg/runtime's podDriver's, duplicated here because test helpers do not
// cross packages.
type hostDriver struct{}

func (hostDriver) Name() string { return "docker" }
func (hostDriver) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{SupportsImage: true, SupportsMounts: true}
}

type hostPrepared struct{}

func (hostPrepared) DriverName() string { return "docker" }

func (hostDriver) Prepare(context.Context, sandbox.Spec) (sandbox.PreparedSpec, error) {
	return hostPrepared{}, nil
}

func (hostDriver) Start(context.Context, sandbox.PreparedSpec, sandbox.RunInfo) (sandbox.Run, error) {
	return hostRun{}, nil
}

type hostRun struct{}

func (hostRun) Driver() string { return "docker" }
func (hostRun) Command(ctx context.Context, argv []string, opts sandbox.ExecOpts) *exec.Cmd {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(opts.Env) > 0 {
		env := os.Environ()
		for k, v := range opts.Env {
			env = append(env, k+"="+v)
		}
		c.Env = env
	}
	if opts.WorkDir != "" {
		c.Dir = opts.WorkDir
	}
	return c
}
func (hostRun) Exec(ctx context.Context, argv []string, opts sandbox.ExecOpts) (sandbox.ExecResult, error) {
	c := hostRun{}.Command(ctx, argv, opts)
	c.Stdin = opts.Stdin
	return sandbox.ExecCmd(c, opts)
}
func (hostRun) Cleanup(context.Context) error { return nil }

// runHostCapEngine drives ONE agent node through the product — runview's
// executor (claw registry, board store, MCP manager) under Engine.Run with the
// trust pushed by the engine seam — against the stub model.
func runHostCapEngine(t *testing.T, trust store.RunTrust, node *ir.AgentNode, wf *ir.Workflow, stub http.Handler) hostCapRun {
	t.Helper()
	srv := httptest.NewServer(stub)
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_API_KEY", "bench-key")
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ZAI_API_KEY", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
	}
	ws := t.TempDir()
	storeDir := t.TempDir()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	node.BaseNode = ir.BaseNode{ID: "a"}
	if node.Model == "" {
		node.LLMFields = ir.LLMFields{Model: "anthropic/claude-bench", UserPrompt: "p"}
	}
	if wf == nil {
		wf = &ir.Workflow{}
	}
	wf.Name, wf.Entry, wf.Worktree = "host-caps", "a", "none"
	wf.Nodes = map[string]ir.Node{"a": node, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}
	wf.Edges = []*ir.Edge{{From: "a", To: "done"}}
	wf.Prompts = map[string]*ir.Prompt{"p": {Name: "p", Body: "Review the diff."}}
	runID := "host-caps-" + strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	ex, err := runview.BuildExecutor(runview.ExecutorSpec{
		Workflow: wf, Store: st, RunID: runID, StoreDir: storeDir, WorkDir: ws, Logger: iterlog.Nop(),
		BotID: "e2e-host-caps",
		// The bench decides the sandbox per row below (none for the trusted
		// controls, the host-fake driver for the fork rows).
		SandboxTiersKnown: true,
	})
	if err != nil {
		t.Fatalf("BuildExecutor: %v", err)
	}
	defer func() { _ = ex.Close() }()
	opts := []iterruntime.EngineOption{
		iterruntime.WithWorkDir(ws),
		iterruntime.WithLogger(iterlog.Nop()),
	}
	if trust == store.RunTrustDefault {
		opts = append(opts, iterruntime.WithSandboxOverride("none"))
	} else {
		// The admission floor (#1773) runs a fork only inside a per-run
		// sandbox: the bench promises one (hostDriver) and puts the CURRENT
		// iterion on PATH, since the sandboxed claw child re-enters through
		// `iterion __claw-runner` resolved by name.
		wf.Sandbox = &ir.SandboxSpec{Mode: "auto"}
		t.Setenv("ITERION_MODE", "local")
		t.Setenv("PATH", filepath.Dir(iterionBinary(t))+string(os.PathListSeparator)+os.Getenv("PATH"))
		opts = append(opts,
			iterruntime.WithSandboxDrivers(map[string]sandbox.DriverConstructor{
				"docker": func() (sandbox.Driver, error) { return hostDriver{}, nil },
			}),
			iterruntime.WithTrust(trust, ""),
		)
	}
	eng := iterruntime.New(wf, st, ex, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runErr := eng.Run(ctx, runID, nil)
	events, _ := st.LoadEvents(context.Background(), runID)
	return hostCapRun{dispatcherDir: filepath.Join(storeDir, "dispatcher"), events: events, err: runErr}
}

// eventItems returns the items of a workspace_trust_withdrawal event.
func eventItems(e *store.Event) []string {
	raw, _ := json.Marshal(e.Data["items"])
	var items []string
	_ = json.Unmarshal(raw, &items)
	sort.Strings(items)
	return items
}

// trustLabel names a trust for a subtest.
func trustLabel(trust store.RunTrust) string {
	if trust == store.RunTrustDefault {
		return "trusted"
	}
	return string(trust)
}

// writeExecutable writes a stand-in binary.
func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestUntrustedWorkspace_ClawIsOfferedNoHostCapability.
//
// A host capability reaches the tenant's own state: a board card names the bot
// the dispatcher launches and the args it launches with, and the cloud board
// dispatcher launches it as a trusted, unpinned run. On a fork every capability
// is withdrawn at ONE resolver (store.RunTrust.HostCapabilities); this bench
// reads what claw's model was actually OFFERED, per family, on both trusts —
// the trusted rows are the control that the path is live.
//
// The rows without a capability are claw's own gap, closed by the same
// resolver: claw registers the whole iterion_board / iterion_watch /
// iterion_runs family in-process for the run, and a node naming one of its
// tools in `tools:` (by name, by wildcard, by the claude_code FQN) was handed
// it with no capability at all — on a trusted run and on a fork alike.
//
// Every row's model then CALLS create_issue, offered or not: the card must
// land only where the tool was offered.
func TestUntrustedWorkspace_ClawIsOfferedNoHostCapability(t *testing.T) {
	rows := []struct {
		name  string
		caps  []string
		tools []string
		// family is the substring of the claw tool names this row watches;
		// trustedOffered says whether the TRUSTED control must be offered them.
		family         string
		trustedOffered bool
		trustedCard    bool
	}{
		{"board, by its capabilities", []string{"board.create", "board.read"}, []string{"read_file"}, "mcp_iterion_board_", true, true},
		{"watch, by its capability", []string{"watch.subscribe"}, []string{"read_file", "mcp.iterion_watch.subscribe"}, "mcp_iterion_watch_", true, false},
		{"runs, by its capability", []string{"runs.read"}, []string{"read_file"}, "mcp_iterion_runs_", true, false},
		{"board, named in tools without a capability", nil, []string{"read_file", "mcp.iterion_board.create_issue"}, "mcp_iterion_board_", false, false},
		{"board, by wildcard without a capability", nil, []string{"read_file", "mcp.iterion_board.*"}, "mcp_iterion_board_", false, false},
		{"board, by the claude_code FQN without a capability", nil, []string{"read_file", "mcp__iterion_board__create_issue"}, "mcp_iterion_board_", false, false},
	}
	for _, row := range rows {
		for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
			t.Run(row.name+"/"+trustLabel(trust), func(t *testing.T) {
				stub := &hostCapStub{call: "mcp_iterion_board_create_issue", input: cleanCardInput}
				run := runHostCapEngine(t, trust, &ir.AgentNode{Capabilities: row.caps, Tools: row.tools}, nil, stub)
				offered := stub.offeredWith(row.family)
				titles, launchable := run.cards(t)
				t.Logf("trust=%q run_err=%v offered=%v cards=%v launchable=%d", trust, run.err, offered, titles, launchable)

				wantOffered := row.trustedOffered && trust == store.RunTrustDefault
				if wantOffered && len(offered) == 0 {
					t.Fatalf("the TRUSTED control was offered no %s tool — this row proves nothing", row.family)
				}
				if !wantOffered && len(offered) > 0 {
					t.Errorf("offered %v (trust=%q, capabilities=%v)", offered, trust, row.caps)
				}
				wantCard := row.trustedCard && trust == store.RunTrustDefault
				if wantCard && (len(titles) != 1 || launchable != 1) {
					t.Fatalf("the TRUSTED control's card did not land (cards=%v launchable=%d) — the no-card rows prove nothing", titles, launchable)
				}
				if !wantCard && len(titles) > 0 {
					t.Errorf("a card landed in the dispatcher store: %v (launchable %d)", titles, launchable)
				}

				// The withdrawal is said once, on the node, naming what it took.
				w := run.withdrawals()
				switch {
				case trust == store.RunTrustFork && len(row.caps) > 0:
					if len(w) != 1 || w[0].NodeID != "a" || w[0].Data["withdrawn"] != "capabilities" {
						t.Fatalf("want ONE capabilities withdrawal on node a, got %d: %+v", len(w), w)
					}
					want := append([]string(nil), row.caps...)
					sort.Strings(want)
					if got := eventItems(w[0]); strings.Join(got, ",") != strings.Join(want, ",") {
						t.Errorf("withdrawal names %v, want %v", got, want)
					}
				default:
					if len(w) != 0 {
						t.Errorf("a withdrawal was announced where nothing was withdrawn: %+v", w)
					}
				}
			})
		}
	}
}

// TestClaw_ABoardCapabilityNodeRunsBesideAnAmbientServer.
//
// claw's per-node MCP gate asked "is this tool's server active on the node" of
// iterion's own board server too — which no `mcp:` filter ever lists — so a
// board-capability node FAILED at tool resolution the moment any ambient
// server (a plugin's, the repo's .mcp.json) was active. The host tools are now
// admitted by the capability alone.
func TestClaw_ABoardCapabilityNodeRunsBesideAnAmbientServer(t *testing.T) {
	for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
		t.Run(trustLabel(trust), func(t *testing.T) {
			stub := &hostCapStub{call: "mcp_iterion_board_create_issue", input: cleanCardInput}
			// A catalog the way PrepareWorkflow leaves it: resolved FOR this
			// run's trust, which the engine checks before any node runs.
			wf := &ir.Workflow{
				ActiveMCPServers:   []string{"other"},
				ResolvedMCPServers: map[string]*ir.MCPServer{"other": {Name: "other", Transport: ir.MCPTransportStdio, Command: "true"}},
				MCPPrepared:        &ir.MCPPreparation{Trust: string(trust)},
			}
			node := &ir.AgentNode{Capabilities: []string{"board.create"}, Tools: []string{"read_file"}, ActiveMCPServers: []string{"other"}}
			run := runHostCapEngine(t, trust, node, wf, stub)
			titles, _ := run.cards(t)
			offered := stub.offeredWith("mcp_iterion_board_")
			t.Logf("trust=%q run_err=%v offered=%v cards=%v", trust, run.err, offered, titles)
			if run.err != nil {
				t.Fatalf("the node failed beside an ambient server: %v", run.err)
			}
			if trust == store.RunTrustDefault && (len(offered) == 0 || len(titles) != 1) {
				t.Fatalf("trusted: offered %v, cards %v — the board tools must reach a node that holds the capability", offered, titles)
			}
			if trust == store.RunTrustFork && (len(offered) > 0 || len(titles) > 0) {
				t.Errorf("fork: offered %v, cards %v", offered, titles)
			}
		})
	}
}

// TestUntrustedWorkspace_CLIBackendsAreHandedNoHostCapabilityServer.
//
// The same floor, read on the two CLI transports by what was actually SPAWNED:
// claude_code gets its MCP servers as a FILE on argv (`--mcp-config <path>`), pi
// its bridge list in the file ITERION_PI_MCP_SERVERS_FILE names. Both register iterion's board
// (`__mcp-board`) and runs (`__mcp-runs`) servers for a node whose task carries
// the capability — and the task is built at the executor's one resolver, so a
// fork's spawn names neither while the trusted control names both.
//
// Stand-in `claude`/`pi` binaries on PATH record and exit; ITERION_BIN points
// the product at a stand-in iterion so the stdio servers have a command to
// name. The runs themselves then fail on the empty CLI output; what they were
// handed is the measurement.
func TestUntrustedWorkspace_CLIBackendsAreHandedNoHostCapabilityServer(t *testing.T) {
	binDir, rec := t.TempDir(), t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "claude"), "#!/bin/sh\n{ prev=\"\"; for a in \"$@\"; do if [ \"$prev\" = \"--mcp-config\" ] && [ -f \"$a\" ]; then tr '\\n' ' ' < \"$a\"; printf '\\n'; else printf '%s\\n' \"$a\"; fi; prev=\"$a\"; done; } > '"+rec+"/claude_code'\nexit 0\n")
	writeExecutable(t, filepath.Join(binDir, "pi"), "#!/bin/sh\n{ printf 'SPAWNED\\n'; if [ -n \"$ITERION_PI_MCP_SERVERS_FILE\" ]; then cat \"$ITERION_PI_MCP_SERVERS_FILE\"; else printf '%s' \"$ITERION_PI_MCP_SERVERS\"; fi; } > '"+rec+"/pi'\nexit 0\n")
	iterionBin := filepath.Join(t.TempDir(), "iterion")
	writeExecutable(t, iterionBin, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ITERION_BIN", iterionBin)

	for _, backend := range []string{"claude_code", "pi"} {
		for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
			t.Run(backend+"/"+trustLabel(trust), func(t *testing.T) {
				_ = os.Remove(filepath.Join(rec, backend))
				node := &ir.AgentNode{Capabilities: []string{"board.create", "runs.read"}, Tools: []string{"read_file"}}
				node.LLMFields = ir.LLMFields{Model: "claude-sonnet-4-5", Backend: backend, UserPrompt: "p"}
				run := runHostCapEngine(t, trust, node, nil, &hostCapStub{})
				raw, err := os.ReadFile(filepath.Join(rec, backend))
				if err != nil || len(raw) == 0 {
					t.Fatalf("%s was never spawned (run_err=%v) — this row proves nothing", backend, run.err)
				}
				got := string(raw)
				mcp := ""
				if i := strings.Index(got, "--mcp-config\n"); i >= 0 {
					mcp = strings.SplitN(got[i+len("--mcp-config\n"):], "\n", 2)[0]
				} else if strings.HasPrefix(got, "SPAWNED\n") {
					mcp = strings.TrimPrefix(got, "SPAWNED\n")
				}
				mcp = readMaybeFile(mcp)
				t.Logf("spawned (%d bytes recorded); MCP servers handed: %s", len(got), truncateForLog(mcp, 400))
				// Only the MCP servers handed over count — a prompt may name the
				// board tools too, and it rides argv.
				hostServers := []string{"iterion_board", "__mcp-board", "iterion_runs", "__mcp-runs"}
				if trust == store.RunTrustDefault {
					for _, want := range hostServers {
						if !strings.Contains(mcp, want) {
							t.Fatalf("the TRUSTED control's spawn was not handed %q — this row proves nothing:\n%s", want, truncateForLog(got, 1500))
						}
					}
					return
				}
				for _, banned := range hostServers {
					if strings.Contains(mcp, banned) {
						t.Errorf("a fork's %s spawn was handed %q:\n%s", backend, banned, truncateForLog(mcp, 1500))
					}
				}
				if w := run.withdrawals(); len(w) != 1 || w[0].Data["withdrawn"] != "capabilities" {
					t.Errorf("want ONE capabilities withdrawal, got %+v", w)
				}
			})
		}
	}
}

// convergeClaude is a stand-in `claude` for the review-pr converge node: it
// appends each spawn's argv to $CONVERGE_ARGV — one argument per line, newlines
// inside an argument flattened, each spawn closed by an --END-SPAWN-- line — and
// answers the minimum stream-json a turn needs, with a structured output that
// satisfies the bot's own `emit_output` schema: a converge pass that found
// nothing and filed no card, which a fork's converge can still honestly produce.
const convergeClaude = `#!/bin/sh
{ prev=""; for a in "$@"; do if [ "$prev" = "--mcp-config" ] && [ -f "$a" ]; then tr '\n' ' ' < "$a"; printf '\n'; else printf '%s' "$a" | tr '\n' ' '; printf '\n'; fi; prev="$a"; done; printf -- '--END-SPAWN--\n'; } >> "$CONVERGE_ARGV"
case "$*" in *--input-format*) read -r _ ;; esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s1","model":"fake","tools":[],"mcp_servers":[]}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","num_turns":1,"duration_ms":1,"duration_api_ms":1,"session_id":"s1","structured_output":{"report_path":".review-pr/findings.md","total_findings":0,"issues_created":[],"failed_issues":[],"findings":[],"questions":"","ticket_conformance":""}}'
`

// TestReviewPR_ConvergeDegradesOnAFork.
//
// The lane's own bot: review-pr's `converge` declares
// [board.read, board.create, board.label] to file its findings as cards. On a
// fork it must DEGRADE, not fail — complete without those tools, and say what
// it ran without. Driven on its real route (claude_code, the bot's default)
// with the bot's own compiled nodes, prompts and schemas: tier_expand →
// merge_reviews → converge → done, through runview's executor under
// Engine.Run. A stand-in CLI answers; its argv is what the node was handed.
func TestReviewPR_ConvergeDegradesOnAFork(t *testing.T) {
	full := compileFixture(t, "review-pr/main.bot")
	conv, ok := full.Nodes["converge"].(*ir.AgentNode)
	if !ok {
		t.Fatalf("review-pr has no agent node converge")
	}
	declared := append([]string(nil), conv.Capabilities...)
	sort.Strings(declared)
	if strings.Join(declared, ",") != "board.create,board.label,board.read" {
		t.Fatalf("converge declares %v — this row is about the three board capabilities", conv.Capabilities)
	}

	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "claude"), convergeClaude)
	iterionBin := filepath.Join(t.TempDir(), "iterion")
	writeExecutable(t, iterionBin, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ITERION_BIN", iterionBin)
	t.Setenv("ITERION_VIBE_BACKEND_EMIT", "")

	for _, trust := range []store.RunTrust{store.RunTrustDefault, store.RunTrustFork} {
		t.Run(trustLabel(trust), func(t *testing.T) {
			argvLog := filepath.Join(t.TempDir(), "argv.log")
			t.Setenv("CONVERGE_ARGV", argvLog)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ITERION_HOME", t.TempDir())
			t.Setenv("ANTHROPIC_API_KEY", "bench-key")
			ws := t.TempDir()
			storeDir := t.TempDir()
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			wf := &ir.Workflow{
				Name:    "review-pr-converge",
				Entry:   "tier_expand",
				Nodes:   map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
				Edges:   []*ir.Edge{{From: "tier_expand", To: "merge_reviews"}, {From: "merge_reviews", To: "converge"}, {From: "converge", To: "done"}},
				Prompts: full.Prompts,
				Schemas: full.Schemas,
				Vars:    full.Vars,
				// The bot runs in its own worktree; this bench only needs the node.
				Worktree: "none",
			}
			for _, id := range []string{"tier_expand", "merge_reviews", "converge"} {
				wf.Nodes[id] = full.Nodes[id]
			}
			runID := "converge-" + trustLabel(trust)
			ex, err := runview.BuildExecutor(runview.ExecutorSpec{
				Workflow: wf, Store: st, RunID: runID, StoreDir: storeDir, WorkDir: ws, Logger: iterlog.Nop(),
				BotID: "e2e-host-caps",
				// Same per-row decision as runHostCapEngine's.
				SandboxTiersKnown: true,
			})
			if err != nil {
				t.Fatalf("BuildExecutor: %v", err)
			}
			defer func() { _ = ex.Close() }()
			opts := []iterruntime.EngineOption{iterruntime.WithWorkDir(ws), iterruntime.WithLogger(iterlog.Nop())}
			if trust == store.RunTrustDefault {
				opts = append(opts, iterruntime.WithSandboxOverride("none"))
			} else {
				// The admission floor (#1773) runs a fork only inside a
				// per-run sandbox: the bench promises one (hostDriver) and
				// puts the CURRENT iterion on PATH — the sandboxed claw
				// child re-enters through `iterion __claw-runner`.
				wf.Sandbox = &ir.SandboxSpec{Mode: "auto"}
				t.Setenv("ITERION_MODE", "local")
				t.Setenv("PATH", filepath.Dir(iterionBinary(t))+string(os.PathListSeparator)+os.Getenv("PATH"))
				opts = append(opts,
					iterruntime.WithSandboxDrivers(map[string]sandbox.DriverConstructor{
						"docker": func() (sandbox.Driver, error) { return hostDriver{}, nil },
					}),
					iterruntime.WithTrust(trust, ""),
				)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			runErr := iterruntime.New(wf, st, ex, opts...).Run(ctx, runID, map[string]any{"workspace_dir": ws})
			r, _ := st.LoadRun(context.Background(), runID)
			events, _ := st.LoadEvents(context.Background(), runID)
			finished := false
			var withdrawals []*store.Event
			for _, e := range events {
				if e.Type == store.EventNodeFinished && e.NodeID == "converge" {
					finished = true
				}
				if e.Type == store.EventWorkspaceTrustWithdrawal {
					withdrawals = append(withdrawals, e)
				}
			}
			raw, _ := os.ReadFile(argvLog)
			spawns := strings.Split(strings.TrimSuffix(string(raw), "--END-SPAWN--\n"), "--END-SPAWN--\n")
			// Only the MCP configuration the CLI was handed counts: the bot's
			// own prompt names the board tools, and it rides argv too.
			var mcpConfigs []string
			for _, sp := range spawns {
				args := strings.Split(sp, "\n")
				for i, a := range args {
					if a == "--mcp-config" && i+1 < len(args) {
						mcpConfigs = append(mcpConfigs, readMaybeFile(args[i+1]))
					}
				}
			}
			argv := strings.Join(mcpConfigs, "\n")
			status := store.RunStatus("")
			if r != nil {
				status = r.Status
			}
			t.Logf("run_err=%v status=%s converge_finished=%v withdrawals=%d spawns=%d mcp_configs=%s",
				runErr, status, finished, len(withdrawals), len(spawns), truncateForLog(argv, 300))

			if runErr != nil || status != store.RunStatusFinished || !finished {
				t.Fatalf("converge did not complete (run_err=%v status=%s finished=%v) — a fork must degrade, not fail", runErr, status, finished)
			}
			if len(raw) == 0 {
				t.Fatal("the stand-in CLI was never spawned — this row proves nothing")
			}
			if trust == store.RunTrustDefault {
				if !strings.Contains(argv, "iterion_board") || !strings.Contains(argv, "__mcp-board") {
					t.Fatalf("the TRUSTED control's converge was not handed the board server — this row proves nothing:\n%s", truncateForLog(argv, 1500))
				}
				if len(withdrawals) != 0 {
					t.Errorf("a trusted run announced a withdrawal: %+v", withdrawals)
				}
				return
			}
			if strings.Contains(argv, "iterion_board") || strings.Contains(argv, "__mcp-board") {
				t.Errorf("a fork's converge was handed the board server:\n%s", truncateForLog(argv, 1500))
			}
			if len(withdrawals) != 1 || withdrawals[0].NodeID != "converge" || withdrawals[0].Data["withdrawn"] != "capabilities" {
				t.Fatalf("want ONE capabilities withdrawal on converge, got %+v", withdrawals)
			}
			if got := strings.Join(eventItems(withdrawals[0]), ","); got != "board.create,board.label,board.read" {
				t.Errorf("the withdrawal names %s", got)
			}
		})
	}
}

// TestHostTools_AToolNodeHoldsWhatTheWorkflowGrants.
//
// The call-time half of the capability floor, where the declaration-time half
// cannot see: a TOOL node naming a host tool in its `command:` calls the claw
// registry directly — nothing offered it anything, so no filter on what a node
// is offered ever ran. The host tool checks the caller's grant when CALLED: a
// tool node holds what the workflow's `capabilities:` grant, through the same
// resolver as an agent. Three spellings of the same tool, three arms each:
// trusted without the grant is refused (the bypass the agents had), trusted
// with it files the card, a fork with it is refused — typed, and no card lands.
func TestHostTools_AToolNodeHoldsWhatTheWorkflowGrants(t *testing.T) {
	for _, spelling := range []string{"mcp.iterion_board.create_issue", "mcp__iterion_board__create_issue", "create_issue"} {
		for _, arm := range []struct {
			name  string
			trust store.RunTrust
			caps  []string
			card  bool
		}{
			{"trusted, no grant", store.RunTrustDefault, nil, false},
			{"trusted, granted", store.RunTrustDefault, []string{"board.create"}, true},
			{"fork, granted", store.RunTrustFork, []string{"board.create"}, false},
		} {
			t.Run(spelling+"/"+arm.name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("ITERION_HOME", t.TempDir())
				ws, storeDir := t.TempDir(), t.TempDir()
				st, err := store.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				wf := &ir.Workflow{
					Name: "tool-node-grant", Entry: "plant", Worktree: "none",
					Nodes: map[string]ir.Node{
						"plant": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "plant"}, Command: spelling},
						"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
					},
					Edges:        []*ir.Edge{{From: "plant", To: "done"}},
					Capabilities: arm.caps,
				}
				ex, err := runview.BuildExecutor(runview.ExecutorSpec{Workflow: wf, Store: st, RunID: "tn", StoreDir: storeDir, WorkDir: ws, Logger: iterlog.Nop(),
					BotID: "e2e-host-caps", SandboxTiersKnown: true})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = ex.Close() }()
				// The recovery dispatch every production host wires: the
				// refusal's code is decided there.
				opts := []iterruntime.EngineOption{iterruntime.WithWorkDir(ws), iterruntime.WithLogger(iterlog.Nop()),
					iterruntime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes()))}
				if arm.trust == store.RunTrustDefault {
					opts = append(opts, iterruntime.WithSandboxOverride("none"))
				} else {
					// The admission floor (#1773) runs a fork only inside a
					// per-run sandbox; a tool node never spawns a CLI, so the
					// host-fake driver alone answers the probe.
					wf.Sandbox = &ir.SandboxSpec{Mode: "auto"}
					t.Setenv("ITERION_MODE", "local")
					opts = append(opts,
						iterruntime.WithSandboxDrivers(map[string]sandbox.DriverConstructor{
							"docker": func() (sandbox.Driver, error) { return hostDriver{}, nil },
						}),
						iterruntime.WithTrust(arm.trust, ""),
					)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				runErr := iterruntime.New(wf, st, ex, opts...).Run(ctx, "tn", map[string]any{"title": "PLANTED", "state": "ready", "bot": "branch-improve-loop"})
				titles, launchable := hostCapRun{dispatcherDir: filepath.Join(storeDir, "dispatcher")}.cards(t)
				t.Logf("run_err=%v cards=%v launchable=%d", runErr, titles, launchable)
				if arm.card {
					if runErr != nil || len(titles) != 1 {
						t.Fatalf("the granted TRUSTED control did not file its card (run_err=%v cards=%v) — the refusals below prove nothing", runErr, titles)
					}
					return
				}
				if len(titles) != 0 {
					t.Errorf("a card landed: %v (launchable %d)", titles, launchable)
				}
				if runErr == nil || !strings.Contains(runErr.Error(), "host capability not held") {
					t.Errorf("the tool node was not refused by name: %v", runErr)
				}
				// What decided the refusal travels with it: the floor for a
				// declared capability on a fork, the program for an undeclared
				// one — and neither is retried (a permanent code).
				if withdrawn := errors.Is(runErr, tool.ErrHostCapabilityWithdrawn); withdrawn != (arm.trust != store.RunTrustDefault) {
					t.Errorf("withdrawn=%v on trust %q: %v", withdrawn, arm.trust, runErr)
				}
				var rerr *iterruntime.RuntimeError
				if !errors.As(runErr, &rerr) || rerr.Code != iterruntime.ErrCodeToolFailedPermanent {
					t.Errorf("failure code = %v, want %s", rerr, iterruntime.ErrCodeToolFailedPermanent)
				}
			})
		}
	}
}

// readMaybeFile returns the value an MCP configuration was handed as: the
// inline JSON when it is one, the file's contents when the backend hands a
// PATH (claude_code's --mcp-config <file>, pi's ITERION_PI_MCP_SERVERS_FILE).
func readMaybeFile(s string) string {
	if b, err := os.ReadFile(strings.TrimSpace(s)); err == nil {
		return string(b)
	}
	return s
}

// truncateForLog bounds a fixture dump in a failure message.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
