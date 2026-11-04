package recovery

import (
	"context"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

func r2ZeroUsageMalformed() []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 0},
		{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "z", Name: "structured_output"}},
		{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"verdict":tru`}},
		{Type: api.EventContentBlockStop, Index: 0},
		{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{Reported: true, OutputTokens: 0}},
		{Type: api.EventMessageStop},
	}
}

// P11: the nil-map panic of a zero-usage claw parse failure, through the
// real Engine: is it recovered anywhere on the trunk?
func TestR2_ZeroUsagePanicThroughTheEngine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	reg := model.NewRegistry()
	mock := &rvaScriptClient{scripts: [][]api.StreamEvent{r2ZeroUsageMalformed(), r2ZeroUsageMalformed(), r2ZeroUsageMalformed(), r2ZeroUsageMalformed()}}
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	backends := delegate.NewRegistry()
	backends.Register(delegate.BackendClaw, model.NewClawBackend(reg, model.EventHooks{}, model.RetryPolicy{MaxAttempts: 1}))
	wf := rvaWorkflow(delegate.BackendClaw)
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exec := model.NewClawExecutor(reg, wf, model.WithBackendRegistry(backends), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
	eng := runtime.New(wf, st, exec, runtime.WithRecoveryDispatch(Dispatch(DefaultRecipes())))
	var runErr error
	p := func() (p any) {
		defer func() { p = recover() }()
		runErr = eng.Run(context.Background(), "zero", nil)
		return nil
	}()
	run, _ := st.LoadRun(context.Background(), "zero")
	status := ""
	if run != nil {
		status = string(run.Status)
	}
	if p != nil || run == nil || run.Status == store.RunStatusRunning || runErr == nil {
		t.Fatalf("run not terminal: panic=%v status=%q err=%v", p, status, runErr)
	}
	t.Logf("panic escaped eng.Run=%v | runErr=%v | stored run status=%q | model calls=%d", p, runErr, status, len(mock.calls))
}
