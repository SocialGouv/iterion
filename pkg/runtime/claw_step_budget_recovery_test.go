package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
// the pause — and a retry after the resume (a stream cut the claw backend
// retries, a 502 the dispatch retries in place) is still charged those. A
// failure the resumed run's own retries do not absorb fails the run: the
// engine does not re-run a resume's re-invocation.
// The agent spends bound−1 turns, asks the operator, is answered, then
// works on: every arm runs the whole bound after the pause, no more.
//
// Mutants: keep the anchor across the pause (the resumed run is charged the
// turns before it and has none left); start the dispatch's retry after the
// resume from nothing (it runs the whole bound again on top of the turns it
// replays).
func TestClawStepBudget_AResumeAfterAHumanPauseGetsTheWholeBound(t *testing.T) {
	t.Parallel()
	const bound = 4
	for _, arm := range []struct {
		name          string
		clawTries     int
		dispatchTries int
		failAt        map[int]bool // failing requests, counted after the resume
		cut           bool         // a stream cut, else a 502
	}{
		{name: "resumed", clawTries: 1, dispatchTries: 1},
		{name: "a stream cut after the resume", clawTries: 2, dispatchTries: 1, failAt: map[int]bool{3: true}, cut: true},
		{name: "a dispatch retry after the resume", clawTries: 1, dispatchTries: 2, failAt: map[int]bool{3: true}},
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
workflow w:
  entry: judge
  worktree: none
  judge -> done
`, bound))
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
			if after != bound {
				t.Fatalf("%d tool turns ran after the pause, want the whole bound %d", after, bound)
			}
		})
	}
}
