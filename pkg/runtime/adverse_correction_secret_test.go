package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

type adverseBackendFunc func(context.Context, delegate.Task) (delegate.Result, error)

func (f adverseBackendFunc) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	return f(ctx, task)
}

func TestCorrectionLedgerScrubsModelValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	const secret = "sk-live-9f8e7d6c5b4a3210ffeeddccbbaa0099"
	wf := validationWorkflow()
	wf.Nodes["my_agent"].(*ir.AgentNode).Backend = delegate.BackendClaudeCode
	wf.Schemas["MySchema"].Fields[0].EnumValues = []string{"pass"}
	guard := secretguard.New([]secretguard.Secret{{Name: "API", Value: secret}}, secretguard.DefaultConfig())
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, adverseBackendFunc(func(context.Context, delegate.Task) (delegate.Result, error) {
		return delegate.Result{Output: map[string]any{"summary": secret, "score": 1}, Tokens: 10, BackendName: delegate.BackendClaudeCode}, nil
	}))
	exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg), model.WithSecretGuard(guard))
	st := tmpStore(t)
	eng := New(wf, st, exec, WithOutputCorrectionBudget(1))
	err := eng.Run(context.Background(), "secret-correction", nil)
	var schemaErr *model.OutputSchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("schema error: %v", err)
	}
	run, err := st.LoadRun(context.Background(), "secret-correction")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.OutputCorrections) != 1 {
		t.Fatalf("ledger=%v", run.OutputCorrections)
	}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("persisted correction ledger contains raw model secret: %s", run.OutputCorrections["my_agent"].LastError)
	}
	if run.OutputCorrections["my_agent"].LastError == "" {
		t.Fatal("validation failure is missing")
	}
}
