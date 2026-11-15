package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The guarantee of `tool_max_steps` on a claw node (#3283), on the real
// path: the engine with the production recovery recipes, the real executor
// and claw backend, in process and across a sandbox crossing whose runner is
// a real process running the claw backend the way the iterion runner does.
// Each seed draws a world from a PRNG — stream cuts at random requests,
// retryable 5xx answers, crossing deaths, results large enough to force
// compaction, a provider that names every response's calls call_0, call_1…,
// a pause on an operator question with or without a call beside it, then
// its resume, the session the node's own or persisted in a slot — and every
// run must hold:
//
//	(a) the tool executions of a node execution never exceed
//	    tool_max_steps, counted at the tool, not from requests — before the
//	    pause, and after it: a resume after a human pause gets the whole
//	    bound again, and so does each retry of the resumed conversation,
//	    which replays that conversation alone;
//	(a') a node execution the loop ended by no longer offering its tools —
//	    the request that answers is the recovery pass — ran exactly
//	    tool_max_steps: no retry, no pause starves it;
//	(b) the request that answers carries the result of every execution
//	    its conversation holds — the run's, but for the oldest a compaction
//	    folded and those a dead retry of a resumed conversation ran;
//	(b') after a resume, the first request and the one that answers carry
//	    the operator's answer, but where a compaction folded it;
//	(c) the node answers — unless the world's own failures outlast every
//	    retry budget (the claw backend's, the dispatch's, the engine's
//	    recovery recipes), which no bound decides.
//
// The runner process is this test binary, re-entered on
// TestClawStepBudgetGuaranteeRunner with ITERION_GUARANTEE_RUNNER naming the
// world's directory, where every counter and log of a world lives so both
// processes read one world.

const guaranteeRunnerEnv = "ITERION_GUARANTEE_RUNNER"

// guaranteePlan is a world, drawn from its seed.
type guaranteePlan struct {
	Seed          int64 `json:"seed"`
	Sandbox       bool  `json:"sandbox"`
	Bound         int   `json:"bound"`
	ClawTries     int   `json:"claw_tries"`
	DispatchTries int   `json:"dispatch_tries"`
	BigResults    bool  `json:"big_results"`
	Pause         bool  `json:"pause"`
	PauseSibling  bool  `json:"pause_sibling"`
	MaxFailures   int   `json:"max_failures"`
	ReuseIDs      bool  `json:"reuse_ids"`
	Slot          bool  `json:"slot"`
}

func drawGuaranteePlan(seed int64) guaranteePlan {
	r := rand.New(rand.NewSource(seed))
	p := guaranteePlan{
		Seed:          seed,
		Sandbox:       seed%2 == 1,
		Bound:         3 + r.Intn(4),
		ClawTries:     1 + r.Intn(3),
		DispatchTries: 1 + r.Intn(3),
		BigResults:    r.Intn(4) == 0,
		MaxFailures:   r.Intn(4),
	}
	if !p.Sandbox {
		p.Pause = r.Intn(3) == 0
		p.PauseSibling = p.Pause && r.Intn(2) == 0
	}
	p.ReuseIDs = r.Intn(3) == 0
	// A persisted slot's resume refuses a pending call whose id an earlier
	// call of the conversation bears (sanitizeClawPersistResume), before any
	// tool step: no world pairs the two.
	p.Slot = p.Pause && !p.ReuseIDs && r.Intn(2) == 0
	return p
}

// guaranteeAnswer is the operator's answer to the world's question.
const guaranteeAnswer = "yes, go on (the operator)"

// guaranteeWorld is the shared state of a world, on disk.
type guaranteeWorld struct {
	dir  string
	plan guaranteePlan
	mu   sync.Mutex
}

func (w *guaranteeWorld) path(name string) string { return filepath.Join(w.dir, name) }

func (w *guaranteeWorld) counter(name string) int {
	raw, _ := os.ReadFile(w.path(name))
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return n
}

func (w *guaranteeWorld) setCounter(name string, n int) {
	if err := os.WriteFile(w.path(name), []byte(strconv.Itoa(n)), 0o600); err != nil {
		panic(err)
	}
}

func (w *guaranteeWorld) appendLine(name, line string) {
	f, err := os.OpenFile(w.path(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		panic(err)
	}
}

func (w *guaranteeWorld) text(name string) (string, bool) {
	raw, err := os.ReadFile(w.path(name))
	return string(raw), err == nil
}

func (w *guaranteeWorld) setText(name, text string) {
	if err := os.WriteFile(w.path(name), []byte(text), 0o600); err != nil {
		panic(err)
	}
}

// execution names the node execution a tool call runs in: the pause
// segment, counted from the last resume, then the attempt within it — a
// retry of a resumed conversation replays that conversation alone, so it
// starts a node execution of its own.
func (w *guaranteeWorld) execution() string {
	return fmt.Sprintf("%d.%d", w.counter("segment"), w.counter("attempt"))
}

func (w *guaranteeWorld) lines(name string) []string {
	raw, _ := os.ReadFile(w.path(name))
	return strings.Fields(strings.TrimSpace(string(raw)))
}

// guaranteeRequest is one request the world's model answered.
type guaranteeRequest struct {
	N         int      `json:"n"`
	Execution string   `json:"execution"`
	Loop      bool     `json:"loop"`
	Outcome   string   `json:"outcome"`
	Results   []string `json:"results"`
	Answer    bool     `json:"answer"`
	Answered  bool     `json:"answered"`
}

// probe is the node's tool, read_file (a tool a sandboxed node runs in the
// container): its input names the call — an id of its own, whatever id the
// provider gave the call — it records the execution under the world's
// current node execution, and answers a result that names the call, large
// enough to force compaction when the world says so.
func (w *guaranteeWorld) probe() delegate.ToolDef {
	return delegate.ToolDef{
		Name:        "read_file",
		Description: "read the workspace",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var in struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(input, &in)
			w.mu.Lock()
			defer w.mu.Unlock()
			w.appendLine("executions", w.execution()+":"+in.ID)
			if !w.plan.BigResults {
				return "observed " + in.ID, nil
			}
			r := rand.New(rand.NewSource(w.plan.Seed + int64(len(in.ID))*31))
			buf := make([]byte, 24000)
			r.Read(buf)
			return fmt.Sprintf("observed %s %x", in.ID, buf), nil
		},
	}
}

// guaranteeEvents is a streamed answer.
func guaranteeEvents(blocks ...api.ContentBlock) []api.StreamEvent {
	evs := []api.StreamEvent{{Type: api.EventMessageStart, InputTokens: 10}}
	stop := "end_turn"
	for i, b := range blocks {
		switch b.Type {
		case "text":
			evs = append(evs,
				api.StreamEvent{Type: api.EventContentBlockStart, Index: i, ContentBlock: api.ContentBlockInfo{Type: "text", Index: i}},
				api.StreamEvent{Type: api.EventContentBlockDelta, Index: i, Delta: api.Delta{Type: "text_delta", Text: b.Text}})
		case "tool_use":
			in, _ := json.Marshal(b.Input)
			evs = append(evs,
				api.StreamEvent{Type: api.EventContentBlockStart, Index: i, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: i, ID: b.ID, Name: b.Name}},
				api.StreamEvent{Type: api.EventContentBlockDelta, Index: i, Delta: api.Delta{Type: "input_json_delta", PartialJSON: string(in)}})
			stop = "tool_use"
		}
		evs = append(evs, api.StreamEvent{Type: api.EventContentBlockStop, Index: i})
	}
	return append(evs,
		api.StreamEvent{Type: api.EventMessageDelta, StopReason: stop, Usage: api.UsageDelta{Reported: true, OutputTokens: 5}},
		api.StreamEvent{Type: api.EventMessageStop})
}

