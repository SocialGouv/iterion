package runtime

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// eventNumber reads a numeric event field back from the store, where JSON
// decoding has made every number a float64.
func eventNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return -1
}

// reaskBackend answers a schema-bearing node once WITHOUT a required field,
// then — asked again — with it. It names the session it ran, the shape a
// backend that resumes by id reports, and records every task it was handed.
type reaskBackend struct {
	tasks []delegate.Task
}

func (b *reaskBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	if len(b.tasks) == 1 {
		return delegate.Result{
			Output:      map[string]any{"reply": "here is my answer", "_tokens": 83_000, "_cost_usd": 0.63},
			Tokens:      83_000,
			SessionID:   "sess-copi",
			BackendName: delegate.BackendClaudeCode,
		}, nil
	}
	return delegate.Result{
		Output:      map[string]any{"reply": "here is my answer", "requires_authoring_context": false, "_tokens": 900, "_cost_usd": 0.01},
		Tokens:      900,
		SessionID:   "sess-copi",
		BackendName: delegate.BackendClaudeCode,
	}, nil
}

// The shape of #1385: a structured answer missing one required boolean at
// the end of a session. The engine's node must end on the re-asked answer —
// never failed_resumable, which cost the operator a resume and the whole
// turn again — and the run's record must name the re-ask and its outcome.
// Driven through the production ClawExecutor and a real delegate.Backend so
// the path under test is the one a run takes.
func TestSchemaReaskDeliversTheNodeInsteadOfFailingResumable(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "reask_test",
		Entry: "copi",
		Nodes: map[string]ir.Node{
			"copi": &ir.AgentNode{
				BaseNode:     ir.BaseNode{ID: "copi"},
				LLMFields:    ir.LLMFields{Backend: delegate.BackendClaudeCode, Model: "claude-opus-5"},
				SchemaFields: ir.SchemaFields{OutputSchema: "copi_turn"},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "copi", To: "done"}},
		Schemas: map[string]*ir.Schema{"copi_turn": {
			Name: "copi_turn",
			Fields: []*ir.SchemaField{
				{Name: "reply", Type: ir.FieldTypeString},
				{Name: "requires_authoring_context", Type: ir.FieldTypeBool},
			},
		}},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 1_000_000},
	}
	backend := &reaskBackend{}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, backend)
	st := tmpStore(t)
	ctx := context.Background()
	// The store hooks are what turn the executor's lifecycle hooks into the
	// run's events — the record this test reads the re-ask back from.
	hooks := model.NewStoreEventHooks(ctx, st, "run-reask", iterlog.New(iterlog.LevelError, io.Discard), model.BuildSecretGuard(ctx, wf, nil))
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(reg),
		model.WithEventHooks(hooks),
		model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}),
	)
	eng := New(wf, st, exec)
	if err := eng.Run(context.Background(), "run-reask", nil); err != nil {
		t.Fatalf("the re-ask was supposed to carry the run through, got: %v", err)
	}
	run, err := st.LoadRun(context.Background(), "run-reask")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("run status = %s, want finished (a failed_resumable here is the outcome #1385 reported)", run.Status)
	}
	if len(backend.tasks) != 2 {
		t.Fatalf("expected the answer and one re-ask, got %d delegation(s)", len(backend.tasks))
	}
	// The re-ask resumed the session the answer ran in and asked only for
	// the fix — it did not run the turn again.
	reask := backend.tasks[1]
	if reask.SessionID != "sess-copi" || reask.ForkSession {
		t.Errorf("the re-ask did not resume the answer's session: session=%q fork=%v", reask.SessionID, reask.ForkSession)
	}
	if !strings.Contains(reask.UserPrompt, "requires_authoring_context") {
		t.Errorf("the re-ask does not name the missing field: %q", reask.UserPrompt)
	}

	events, err := st.LoadEvents(context.Background(), "run-reask")
	if err != nil {
		t.Fatal(err)
	}
	var announced, ended bool
	for _, ev := range events {
		switch ev.Type {
		case store.EventDelegateRetry:
			msg, _ := ev.Data["error"].(string)
			if ev.Data["reask"] == model.ReaskResumeSession && strings.Contains(msg, "requires_authoring_context") {
				announced = true
			}
		case store.EventDelegateFinished:
			if eventNumber(ev.Data["attempt"]) == 2 && ev.Data["reask"] == model.ReaskResumeSession && eventNumber(ev.Data["tokens"]) == 900 {
				ended = true
			}
		}
	}
	if !announced {
		t.Error("no delegate_retry event names the re-ask (reask mode + the validation error)")
	}
	if !ended {
		t.Error("no delegate_finished event marks the re-ask's own end (attempt 2, its own tokens)")
	}
}
