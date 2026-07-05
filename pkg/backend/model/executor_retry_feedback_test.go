package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// capturingBackend records every Task it is asked to Execute so a test can
// assert on the prompt content the executor sends per attempt.
type capturingBackend struct {
	mu      sync.Mutex
	tasks   []delegate.Task
	results []delegate.Result
}

func (b *capturingBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	idx := len(b.tasks)
	b.tasks = append(b.tasks, task)
	if idx < len(b.results) {
		return b.results[idx], nil
	}
	return delegate.Result{}, nil
}

// TestValidateAndRetry_InjectsSchemaFeedback proves that when structured
// output fails validation with a retry-eligible error (missing required
// field), the executor's SECOND backend call receives a UserPrompt augmented
// with the schema-validation feedback marker — the model is told what failed
// instead of blindly re-running the identical prompt.
func TestValidateAndRetry_InjectsSchemaFeedback(t *testing.T) {
	backend := &capturingBackend{
		results: []delegate.Result{
			// 1st call: valid JSON but missing the required "answer" field.
			{Output: map[string]interface{}{"other": "x"}, BackendName: "test_backend"},
			// 2nd call: corrected, schema-valid output.
			{Output: map[string]interface{}{"answer": "blue"}, BackendName: "test_backend"},
		},
	}

	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{},
		Schemas: map[string]*ir.Schema{
			"out_schema": {
				Name:   "out_schema",
				Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}},
			},
		},
	}
	exec := NewClawExecutor(NewRegistry(), wf,
		WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 3, BackoffBase: time.Millisecond}),
	)

	node := &ir.AgentNode{
		BaseNode:     ir.BaseNode{ID: "answerer"},
		LLMFields:    ir.LLMFields{Backend: "test_backend"},
		SchemaFields: ir.SchemaFields{OutputSchema: "out_schema"},
	}

	output, err := exec.executeBackend(context.Background(), node, map[string]interface{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := output["answer"]; got != "blue" {
		t.Fatalf("expected corrected answer=blue, got %v", output)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.tasks) != 2 {
		t.Fatalf("expected exactly 2 backend calls (attempt + feedback retry), got %d", len(backend.tasks))
	}
	if strings.Contains(backend.tasks[0].UserPrompt, schemaRetryFeedbackMarker) {
		t.Errorf("first attempt must NOT carry the retry feedback marker: %q", backend.tasks[0].UserPrompt)
	}
	if !strings.Contains(backend.tasks[1].UserPrompt, schemaRetryFeedbackMarker) {
		t.Errorf("retry attempt UserPrompt missing feedback marker %q: %q", schemaRetryFeedbackMarker, backend.tasks[1].UserPrompt)
	}
	if !strings.Contains(backend.tasks[1].UserPrompt, "missing required field") {
		t.Errorf("retry feedback should name the validation error, got: %q", backend.tasks[1].UserPrompt)
	}
}
