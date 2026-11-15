package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// recoveryClient answers each request from fn by its 1-based rank.
type recoveryClient struct {
	mu    sync.Mutex
	fn    func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error)
	calls int
}

func (c *recoveryClient) StreamResponse(_ context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	evs, err := c.fn(n, req)
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

// The engine's recovery re-runs a failed claw node through a fresh dispatch
// of the executor, on the session the executor keeps on error: that re-run
// replays the failed execution's turns and is a retry of the same node
// execution (#3283). The node completes 3 of its 4 turns, then two 502s
// spend the claw backend's and the dispatch's attempts; the production
// recipes re-run the node (NETWORK_TRANSIENT, RetrySameNode), which is
// charged the 3 turns it replays and left the 1 the bound allows.
//
// Mutant: start each dispatch's anchor carry empty instead of from the
// anchor kept beside the session — the re-run fixes its anchor from its own
// replay and runs the whole bound again: 7 for 4.
func TestClawStepBudget_AnEngineRecoveryRerunIsChargedWhatItReplays(t *testing.T) {
	t.Parallel()
	const bound = 4
	wf := compileGuaranteeBot(t, `schema out:
  verdict: string
prompt p:
  Judge the diff.
agent judge:
  backend: claw
  model: "test/test-model"
  system: p
  user: p
  output: out
  tools: [read_file]
  tool_max_steps: 4
workflow w:
  entry: judge
  worktree: none
  judge -> done
`)
	var ran atomic.Int64
	tr := tool.NewRegistry()
	if err := tr.RegisterBuiltin("read_file", "read the workspace", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (string, error) { ran.Add(1); return "observed", nil }); err != nil {
		t.Fatal(err)
	}
	if err := tool.RegisterClawTodo(tr); err != nil {
		t.Fatal(err)
	}
	offers := func(req api.CreateMessageRequest) bool {
		return slices.ContainsFunc(req.Tools, func(t api.Tool) bool { return t.Name == "read_file" })
	}
	client := &recoveryClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
		switch {
		case n == 4 || n == 5:
			return nil, &api.APIError{Provider: "test", StatusCode: 502, Message: "bad gateway", Retryable: true}
		case offers(req):
			return guaranteeEvents(probeCall(fmt.Sprintf("tu_%d", n))), nil
		}
		return guaranteeEvents(api.ContentBlock{Type: "tool_use", ID: "so_1", Name: "structured_output", Input: map[string]any{"verdict": "done"}}), nil
	}}
	reg := model.NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
	br := delegate.NewRegistry()
	br.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	workspace := t.TempDir()
	cexec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(br), model.WithToolRegistry(tr),
		model.WithLogger(iterlog.Nop()), model.WithWorkDir(workspace),
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond}))
	t.Cleanup(func() { _ = cexec.Close() })
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	recipes := recovery.Dispatch(recovery.DefaultRecipes())
	var decisions []runtime.ErrorCode
	dispatch := func(ctx context.Context, err error, prior func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
		action, code := recipes(ctx, err, prior)
		decisions = append(decisions, code)
		action.Delay = 0
		return action, code
	}
	runErr := runtime.New(wf, st, cexec, runtime.WithWorkDir(workspace), runtime.WithLogger(iterlog.Nop()),
		runtime.WithRecoveryDispatch(dispatch)).Run(t.Context(), "recovery-rerun", nil)
	if runErr != nil {
		t.Fatalf("the node must answer after the engine's re-run: %v", runErr)
	}
	if len(decisions) == 0 {
		t.Fatal("scenario broken: the engine's recovery never re-ran the node")
	}
	if got := ran.Load(); got != bound {
		t.Fatalf("%d tool turns ran across the engine's re-run (recovery %v), want tool_max_steps %d", got, decisions, bound)
	}
}

