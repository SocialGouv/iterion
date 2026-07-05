package model

import (
	"encoding/json"
	"testing"
)

// TestParsePlanTodos covers both backend input shapes: claude_code's
// TodoWrite (content/status/activeForm) and claw's todo_write
// (id/content/status=done/priority), plus status canonicalisation and
// the no-todos / unparsable guards.
func TestParsePlanTodos(t *testing.T) {
	t.Run("claude_code TodoWrite shape", func(t *testing.T) {
		in := json.RawMessage(`{"todos":[
			{"content":"build it","status":"in_progress","activeForm":"building it"},
			{"content":"ship it","status":"pending"}
		]}`)
		got := parsePlanTodos(in, nil)
		if len(got) != 2 {
			t.Fatalf("expected 2 todos, got %d", len(got))
		}
		if got[0].ActiveForm != "building it" || got[0].Status != "in_progress" {
			t.Errorf("first todo not parsed: %+v", got[0])
		}
	})

	t.Run("claw todo_write shape: done→completed, priority kept", func(t *testing.T) {
		in := json.RawMessage(`{"action":"write","todos":[
			{"id":"t1","content":"done task","status":"done","priority":"high"}
		]}`)
		got := parsePlanTodos(in, nil)
		if len(got) != 1 {
			t.Fatalf("expected 1 todo, got %d", len(got))
		}
		if got[0].Status != "completed" {
			t.Errorf("expected done→completed, got %q", got[0].Status)
		}
		if got[0].Priority != "high" || got[0].ID != "t1" {
			t.Errorf("claw fields lost: %+v", got[0])
		}
	})

	t.Run("no todos (claw read) yields nil", func(t *testing.T) {
		if got := parsePlanTodos(json.RawMessage(`{"action":"read"}`), nil); got != nil {
			t.Errorf("expected nil for read call, got %+v", got)
		}
	})

	t.Run("unparsable yields nil", func(t *testing.T) {
		if got := parsePlanTodos(json.RawMessage(`not json`), nil); got != nil {
			t.Errorf("expected nil for bad json, got %+v", got)
		}
		if got := parsePlanTodos(nil, nil); got != nil {
			t.Errorf("expected nil for empty input, got %+v", got)
		}
	})

	t.Run("redactor scrubs free-text fields", func(t *testing.T) {
		in := json.RawMessage(`{"todos":[{"content":"secret-abc","status":"pending","activeForm":"secret-abc now"}]}`)
		redact := func(s string) string {
			if s == "secret-abc" {
				return "***"
			}
			if s == "secret-abc now" {
				return "*** now"
			}
			return s
		}
		got := parsePlanTodos(in, redact)
		if got[0].Content != "***" || got[0].ActiveForm != "*** now" {
			t.Errorf("redaction not applied: %+v", got[0])
		}
	})
}

func TestIsPlanTool(t *testing.T) {
	for _, name := range []string{"TodoWrite", "todo_write"} {
		if !isPlanTool(name) {
			t.Errorf("%q should be a plan tool", name)
		}
	}
	for _, name := range []string{"Bash", "Read", "todowrite", ""} {
		if isPlanTool(name) {
			t.Errorf("%q should NOT be a plan tool", name)
		}
	}
}
