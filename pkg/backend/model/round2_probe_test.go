package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// A malformed tool block with zero reported usage still returns a partial
// result without panicking on the empty metered result.
func TestR2_ClawParseFailureWithZeroReportedUsage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	mock := newMockClient(
		toolUseEvents("tu_1", "structured_output", `{"verdict":tru`, 0, 0),
		toolUseEvents("tu_2", "structured_output", `{"verdict":tru`, 0, 0),
		toolUseEvents("tu_3", "structured_output", `{"verdict":true,"reason":"fixed"}`, 0, 0),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	var out map[string]any
	var err error
	panicked := func() (p any) {
		defer func() { p = recover() }()
		out, err = exec.Execute(WithRunID(context.Background(), "run-zero"), verdictJudge(""), map[string]any{})
		return nil
	}()
	t.Logf("panic=%v", panicked)
	t.Logf("out=%v err=%v calls=%d", out, err, len(mock.getCalls()))
	if panicked != nil {
		t.Errorf("PANIC on a zero-usage claw parse failure: %v", panicked)
	}
}

// A failed continue_conversation re-ask retains its latest validation
// feedback for the following correction call.
func TestR2_ClawParseFailureOnReaskContinuesAStaleConversation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	mock := newMockClient(
		// business call: valid JSON, missing "reason"
		toolUseEvents("tu_1", "structured_output", `{"verdict":true}`, 50, 20),
		// re-ask 1 (continue_conversation): forced turn malformed, unforced turn malformed
		toolUseEvents("tu_2", "structured_output", `{"verdict":tru`, 60, 20),
		toolUseEvents("tu_3", "structured_output", `{"verdict":tru`, 70, 20),
		// re-ask 2
		toolUseEvents("tu_4", "structured_output", `{"verdict":true,"reason":"fixed"}`, 80, 10),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	obs := &reaskObserver{}
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithEventHooks(obs.hooks()), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	out, err := exec.Execute(WithRunID(context.Background(), "run-stale"), verdictJudge(""), map[string]any{})
	t.Logf("out=%v err=%v", out, err)
	calls := mock.getCalls()
	t.Logf("provider calls=%d retries announced=%d", len(calls), len(obs.retries))
	for i, r := range obs.retries {
		t.Logf("retry[%d] attempt=%d mode=%s error=%v", i, r.Attempt, r.Reask, r.Error)
	}
	for i, c := range calls {
		var parts []string
		for _, m := range c.Messages {
			var txt []string
			for _, b := range m.Content {
				s := b.Text
				if b.Type == "tool_result" {
					s = "tool_result"
				}
				if len(s) > 160 {
					s = s[:160] + "…"
				}
				txt = append(txt, fmt.Sprintf("%s:%q", b.Type, s))
			}
			parts = append(parts, m.Role+"["+strings.Join(txt, ",")+"]")
		}
		t.Logf("call %d: %d messages: %s", i+1, len(c.Messages), strings.Join(parts, " | "))
	}
	if len(calls) >= 4 {
		last := calls[3]
		raw, _ := json.Marshal(last.Messages)
		mentionsReason := false
		for _, m := range last.Messages {
			for _, b := range m.Content {
				if strings.Contains(b.Text, `missing required field "reason"`) {
					mentionsReason = true
				}
			}
		}
		mentionsParse := strings.Contains(string(raw), "could not be parsed")
		if !mentionsReason || !mentionsParse {
			t.Errorf("lost correction history: %s", raw)
		}
		t.Logf("call 4: carries the first feedback (missing reason)=%v carries the parse feedback=%v", mentionsReason, mentionsParse)
	}
}