// A pause for a human is a supervised checkpoint: the node execution
// resumed from it gets the whole bound again, charged only the turns after
// the pause. A retry of the resumed run (a stream cut the claw backend
// retries, a 502 the dispatch retries in place) replays the conversation it
// resumed with alone: it is charged nothing and runs the whole bound again.
// A failure the resumed run's own retries do not absorb fails the run: the
// engine does not re-run a resume's re-invocation.
// The agent spends bound−1 turns, asks the operator, is answered, then
// works on: the resumed run runs the whole bound after the pause, and one
// retried 2 turns in runs those 2 and the whole bound — a node whose
// session persists in a slot of another name as well.
//
// Mutants: keep the anchor across the pause (the resumed run is charged the
// turns before it and has none left); drop it under the node's id rather
// than its session key (the slot's anchor survives the pause).
func TestClawStepBudget_AResumeAfterAHumanPauseGetsTheWholeBound(t *testing.T) {
	t.Parallel()
	const bound = 4
	for _, arm := range []struct {
		name          string
		clawTries     int
		dispatchTries int
		failAt        map[int]bool // failing requests, counted after the resume
		cut           bool         // a stream cut, else a 502
		session       string       // the agent's session lines
		after         int          // the tool turns that run after the pause
	}{
		{name: "resumed", clawTries: 1, dispatchTries: 1, after: bound},
		{name: "a stream cut after the resume", clawTries: 2, dispatchTries: 1, failAt: map[int]bool{3: true}, cut: true, after: 2 + bound},
		{name: "a dispatch retry after the resume", clawTries: 1, dispatchTries: 2, failAt: map[int]bool{3: true}, after: 2 + bound},
		{name: "resumed, session persisted in a slot", clawTries: 1, dispatchTries: 1, session: "  session: persist\n  session_slot: conversation\n", after: bound},
		{name: "a dispatch retry after the resume, session persisted in a slot", clawTries: 1, dispatchTries: 2, failAt: map[int]bool{3: true}, session: "  session: persist\n  session_slot: conversation\n", after: 2 + bound},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			wf := compileGuaranteeBot(t, fmt.Sprintf(`schema out:
  verdict: string
prompt p:
  Judge the diff.
agent judge:
  backend: claw
  model: "test/test-model"
  system: p
  user: p
  output: out
  tools: [read_file]
  tool_max_steps: %d
  interaction: human
%sworkflow w:
  entry: judge
  worktree: none
  judge -> done
`, bound, arm.session))
			var mu sync.Mutex
			paused, before, after, resumedRequests := false, 0, 0, 0
			tr := tool.NewRegistry()
			if err := tr.RegisterBuiltin("read_file", "read the workspace", json.RawMessage(`{"type":"object"}`),
				func(context.Context, json.RawMessage) (string, error) {
					mu.Lock()
					defer mu.Unlock()
					if paused {
						after++
					} else {
						before++
					}
					return "observed", nil
				}); err != nil {
				t.Fatal(err)
			}
			if err := tool.RegisterClawTodo(tr); err != nil {
				t.Fatal(err)
			}
			if err := tool.RegisterAskUser(tr, nil); err != nil {
				t.Fatal(err)
			}
			client := &recoveryClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
				mu.Lock()
				defer mu.Unlock()
				if !slices.ContainsFunc(req.Tools, func(t api.Tool) bool { return t.Name == "read_file" }) {
					return guaranteeEvents(api.ContentBlock{Type: "tool_use", ID: "so_1", Name: "structured_output", Input: map[string]any{"verdict": "done"}}), nil
				}
				if !paused {
					if before < bound-1 {
						return guaranteeEvents(probeCall(fmt.Sprintf("tu_%d", n))), nil
					}
					paused = true
					return guaranteeEvents(api.ContentBlock{Type: "tool_use", ID: "ask_1", Name: "ask_user", Input: map[string]any{"question": "go on?"}}), nil
				}
				resumedRequests++
				if arm.failAt[resumedRequests] {
					if arm.cut {
						return []api.StreamEvent{{Type: api.EventMessageStart, InputTokens: 10}, {Type: api.EventError, ErrorMessage: "unexpected EOF"}}, nil
					}
					return nil, &api.APIError{Provider: "test", StatusCode: 502, Message: "bad gateway", Retryable: true}
				}
				return guaranteeEvents(probeCall(fmt.Sprintf("tu_%d", n))), nil
			}}
			reg := model.NewRegistry()
			reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
			br := delegate.NewRegistry()
			br.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: arm.clawTries, BackoffBase: time.Millisecond}))
			workspace := t.TempDir()
			cexec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(br), model.WithToolRegistry(tr),
				model.WithLogger(iterlog.Nop()), model.WithWorkDir(workspace),
				model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: arm.dispatchTries, BackoffBase: time.Millisecond}))
			t.Cleanup(func() { _ = cexec.Close() })
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			engine := func() *runtime.Engine {
				return runtime.New(wf, st, cexec, runtime.WithWorkDir(workspace), runtime.WithLogger(iterlog.Nop()))
			}
			runErr := engine().Run(t.Context(), "pause-resume", nil)
			if !errors.Is(runErr, runtime.ErrRunPaused) {
				t.Fatalf("the run must pause on the question: %v", runErr)
			}
			if runErr = engine().Resume(t.Context(), "pause-resume", map[string]any{delegate.AskUserQuestionKey: "yes"}); runErr != nil {
				t.Fatalf("the resumed node must answer: %v", runErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if before != bound-1 {
				t.Fatalf("scenario broken: %d tool turns before the pause, want %d", before, bound-1)
			}
			if after != arm.after {
				t.Fatalf("%d tool turns ran after the pause, want %d for a bound of %d", after, arm.after, bound)
			}
		})
	}
}