func probeCall(id string) api.ContentBlock {
	return api.ContentBlock{Type: "tool_use", ID: id, Name: "read_file", Input: map[string]any{"id": id}}
}

// callID is the id the world's provider gives a response's call at
// position: the call's own name, or call_<position> when the provider
// reuses ids from one response to the next.
func (w *guaranteeWorld) callID(position int, own string) string {
	if w.plan.ReuseIDs {
		return fmt.Sprintf("call_%d", position)
	}
	return own
}

// carried reads what a request carries: the probe results, by the name each
// result gives its call, and whether the operator's answer is among them.
func carried(req api.CreateMessageRequest) (results []string, answer bool) {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type != "tool_result" {
				continue
			}
			var text strings.Builder
			for _, inner := range b.Content {
				text.WriteString(inner.Text)
			}
			if fields := strings.Fields(text.String()); len(fields) >= 2 && fields[0] == "observed" {
				results = append(results, fields[1])
			}
			answer = answer || strings.Contains(text.String(), guaranteeAnswer)
		}
	}
	return results, answer
}

// respond is the world's model: a function of the seed, the request's rank
// in the world and what the request carries.
func (w *guaranteeWorld) respond(req api.CreateMessageRequest) ([]api.StreamEvent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.counter("requests") + 1
	w.setCounter("requests", n)
	r := rand.New(rand.NewSource(w.plan.Seed*7919 + int64(n)))
	loop := slices.ContainsFunc(req.Tools, func(t api.Tool) bool { return t.Name == "read_file" })
	results, answer := carried(req)
	if w.counter("segment") > 0 && loop {
		// After a resume, a loop request that carries what the resumed
		// conversation's first one carried, and is not that one, is a
		// retry's: it replays the resumed conversation alone.
		key, name := strings.Join(results, " "), fmt.Sprintf("resumed-%d", w.counter("segment"))
		if first, ok := w.text(name); !ok {
			w.setText(name, key)
		} else if first == key {
			w.setCounter("attempt", w.counter("attempt")+1)
		}
	}
	record := guaranteeRequest{N: n, Execution: w.execution(), Loop: loop, Results: results, Answer: answer}
	defer func() {
		line, _ := json.Marshal(record)
		w.appendLine("requests.log", string(line))
	}()

	if failures := w.counter("failures"); failures < w.plan.MaxFailures && r.Float64() < 0.3 {
		w.setCounter("failures", failures+1)
		switch kind := r.Intn(3); {
		case kind == 2 && w.plan.Sandbox:
			record.Outcome = "die"
			line, _ := json.Marshal(record)
			w.appendLine("requests.log", string(line))
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		case kind == 1:
			record.Outcome = "5xx"
			return nil, &api.APIError{Provider: "test", StatusCode: 502, Message: "bad gateway", Retryable: true}
		default:
			record.Outcome = "cut"
			return []api.StreamEvent{{Type: api.EventMessageStart, InputTokens: 10}, {Type: api.EventError, ErrorMessage: "unexpected EOF"}}, nil
		}
	}
	id := fmt.Sprintf("tu_%d", n)
	call := func(position int) api.ContentBlock {
		c := probeCall(id)
		c.ID = w.callID(position, id)
		return c
	}
	switch {
	case !loop:
		record.Outcome, record.Answered = "verdict", true
		return guaranteeEvents(api.ContentBlock{Type: "tool_use", ID: "so_" + id, Name: "structured_output", Input: map[string]any{"verdict": "done"}}), nil
	case w.plan.Pause && w.counter("pauses") == 0 && len(results) >= 1 && r.Intn(2) == 0:
		w.setCounter("pauses", 1)
		record.Outcome = "pause"
		ask := api.ContentBlock{Type: "tool_use", ID: w.callID(0, "ask_"+id), Name: "ask_user", Input: map[string]any{"question": "go on?"}}
		if w.plan.PauseSibling {
			return guaranteeEvents(ask, call(1)), nil
		}
		return guaranteeEvents(ask), nil
	case len(results) >= 2 && r.Intn(7) == 0:
		record.Outcome, record.Answered = "answer", true
		return guaranteeEvents(api.ContentBlock{Type: "text", Text: `{"verdict":"done"}`}), nil
	}
	record.Outcome = "call"
	return guaranteeEvents(call(0)), nil
}

