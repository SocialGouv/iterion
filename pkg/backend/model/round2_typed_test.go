package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

type r2ScriptedBackend struct {
	mu    sync.Mutex
	calls int
	steps []func() (delegate.Result, error)
}

func (b *r2ScriptedBackend) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := b.calls
	b.calls++
	if i < len(b.steps) {
		return b.steps[i]()
	}
	return delegate.Result{}, errors.New("no more steps")
}

// P5: the re-ask that consumed the LAST correction attempt fails with a typed
// provider error (usage window / 401). The extraction rescue then asks the
// policy for an attempt it no longer has. Which error does the node end on?
func TestR2_ExhaustedExtractionReplacesTheReaskTypedError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"usage_window", &delegate.ErrRateLimited{Provider: "test_backend", Kind: delegate.RateLimitKindUsageWindow, Detail: "weekly limit reached"}},
		{"auth", &delegate.ErrAuthFailed{Provider: "test_backend", Detail: "upstream 401: token revoked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("ANTHROPIC_BASE_URL", "")
			t.Setenv("ANTHROPIC_API_KEY", "test-key")
			t.Setenv(EnvOutputCorrectionBudget, "1")
			schema := &ir.Schema{Name: "out_schema", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}
			schemaJSON, err := SchemaToJSON(schema)
			if err != nil {
				t.Fatal(err)
			}
			clawReg := NewRegistry()
			extraction := newMockClient(textEvents("narration", 10, 5))
			clawReg.Register("anthropic", func(string) (api.APIClient, error) { return extraction, nil })
			reaskErr := tc.err
			backend := &r2ScriptedBackend{steps: []func() (delegate.Result, error){
				func() (delegate.Result, error) { return delegate.Result{}, reaskErr },
			}}
			delegateReg := delegate.NewRegistry()
			delegateReg.Register("test_backend", backend)
			exec := NewClawExecutor(clawReg,
				&ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{"out_schema": schema}},
				WithBackendRegistry(delegateReg),
				WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
			)
			be, _ := delegateReg.Resolve("test_backend")
			task := &delegate.Task{OutputSchema: schemaJSON}
			first := delegate.Result{CostUSD: 1.00, Output: map[string]any{"text": "the answer is blue"}, Tokens: 1_000, ParseFallback: true, BackendName: "test_backend"}
			_, err = exec.validateAndRetry(context.Background(), backendFields{id: "answerer", outputSchema: "out_schema"}, "test_backend", be, task, first, schema)
			if !errors.Is(err, tc.err) {
				t.Errorf("typed refusal lost: %v", err)
			}
			var schemaErr *OutputSchemaError
			var rl *delegate.ErrRateLimited
			var af *delegate.ErrAuthFailed
			t.Logf("node error: %v", err)
			t.Logf("errors.As OutputSchemaError=%v ErrRateLimited=%v ErrAuthFailed=%v re-ask calls=%d extraction calls=%d",
				errors.As(err, &schemaErr), errors.As(err, &rl), errors.As(err, &af), backend.calls, len(extraction.getCalls()))
		})
	}
}
