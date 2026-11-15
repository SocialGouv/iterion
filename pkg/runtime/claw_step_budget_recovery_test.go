package runtime_test

import (
	"context"
	"encoding/json"
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