// A recovery pause — a failure the recipes hand to the operator (an
// authentication failure here) — ends the node execution: the node the
// operator's acknowledgement re-executes starts from no conversation with
// the whole bound, whether the same executor resumes it (warm) or a fresh
// one does (cold), and whether its session is its own or persisted in a
// slot of another name. The agent spends bound−1 turns, then its provider
// refuses the key.
//
// Mutants: leave the executor's state alone at a recovery pause (warm, the
// re-execution replays the dead turns and is charged them: 1 for 4); drop
// the budget alone (warm, it replays the dead turns); key the eviction on
// the node's id rather than its session key (the slot's state survives).
func TestClawStepBudget_ARecoveryPauseEndsTheNodeExecution(t *testing.T) {
	t.Parallel()
	const bound = 4
	for _, arm := range []struct {
		name    string
		warm    bool
		session string
	}{
		{name: "warm", warm: true},
		{name: "cold"},
		{name: "warm, session persisted in a slot", warm: true, session: "  session: persist\n  session_slot: conversation\n"},
		{name: "cold, session persisted in a slot", session: "  session: persist\n  session_slot: conversation\n"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			src := fmt.Sprintf(`schema out:
  verdict: string
prompt p:
  Judge the diff.
agent judge:
  backend: claw
  model: "test/test-model"
  system: p
  user: p
  output: out
  tools: [read_file]
  tool_max_steps: %d
%sworkflow w:
  entry: judge
  worktree: none
  judge -> done
`, bound, arm.session)
			wf := compileGuaranteeBot(t, src)
			var mu sync.Mutex
			resumed, before, after, firstAfterResults := false, 0, 0, -1
			client := &recoveryClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
				mu.Lock()
				defer mu.Unlock()
				if !slices.ContainsFunc(req.Tools, func(t api.Tool) bool { return t.Name == "read_file" }) {
					return guaranteeEvents(api.ContentBlock{Type: "tool_use", ID: "so_1", Name: "structured_output", Input: map[string]any{"verdict": "done"}}), nil
				}
				if !resumed {
					if before < bound-1 {
						return guaranteeEvents(probeCall(fmt.Sprintf("tu_%d", n))), nil
					}
					return nil, &api.APIError{Provider: "test", StatusCode: 401, Message: "invalid api key"}
				}
				if firstAfterResults < 0 {
					firstAfterResults = 0
					for _, m := range req.Messages {
						for _, b := range m.Content {
							if b.Type == "tool_result" {
								firstAfterResults++
							}
						}
					}
				}
				return guaranteeEvents(probeCall(fmt.Sprintf("tu_%d", n))), nil
			}}
			executor := func() *model.ClawExecutor {
				tr := tool.NewRegistry()
				if err := tr.RegisterBuiltin("read_file", "read the workspace", json.RawMessage(`{"type":"object"}`),
					func(context.Context, json.RawMessage) (string, error) {
						mu.Lock()
						defer mu.Unlock()
						if resumed {
							after++
						} else {
							before++
						}
						return "observed", nil
					}); err != nil {
					t.Fatal(err)
				}
				if err := tool.RegisterClawTodo(tr); err != nil {
					t.Fatal(err)
				}
				reg := model.NewRegistry()
				reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
				br := delegate.NewRegistry()
				br.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
				cexec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(br), model.WithToolRegistry(tr),
					model.WithLogger(iterlog.Nop()), model.WithWorkDir(t.TempDir()),
					model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
				t.Cleanup(func() { _ = cexec.Close() })
				return cexec
			}
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			recipes := recovery.Dispatch(recovery.DefaultRecipes())
			var paused []runtime.ErrorCode
			dispatch := func(ctx context.Context, err error, prior func(runtime.ErrorCode) int) (runtime.RecoveryAction, runtime.ErrorCode) {
				action, code := recipes(ctx, err, prior)
				if action.Kind == runtime.RecoveryPauseForHuman {
					paused = append(paused, code)
				}
				action.Delay = 0
				return action, code
			}
			workspace := t.TempDir()
			engine := func(cexec *model.ClawExecutor) *runtime.Engine {
				return runtime.New(wf, st, cexec, runtime.WithWorkDir(workspace), runtime.WithLogger(iterlog.Nop()), runtime.WithRecoveryDispatch(dispatch))
			}
			first := executor()
			if runErr := engine(first).Run(t.Context(), "recovery-pause", nil); !errors.Is(runErr, runtime.ErrRunPaused) || len(paused) != 1 {
				t.Fatalf("scenario broken: the refused key must pause the run for the operator (err %v, pauses %v)", runErr, paused)
			}
			mu.Lock()
			resumed = true
			mu.Unlock()
			again := first
			if !arm.warm {
				again = executor()
			}
			if runErr := engine(again).Resume(t.Context(), "recovery-pause", map[string]any{"acknowledge_recovery": "continue"}); runErr != nil {
				t.Fatalf("the re-executed node must answer: %v", runErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if before != bound-1 {
				t.Fatalf("scenario broken: %d tool turns before the pause, want %d", before, bound-1)
			}
			if firstAfterResults != 0 {
				t.Errorf("the re-execution's first request replays %d tool results of the dead execution, want none", firstAfterResults)
			}
			if after != bound {
				t.Errorf("%d tool turns ran after the pause, want the whole bound %d", after, bound)
			}
		})
	}
}

