package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The frozen pool stamp must survive the DOCUMENT built from the named
// field list: the wire carries it (child := *msg), but a child doc reading
// "" would fail the doc ≡ message admission for every subbot node of a
// pool-team run (rva F3). Red when the stamping, the named-field gate or
// the WithRunnerPool option is dropped.
func TestEngineRun_TheFrozenPoolStampReachesTheDocument(t *testing.T) {
	pr := parser.Parse("pool.bot", `schema out:
  answer: string
prompt p:
  Say the word.
agent probe:
  backend: claw
  model: "openai/gpt-test"
  system: p
  output: out
workflow pool:
  entry: probe
  worktree: none
  probe -> done
`)
	if len(pr.Diagnostics) > 0 {
		t.Fatalf("parse: %v", pr.Diagnostics)
	}
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("compile: %v", cr.Diagnostics)
	}
	st, err := store.New(t.TempDir(), store.WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	be := &poolDocBackend{}
	br := delegate.NewRegistry()
	br.Register(delegate.BackendClaw, be)
	ex := model.NewClawExecutor(model.NewRegistry(), cr.Workflow, model.WithBackendRegistry(br), model.WithLogger(iterlog.Nop()))
	e := New(cr.Workflow, st, ex,
		WithWorkDir(t.TempDir()),
		WithLogger(iterlog.Nop()),
		WithRunnerPool("honorabilite"),
	)
	const runID = "run-pool-doc"
	if err := e.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	r, err := st.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if r.RunnerPool != "honorabilite" {
		t.Fatalf("the document lost the frozen pool stamp: %+v", r)
	}
}

type poolDocBackend struct{ calls int }

func (b *poolDocBackend) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.calls++
	return delegate.Result{Output: map[string]any{"answer": "ok"}, BackendName: delegate.BackendClaw}, nil
}