// StreamResponse makes the world an api.APIClient.
type guaranteeClient struct{ w *guaranteeWorld }

func (c guaranteeClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	evs, err := c.w.respond(req)
	if err != nil {
		return nil, err
	}
	ch := make(chan api.StreamEvent, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// guaranteeSandbox spawns the world's runner for a crossing and runs every
// other exec (the node session's sweep) as written.
type guaranteeSandbox struct{ dir string }

func (s guaranteeSandbox) Driver() string { return "guarantee" }

func (s guaranteeSandbox) Command(ctx context.Context, argv []string, _ sandbox.ExecOpts) *exec.Cmd {
	if n := len(argv); n >= 2 && argv[n-2] == "iterion" && argv[n-1] == "__claw-runner" {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClawStepBudgetGuaranteeRunner$", "-test.count=1")
		cmd.Env = append(os.Environ(), guaranteeRunnerEnv+"="+s.dir)
		return cmd
	}
	if len(argv) == 0 {
		return exec.CommandContext(ctx, "false")
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...)
}

func (s guaranteeSandbox) Exec(context.Context, []string, sandbox.ExecOpts) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}

func (s guaranteeSandbox) Cleanup(context.Context) error { return nil }

// sandboxedExecutor keeps the world's sandbox on the executor whatever the
// engine settles: the run declares none, the crossing is the world's.
type sandboxedExecutor struct {
	*model.ClawExecutor
	run sandbox.Run
}

func (s sandboxedExecutor) SetSandbox(sandbox.Run) { s.ClawExecutor.SetSandbox(s.run) }

// TestClawStepBudgetGuaranteeRunner is the world's sandbox runner when the
// test binary is re-entered for a crossing: it reads the launcher's
// envelopes, runs the claw backend on the task the way the iterion runner
// does — the replayed session in a runner-local store whose captures cross
// back, the step events relayed — and answers with the result envelope.
func TestClawStepBudgetGuaranteeRunner(t *testing.T) {
	dir := os.Getenv(guaranteeRunnerEnv)
	if dir == "" {
		t.Skip("the sandbox runner of TestClawStepBudgetGuarantee, re-entered for a crossing")
	}
	os.Exit(runGuaranteeRunner(dir))
}

func runGuaranteeRunner(dir string) int {
	var plan guaranteePlan
	raw, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil || json.Unmarshal(raw, &plan) != nil {
		return 3
	}
	w := &guaranteeWorld{dir: dir, plan: plan}
	reader := delegate.NewEnvelopeReader(os.Stdin)
	writer := delegate.NewEnvelopeWriter(os.Stdout)
	var writeMu sync.Mutex
	write := func(env delegate.Envelope) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writer.Write(env)
	}
	var ioTask delegate.IOTask
	var replays [][]byte
	for {
		env, err := reader.Read()
		if err != nil {
			return 4
		}
		if env.Type == delegate.EnvelopeTask {
			if json.Unmarshal(env.Data, &ioTask) != nil {
				return 5
			}
			break
		}
		if env.Type == delegate.EnvelopeSessionReplay {
			replays = append(replays, append([]byte(nil), env.Data...))
		}
	}
	task := delegate.FromIOTask(ioTask)
	task.Sandbox, task.Model = nil, "test/test-model"
	task.HasTools, task.ToolDefs = true, []delegate.ToolDef{w.probe()}
	sessions := model.NewNodeSessionStore()
	for _, snap := range replays {
		_ = sessions.SaveSnapshot("runner", model.TaskSessionKey(task), snap)
	}
	ctx := model.WithSandboxRunnerSession(context.Background(), "runner", sessions, guaranteeCaptureSink{write: write})
	if ioTask.LauncherEndsNodeSession {
		ctx = model.WithLauncherEndedNodeSession(ctx)
	}
	reg := model.NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return guaranteeClient{w: w}, nil })
	backend := model.NewClawBackend(reg, model.SandboxRelayHooks(write, nil), model.RetryPolicyFromWire(ioTask.Retry))
	res, runErr := backend.Execute(ctx, task)
	out := delegate.ToIOResult(res)
	if runErr != nil {
		out.Error = runErr.Error()
	}
	env, err := delegate.NewResultEnvelope(out)
	if err != nil || write(env) != nil {
		return 6
	}
	return 0
}

