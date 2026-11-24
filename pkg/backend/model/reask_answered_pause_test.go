package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

func TestRVA_RestartAfterAPauseDropsTheOperatorsAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	const operatorAnswer = "OPERATOR-SAYS-ONLY-FILE-A"
	mock := newMockClient(
		textEvents(`{"verdict":true}`, 50, 20),
		toolUseEvents("tu_2", "structured_output", `{"verdict":true,"reason":"because"}`, 30, 10),
	)
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return mock, nil })
	tr := tool.NewRegistry()
	for _, name := range []string{"read_file", "todo_write"} {
		if err := tr.RegisterBuiltin(name, "test tool "+name, nil, jsonExec(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	obs := &reaskObserver{}
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithToolRegistry(tr), WithEventHooks(obs.hooks()))
	node := verdictJudge("")
	node.Tools = []string{"read_file"}
	pause, err := json.Marshal([]api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: reaskPrompt}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "tool_use", ID: "tu_ask", Name: "ask_user", Input: map[string]any{"question": "which files may I judge?"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{
		delegate.ResumeConversationKey:     json.RawMessage(pause),
		delegate.ResumePendingToolUseIDKey: "tu_ask",
		delegate.ResumeAnswerKey:           operatorAnswer,
	}
	// No WithRunID: nothing captured, so the re-ask is a restart.
	out, err := exec.executeBackend(context.Background(), node, input)
	t.Logf("out=%v err=%v", out, err)
	calls := mock.getCalls()
	for i, c := range calls {
		answer := 0
		for _, m := range c.Messages {
			for _, b := range m.Content {
				for _, inner := range b.Content {
					answer += strings.Count(inner.Text, operatorAnswer)
				}
				answer += strings.Count(b.Text, operatorAnswer)
			}
		}
		t.Logf("call %d: %d messages, operator answer occurrences=%d, tools offered=%d", i+1, len(c.Messages), answer, len(c.Tools))
		for j, m := range c.Messages {
			var kinds []string
			for _, b := range m.Content {
				txt := b.Text
				if len(txt) > 70 {
					txt = txt[:70]
				}
				kinds = append(kinds, b.Type+":"+txt)
			}
			t.Logf("   [%d] %s %v", j, m.Role, kinds)
		}
	}
	if len(obs.retries) > 0 {
		t.Logf("reask mode=%s", obs.retries[0].Reask)
	}
	if len(calls) < 2 {
		t.Fatalf("calls=%d err=%v", len(calls), err)
	}
	{
		got := 0
		for _, m := range calls[1].Messages {
			for _, b := range m.Content {
				got += strings.Count(b.Text, operatorAnswer)
				for _, inner := range b.Content {
					got += strings.Count(inner.Text, operatorAnswer)
				}
			}
		}
		raw, _ := json.Marshal(calls[1].Messages)
		if !strings.Contains(string(raw), schemaRetryFeedbackMarker) {
			t.Error("corrective turn lost validation feedback")
		}
		if got != 1 {
			t.Errorf("the corrective restart of a resumed node no longer carries the operator's answer")
		}
	}
}
