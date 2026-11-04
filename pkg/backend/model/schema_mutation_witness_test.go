package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

func mutationExecutor(t *testing.T, backend *capturingBackend, reg *Registry) *ClawExecutor {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	dreg := delegate.NewRegistry()
	dreg.Register(delegate.BackendClaudeCode, backend)
	return NewClawExecutor(reg, verdictWorkflow(), WithBackendRegistry(dreg), WithRetryPolicy(RetryPolicy{MaxAttempts: 1}))
}

func TestCorrectionBudgetStopsChangingAnswers(t *testing.T) {
	t.Setenv(EnvOutputCorrectionBudget, "1")
	b := &capturingBackend{results: []delegate.Result{
		{Output: map[string]any{"verdict": true}},
		{Output: map[string]any{"verdict": true, "reason": 42}},
		{Output: map[string]any{"verdict": true, "reason": "too late"}},
	}}
	exec := mutationExecutor(t, b, NewRegistry())
	_, err := exec.executeBackend(context.Background(), verdictJudge(delegate.BackendClaudeCode), nil)
	var schemaErr *OutputSchemaError
	if len(b.tasks) != 2 || !errors.As(err, &schemaErr) {
		t.Fatalf("allowance exceeded without progress convergence: calls=%d err=%v", len(b.tasks), err)
	}
}

func TestReaskBackfillsSessionBeforeNextCorrection(t *testing.T) {
	t.Setenv(EnvOutputCorrectionBudget, "2")
	b := &capturingBackend{results: []delegate.Result{
		{Output: map[string]any{"verdict": true}, SessionID: "session-1"},
		{Output: map[string]any{"verdict": true, "reason": 42}},
		{Output: map[string]any{"verdict": true, "reason": "fixed"}},
	}}
	exec := mutationExecutor(t, b, NewRegistry())
	out, err := exec.executeBackend(context.Background(), verdictJudge(delegate.BackendClaudeCode), nil)
	if err != nil || len(b.tasks) != 3 {
		t.Fatalf("calls=%d err=%v", len(b.tasks), err)
	}
	if b.tasks[2].SessionID != "session-1" || out["_session_id"] != "session-1" {
		t.Fatalf("session disappeared between corrections: task=%q output=%v", b.tasks[2].SessionID, out)
	}
}

func TestRestartClearsAnsweredPauseControl(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow())
	task := &delegate.Task{UserPrompt: "business", ResumeConversation: []byte(`[]`), ResumePendingToolUseID: "asked", ResumeAnswer: "operator-choice", UserContent: []delegate.ContentBlock{{Type: "text", Text: "business"}}}
	r := exec.planSchemaReask(context.Background(), "grok", task, delegate.Result{}, "schema-feedback")
	if r.mode != ReaskRestart || len(r.task.ResumeConversation) != 0 || r.task.ResumePendingToolUseID != "" || r.task.ResumeAnswer != "" {
		t.Fatalf("restart replays answered pause: %+v", r.task)
	}
	if !strings.Contains(r.task.UserPrompt, "operator-choice") || !strings.Contains(r.task.UserPrompt, "schema-feedback") {
		t.Fatalf("restart loses answer or feedback: %q", r.task.UserPrompt)
	}
	var content strings.Builder
	for _, block := range r.task.UserContent {
		content.WriteString(block.Text)
	}
	if !strings.Contains(content.String(), "operator-choice") || !strings.Contains(content.String(), "schema-feedback") {
		t.Fatalf("multimodal restart loses answer or feedback: %s", content.String())
	}
}

func TestExtractionReservesSharedCorrectionAllowance(t *testing.T) {
	t.Setenv(EnvOutputCorrectionBudget, "1")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	client := newMockClient(toolUseEvents("extract", "structured_output", `{"verdict":true,"reason":"fixed"}`, 50, 20))
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return client, nil })
	b := &capturingBackend{results: []delegate.Result{
		{Output: map[string]any{"text": "first prose"}, ParseFallback: true},
		{Output: map[string]any{"text": "second prose"}, ParseFallback: true},
	}}
	exec := mutationExecutor(t, b, reg)
	_, err := exec.executeBackend(context.Background(), verdictJudge(delegate.BackendClaudeCode), nil)
	var schemaErr *OutputSchemaError
	if !errors.As(err, &schemaErr) || len(client.getCalls()) != 0 {
		t.Fatalf("extraction spent beyond shared allowance: calls=%d err=%v", len(client.getCalls()), err)
	}
}

func TestClawExecuteReportsPerCallCost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	client := newMockClient(toolUseEvents("cost", "structured_output", `{"verdict":true,"reason":"ok"}`, 50, 20))
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
	schema, err := SchemaToJSON(verdictWorkflow().Schemas["verdict_schema"])
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1}).Execute(context.Background(), delegate.Task{Model: "test/test-model", OutputSchema: schema, UserPrompt: "judge"})
	if err != nil || result.Tokens != 70 {
		t.Fatalf("execute: %+v err=%v", result, err)
	}
	if result.CostIsSessionTotal {
		t.Fatal("real claw Execute declares a per-call bill as a session total")
	}
}