// guaranteeCaptureSink crosses a runner-local capture back to the launcher,
// bounded for the channel like the iterion runner's.
type guaranteeCaptureSink struct{ write func(delegate.Envelope) error }

func (s guaranteeCaptureSink) Capture(_, _ string, snapshot []byte) {
	if bounded, _, err := model.ConstrainSessionSnapshotForWire(snapshot); err == nil {
		_ = s.write(delegate.NewSessionCaptureEnvelope(bounded))
	}
}

func guaranteeSource(plan guaranteePlan) string {
	modelSpec, interaction := "test/test-model", ""
	if plan.Sandbox {
		modelSpec = "openai/gpt-5"
	}
	if plan.Pause {
		interaction = "  interaction: human\n"
	}
	if plan.Slot {
		interaction += "  session: persist\n  session_slot: conversation\n"
	}
	return fmt.Sprintf(`schema out:
  verdict: string
prompt p:
  Judge the diff.
agent judge:
  backend: claw
  model: %q
  system: p
  user: p
  output: out
  tools: [read_file]
  tool_max_steps: %d
%sworkflow w:
  entry: judge
  worktree: none
  judge -> done
`, modelSpec, plan.Bound, interaction)
}

// guaranteeOutcome is what one world's run showed.
type guaranteeOutcome struct {
	plan         guaranteePlan
	runErr       error
	perExecution map[string]int
	executed     []guaranteeExecution
	answering    *guaranteeRequest
	requests     []guaranteeRequest
}

