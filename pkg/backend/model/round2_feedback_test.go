package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Capping an offending value preserves violations of later fields.
func TestR2_FeedbackCapDropsLaterViolations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	schema := &ir.Schema{Name: "s", Fields: []*ir.SchemaField{
		{Name: "kind", Type: ir.FieldTypeString, EnumValues: []string{"bug", "feature"}},
		{Name: "title", Type: ir.FieldTypeString},
	}}
	long := strings.Repeat("x", 3000)
	verr := ValidateOutput(map[string]any{"kind": long}, schema)
	fb := formatSchemaRetryFeedback(verr)
	if !strings.Contains(fb, `"title"`) {
		t.Error("later field violation lost")
	}
	t.Logf("validation error=%d bytes, mentions title=%v", len(verr.Error()), strings.Contains(verr.Error(), `"title"`))
	t.Logf("feedback=%d bytes, mentions title=%v, tail=%q", len(fb), strings.Contains(fb, `"title"`), fb[len(fb)-160:])
}

// The parse diagnostic stays bounded in node errors, retry events and the
// ledger, while the extraction request still receives the complete answer.
func TestR2_ParseFallbackErrorQuotesTheWholeProse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	schema := &ir.Schema{Name: "out_schema", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}
	schemaJSON, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatal(err)
	}
	prose := "Here is my long analysis. " + strings.Repeat("lorem ipsum dolor sit amet ", 8000)
	backend := &capturingBackend{results: []delegate.Result{
		{Output: map[string]any{"text": prose + " (second)"}, ParseFallback: true, BackendName: "test_backend", SessionID: "s1"},
		{Output: map[string]any{"text": prose + " (third)"}, ParseFallback: true, BackendName: "test_backend", SessionID: "s1"},
	}}
	reg := delegate.NewRegistry()
	reg.Register("test_backend", backend)
	var retryErrLens []int
	exec := NewClawExecutor(NewRegistry(), &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{"out_schema": schema}},
		WithBackendRegistry(reg), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
		WithEventHooks(EventHooks{OnDelegateRetry: func(_ string, i DelegateInfo) {
			if i.Error != nil {
				retryErrLens = append(retryErrLens, len(i.Error.Error()))
			}
		}}))
	be, _ := reg.Resolve("test_backend")
	task := &delegate.Task{OutputSchema: schemaJSON}
	first := delegate.Result{Output: map[string]any{"text": prose}, ParseFallback: true, BackendName: "test_backend", SessionID: "s1"}
	var befores []int
	ctx := WithOutputCorrectionPolicy(context.Background(), OutputCorrectionPolicy{
		Before: func(_ context.Context, _ map[string]any, e error) error {
			befores = append(befores, len(e.Error()))
			if len(befores) > 2 {
				return &OutputSchemaError{Cause: e}
			}
			return nil
		},
		After: func(context.Context, map[string]any, error) error { return nil },
	})
	_, err = exec.validateAndRetry(ctx, backendFields{id: "answerer", outputSchema: "out_schema"}, "test_backend", be, task, first, schema)
	if err == nil || len(err.Error()) > 4096 {
		t.Fatalf("unbounded parse error: %d bytes", len(err.Error()))
	}
	for _, n := range append(retryErrLens, befores...) {
		if n > 4096 {
			t.Errorf("unbounded event/ledger error: %d", n)
		}
	}
	fed := 0
	for _, tk := range backend.tasks {
		fed = max(fed, len(tk.UserPrompt))
	}
	t.Logf("prose=%d bytes; node error=%d bytes; delegate_retry error lengths=%v; ledger Before() error lengths=%v; largest re-ask prompt=%d bytes",
		len(prose), len(err.Error()), retryErrLens, befores, fed)
}
