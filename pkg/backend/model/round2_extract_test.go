package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// A malformed tool block has no model prose for extraction. Its parse
// diagnostic must never become an accepted business answer.
func TestR2_ClawParseFailureFeedsTheErrorDescriptionToTheExtraction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	node := newMockClient(
		toolUseEvents("tu_1", "structured_output", `{"verdict":tru`, 50, 20),
		toolUseEvents("tu_2", "structured_output", `{"verdict":tru`, 50, 20),
		toolUseEvents("tu_3", "structured_output", `{"verdict":fals`, 60, 20),
		toolUseEvents("tu_4", "structured_output", `{"verdict":fals`, 60, 20),
		toolUseEvents("tu_5", "structured_output", `{"verdict":fals`, 60, 20),
		toolUseEvents("tu_6", "structured_output", `{"verdict":fals`, 60, 20),
	)
	extraction := newMockClient(
		// an extractor told to "populate every required field"
		toolUseEvents("ex_1", "structured_output", `{"verdict":false,"reason":"the tool block was malformed"}`, 200, 30),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return node, nil })
	reg.Register("anthropic", func(string) (api.APIClient, error) { return extraction, nil })
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	out, err := exec.Execute(WithRunID(context.Background(), "run-extract"), verdictJudge(""), map[string]any{})
	var schemaErr *OutputSchemaError
	if !errors.As(err, &schemaErr) || len(extraction.getCalls()) != 0 {
		t.Fatalf("error metadata accepted as answer: err=%v extraction=%d out=%v", err, len(extraction.getCalls()), out)
	}
	t.Logf("node err=%v", err)
	t.Logf("node output=%v", out)
	t.Logf("node provider calls=%d extraction calls=%d", len(node.getCalls()), len(extraction.getCalls()))
	for i, c := range extraction.getCalls() {
		for _, m := range c.Messages {
			for _, b := range m.Content {
				txt := b.Text
				if len(txt) > 420 {
					txt = txt[:420] + "…"
				}
				t.Logf("extraction call %d %s: %q", i+1, m.Role, txt)
				if strings.Contains(b.Text, "model did not produce") {
					t.Logf("=> the extraction was handed the StructuredOutputError description as \"the assistant's finished answer\"")
				}
			}
		}
	}
}

func TestR2_ClawParseFailurePreservesRealProseForExtraction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	prose := "The verdict is true because all checks passed. " + strings.Repeat("actual model explanation. ", 300)
	primary := newMockClient(textEvents(prose, 50, 20), textEvents(prose, 50, 20), textEvents(prose, 50, 20), textEvents(prose, 50, 20))
	extraction := newMockClient(toolUseEvents("extract", "structured_output", `{"verdict":true,"reason":"all checks passed"}`, 50, 20))
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return primary, nil })
	reg.Register("anthropic", func(string) (api.APIClient, error) { return extraction, nil })
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithRetryPolicy(RetryPolicy{MaxAttempts: 1}))
	out, err := exec.Execute(WithRunID(context.Background(), "actual-prose"), verdictJudge(""), nil)
	if err != nil || out["verdict"] != true {
		t.Fatalf("real prose cannot be extracted: output=%v err=%v", out, err)
	}
	calls := extraction.getCalls()
	if len(calls) != 1 {
		t.Fatalf("extraction calls=%d, want 1", len(calls))
	}
	var answer strings.Builder
	for _, message := range calls[0].Messages {
		for _, block := range message.Content {
			answer.WriteString(block.Text)
		}
	}
	if !strings.Contains(answer.String(), prose) || strings.Contains(answer.String(), "model did not produce") {
		t.Fatalf("extraction lost or replaced the complete model answer: %d bytes", answer.Len())
	}
}