// guaranteeExecution is one tool execution: the node execution it ran in
// and the call it ran.
type guaranteeExecution struct{ execution, call string }

func runGuaranteeWorld(t *testing.T, seed int64) guaranteeOutcome {
	t.Helper()
	plan := drawGuaranteePlan(seed)
	dir := t.TempDir()
	if raw, err := json.Marshal(plan); err != nil || os.WriteFile(filepath.Join(dir, "plan.json"), raw, 0o600) != nil {
		t.Fatalf("write the plan: %v", err)
	}
	w := &guaranteeWorld{dir: dir, plan: plan}
	wf := compileGuaranteeBot(t, guaranteeSource(plan))
	tr := tool.NewRegistry()
	probe := w.probe()
	if err := tr.RegisterBuiltin(probe.Name, probe.Description, probe.InputSchema, probe.Execute); err != nil {
		t.Fatal(err)
	}
	if err := tool.RegisterClawTodo(tr); err != nil {
		t.Fatal(err)
	}
	if err := tool.RegisterAskUser(tr, nil); err != nil {
		t.Fatal(err)
	}
	reg := model.NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return guaranteeClient{w: w}, nil })
	br := delegate.NewRegistry()
	br.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: plan.ClawTries, BackoffBase: time.Millisecond}))
	workspace := t.TempDir()
	cexec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(br), model.WithToolRegistry(tr),
		model.WithLogger(iterlog.Nop()), model.WithWorkDir(workspace),
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: plan.DispatchTries, BackoffBase: time.Millisecond}))
	t.Cleanup(func() { _ = cexec.Close() })
	var executor runtime.NodeExecutor = cexec
	if plan.Sandbox {
		box := guaranteeSandbox{dir: dir}
		cexec.SetSandbox(box)
		executor = sandboxedExecutor{ClawExecutor: cexec, run: box}
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The production recipes decide; only their wait is dropped.
	recipes := recovery.Dispatch(recovery.DefaultRecipes())
	dispatch := func(ctx context.Context, err error, prior func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
		action, code := recipes(ctx, err, prior)
		action.Delay = 0
		return action, code
	}
	engine := func() *runtime.Engine {
		return runtime.New(wf, st, executor, runtime.WithWorkDir(workspace), runtime.WithLogger(iterlog.Nop()), runtime.WithRecoveryDispatch(dispatch))
	}
	runID := fmt.Sprintf("guarantee-%d", seed)
	runErr := engine().Run(t.Context(), runID, nil)
	for resumes := 0; errors.Is(runErr, runtime.ErrRunPaused) && resumes < 3; resumes++ {
		w.setCounter("segment", w.counter("segment")+1)
		w.setCounter("attempt", 0)
		runErr = engine().Resume(t.Context(), runID, map[string]any{delegate.AskUserQuestionKey: guaranteeAnswer})
	}

	out := guaranteeOutcome{plan: plan, runErr: runErr, perExecution: map[string]int{}}
	for _, line := range w.lines("executions") {
		execution, id, _ := strings.Cut(line, ":")
		out.perExecution[execution]++
		out.executed = append(out.executed, guaranteeExecution{execution: execution, call: id})
	}
	for _, line := range strings.Split(strings.TrimSpace(readFileOr(w.path("requests.log"))), "\n") {
		var req guaranteeRequest
		if json.Unmarshal([]byte(line), &req) == nil {
			out.requests = append(out.requests, req)
		}
	}
	for i := range out.requests {
		if out.requests[i].Answered {
			out.answering = &out.requests[i]
		}
	}
	return out
}

// compileGuaranteeBot compiles a world's workflow.
func compileGuaranteeBot(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parsertest.Parse("guarantee.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("the world's workflow does not parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("the world's workflow does not compile: %s", d.Error())
		}
	}
	return cr.Workflow
}

