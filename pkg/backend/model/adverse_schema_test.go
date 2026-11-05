package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestRVA_ExtractionFailureBecomesTheNodeError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	schema := &ir.Schema{Name: "out_schema", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}
	schemaJSON, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatal(err)
	}
	clawReg := NewRegistry()
	extraction := newMockClient(textEvents("I would rather narrate.", 200, 50), textEvents("still narrating", 200, 50))
	clawReg.Register("anthropic", func(string) (api.APIClient, error) { return extraction, nil })
	backend := &capturingBackend{results: []delegate.Result{{
		CostUSD: 0.50, Output: map[string]any{"text": "the answer is blue"}, Tokens: 500, ParseFallback: true, BackendName: "test_backend",
	}}}
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
	var schemaErr *OutputSchemaError
	var structErr *StructuredOutputError
	t.Logf("node error: %v", err)
	t.Logf("errors.As OutputSchemaError=%v  errors.As StructuredOutputError=%v  delegations=%d  extraction calls=%d",
		errors.As(err, &schemaErr), errors.As(err, &structErr), len(backend.tasks), len(extraction.getCalls()))
	if !errors.As(err, &schemaErr) {
		t.Errorf("a schema failure ended on the extraction's own error, not the typed SCHEMA_VALIDATION exhaustion: %v", err)
	}
}

func TestRVA_ClawParseFailureSkipsTheCorrection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	mock := newMockClient(
		toolUseEvents("tu_1", "structured_output", `{"verdict":tru`, 50, 20),
		toolUseEvents("tu_2", "structured_output", `{"verdict":tru`, 50, 20),
		toolUseEvents("tu_3", "structured_output", `{"verdict":true,"reason":"fixed"}`, 30, 10),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	obs := &reaskObserver{}
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithEventHooks(obs.hooks()), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	out, err := exec.Execute(WithRunID(context.Background(), "run-parse"), verdictJudge(""), map[string]any{})
	var schemaErr *OutputSchemaError
	var structErr *StructuredOutputError
	t.Logf("output=%v", out)
	t.Logf("err=%v", err)
	t.Logf("provider calls=%d  schema re-asks announced=%d  OutputSchemaError=%v StructuredOutputError=%v",
		len(mock.getCalls()), len(obs.retries), errors.As(err, &schemaErr), errors.As(err, &structErr))
	if err != nil || len(obs.retries) != 1 || !strings.Contains(obs.retries[0].Error.Error(), "malformed JSON") {
		t.Errorf("claw parse failure surfaced untyped (no correction with feedback, not SCHEMA_VALIDATION): %v", err)
	}
}