// A fan-out branch runs without the run's identity, so the executor keeps
// no session for a node inside it: a retry there replays nothing and gets
// the whole bound again — on the trunk, the same retry is charged the turns
// it replays. Each node runs 2 tool turns, its third request answers a 502,
// the dispatch retries in place, and the node calls its tool until its
// bound: 3 on the trunk, 2 + 3 in a branch.
//
// Mutant: hand a branch the run's identity, and its retry replays the dead
// attempt's turns: 3 in the branch too.
func TestClawStepBudget_ARetryInAFanOutBranchGetsTheWholeBoundAgain(t *testing.T) {
	t.Parallel()
	const bound = 3
	for _, arm := range []struct {
		name   string
		fanOut bool
		want   int
	}{
		{name: "trunk", want: bound},
		{name: "fan_out_all branch", fanOut: true, want: 2 + bound},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			executions, loopRequests := map[string]int{}, map[string]int{}
			tr := tool.NewRegistry()
			if err := tr.RegisterBuiltin("read_file", "read the workspace", json.RawMessage(`{"type":"object"}`),
				func(_ context.Context, input json.RawMessage) (string, error) {
					var in struct {
						ID string `json:"id"`
					}
					_ = json.Unmarshal(input, &in)
					node, _, _ := strings.Cut(strings.TrimPrefix(in.ID, "tu_"), "_")
					mu.Lock()
					defer mu.Unlock()
					executions[node]++
					return "observed " + in.ID, nil
				}); err != nil {
				t.Fatal(err)
			}
			if err := tool.RegisterClawTodo(tr); err != nil {
				t.Fatal(err)
			}
			client := &recoveryClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
				prompt := fmt.Sprint(req.System)
				for _, m := range req.Messages {
					for _, b := range m.Content {
						prompt += b.Text
					}
				}
				node := "collect"
				switch {
				case strings.Contains(prompt, "NODE-A"):
					node = "a"
				case strings.Contains(prompt, "NODE-B"):
					node = "b"
				}
				if !slices.ContainsFunc(req.Tools, func(t api.Tool) bool { return t.Name == "read_file" }) {
					return guaranteeEvents(api.ContentBlock{Type: "tool_use", ID: "so_" + node, Name: "structured_output", Input: map[string]any{"verdict": "done"}}), nil
				}
				mu.Lock()
				loopRequests[node]++
				k := loopRequests[node]
				mu.Unlock()
				if k == 3 {
					return nil, &api.APIError{Provider: "test", StatusCode: 502, Message: "bad gateway", Retryable: true}
				}
				return guaranteeEvents(probeCall(fmt.Sprintf("tu_%s_%d", node, n))), nil
			}}
			src := fmt.Sprintf(`schema out:
  verdict: string
prompt pa:
  NODE-A judge the diff.
prompt pb:
  NODE-B judge the diff.
prompt pc:
  Collect the verdicts.
agent a:
  backend: claw
  model: "test/test-model"
  system: pa
  user: pa
  output: out
  tools: [read_file]
  tool_max_steps: %d
agent b:
  backend: claw
  model: "test/test-model"
  system: pb
  user: pb
  output: out
  tools: [read_file]
  tool_max_steps: %d
`, bound, bound)
			if arm.fanOut {
				src += `router fan:
  mode: fan_out_all
agent collect:
  backend: claw
  model: "test/test-model"
  system: pc
  user: pc
  output: out
  await: wait_all
workflow w:
  entry: fan
  worktree: none
  fan -> a
  fan -> b
  a -> collect
  b -> collect
  collect -> done
`
			} else {
				src += `workflow w:
  entry: a
  worktree: none
  a -> b
  b -> done
`
			}
			wf := compileGuaranteeBot(t, src)
			reg := model.NewRegistry()
			reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
			br := delegate.NewRegistry()
			br.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
			workspace := t.TempDir()
			cexec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(br), model.WithToolRegistry(tr),
				model.WithLogger(iterlog.Nop()), model.WithWorkDir(workspace),
				model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 2, BackoffBase: time.Millisecond}))
			t.Cleanup(func() { _ = cexec.Close() })
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if runErr := runtime.New(wf, st, cexec, runtime.WithWorkDir(workspace), runtime.WithLogger(iterlog.Nop())).Run(t.Context(), "fan", nil); runErr != nil {
				t.Fatalf("the run must finish: %v", runErr)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, node := range []string{"a", "b"} {
				if executions[node] != arm.want {
					t.Errorf("node %s ran %d tool executions for tool_max_steps %d, want %d", node, executions[node], bound, arm.want)
				}
			}
		})
	}
}