func readFileOr(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

// violations is what a world's run breaks of (a), (a'), (b), (b') and (c).
func (o guaranteeOutcome) violations() []string {
	var v []string
	for execution, n := range o.perExecution {
		if n > o.plan.Bound {
			v = append(v, fmt.Sprintf("(a) node execution %s (pause segment.attempt) ran %d tool executions for tool_max_steps %d", execution, n, o.plan.Bound))
		}
	}
	if o.runErr != nil {
		if !o.exhausted() {
			v = append(v, fmt.Sprintf("(c) the node did not answer: %v", o.runErr))
		}
		return v
	}
	if o.answering == nil {
		return append(v, "(b) no request answered")
	}
	if n := o.perExecution[o.answering.Execution]; !o.answering.Loop && n != o.plan.Bound {
		v = append(v, fmt.Sprintf("(a') node execution %s ended its loop after %d tool executions for tool_max_steps %d", o.answering.Execution, n, o.plan.Bound))
	}
	// The conversation that answers holds the executions of the pause
	// segments before its own, and those of its own node execution.
	segmentOf := func(execution string) int {
		s, _, _ := strings.Cut(execution, ".")
		n, _ := strconv.Atoi(s)
		return n
	}
	segment := segmentOf(o.answering.Execution)
	var held []string
	for _, e := range o.executed {
		if segmentOf(e.execution) < segment || e.execution == o.answering.Execution {
			held = append(held, e.call)
		}
	}
	missing := 0
	for i, id := range held {
		if slices.Contains(o.answering.Results, id) {
			continue
		}
		folded := o.plan.BigResults && !slices.ContainsFunc(held[:i], func(earlier string) bool {
			return slices.Contains(o.answering.Results, earlier)
		})
		if !folded {
			missing++
		}
	}
	if missing > 0 {
		v = append(v, fmt.Sprintf("(b) the answering request %d misses %d of the %d results its conversation holds (it carries %v, the conversation executed %v)",
			o.answering.N, missing, len(held), o.answering.Results, held))
	}
	if segment > 0 && !o.plan.BigResults {
		first := slices.IndexFunc(o.requests, func(r guaranteeRequest) bool { return segmentOf(r.Execution) > 0 })
		if !o.answering.Answer || first < 0 || !o.requests[first].Answer {
			v = append(v, "(b') the operator's answer misses the first request after the resume or the one that answers")
		}
	}
	return v
}

// exhausted reports a run that failed on one of the world's own failures:
// the retry budgets — the claw backend's, the dispatch's, the engine's
// recovery recipes — ran out before the failures did, which no bound
// decides.
func (o guaranteeOutcome) exhausted() bool {
	msg := o.runErr.Error()
	return strings.Contains(msg, "bad gateway") || strings.Contains(msg, "unexpected EOF") || strings.Contains(msg, "signal: killed")
}

func (o guaranteeOutcome) trace() string {
	var b strings.Builder
	for _, r := range o.requests {
		kind := "R"
		if r.Loop {
			kind = "L"
		}
		fmt.Fprintf(&b, " %d%s@%s:%s/%d", r.N, kind, r.Execution, r.Outcome, len(r.Results))
	}
	return b.String()
}

// guaranteeSeeds is the table: 240 worlds, half of them sandboxed.
const guaranteeSeeds = 240

// guaranteeWorlds bounds the worlds that run at once: a sandboxed world
// starts a process per crossing.
var guaranteeWorlds = make(chan struct{}, 8)

// TestClawStepBudgetGuarantee runs every world of the table and reports
// each one that breaks (a), (b) or (c), with its plan and its requests.
func TestClawStepBudgetGuarantee(t *testing.T) {
	t.Parallel()
	for seed := int64(1); seed <= guaranteeSeeds; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			guaranteeWorlds <- struct{}{}
			defer func() { <-guaranteeWorlds }()
			out := runGuaranteeWorld(t, seed)
			t.Logf("plan %+v\n  executions per node execution %v, run err %v\n  requests%s", out.plan, out.perExecution, out.runErr, out.trace())
			if v := out.violations(); len(v) > 0 {
				t.Errorf("%s\n  plan %+v\n  requests%s", strings.Join(v, "\n  "), out.plan, out.trace())
			}
		})
	}
}
