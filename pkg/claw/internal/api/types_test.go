package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestContentBlockMarshalJSON_ToolUseInputAlwaysPresent guards the
// Anthropic protocol invariant: tool_use blocks must carry an
// `input` object on the wire, even when the LLM produced no
// arguments. Without the custom MarshalJSON the field is `omitempty`
// and Go strips empty maps, which Anthropic then rejects on the
// next turn with
//
//	messages.N.content.M.tool_use.input: Field required (400).
func TestContentBlockMarshalJSON_ToolUseInputAlwaysPresent(t *testing.T) {
	cases := []struct {
		name  string
		block ContentBlock
	}{
		{
			name: "nil input",
			block: ContentBlock{
				Type: "tool_use",
				ID:   "toolu_1",
				Name: "enter_plan_mode",
			},
		},
		{
			name: "empty map input",
			block: ContentBlock{
				Type:  "tool_use",
				ID:    "toolu_2",
				Name:  "exit_plan_mode",
				Input: map[string]any{},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.block)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !strings.Contains(string(b), `"input":{}`) {
				t.Errorf("expected `\"input\":{}` in output, got: %s", string(b))
			}
		})
	}
}

// TestContentBlockMarshalJSON_NonToolUseStillOmitsInput verifies the
// custom MarshalJSON does not regress the omitempty behaviour for
// text / tool_result / image blocks — only tool_use forces input.
func TestContentBlockMarshalJSON_NonToolUseStillOmitsInput(t *testing.T) {
	cases := []ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "tool_result", ToolUseID: "toolu_3", Content: []ContentBlock{{Type: "text", Text: "ok"}}},
	}
	for _, b := range cases {
		t.Run(b.Type, func(t *testing.T) {
			out, err := json.Marshal(b)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if strings.Contains(string(out), `"input"`) {
				t.Errorf("non-tool_use block should not emit input field, got: %s", string(out))
			}
		})
	}
}

// TestContentBlockMarshalJSON_ToolUseWithNonEmptyInputUnchanged
// guards the common path: when the LLM did supply args, the
// MarshalJSON forwarder must not clobber them.
func TestContentBlockMarshalJSON_ToolUseWithNonEmptyInputUnchanged(t *testing.T) {
	b := ContentBlock{
		Type:  "tool_use",
		ID:    "toolu_4",
		Name:  "read_file",
		Input: map[string]any{"path": "/tmp/x"},
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), `"path":"/tmp/x"`) {
		t.Errorf("expected path in serialised input, got: %s", string(out))
	}
}

// TestContentBlockMarshalJSON_TextAlwaysPresent guards the Anthropic
// protocol invariant for text blocks: the `text` field must be present
// even when empty. An empty tool result (a grep with no matches, a
// silent git add) is spliced into a tool_result as a nested
// {type:"text", text:""} block; without forcing the field present the
// omitempty rule drops it and Anthropic rejects the next turn with
//
//	messages.N.content.M.tool_result.content.0.text.text: Field required (400).
func TestContentBlockMarshalJSON_TextAlwaysPresent(t *testing.T) {
	t.Run("empty text block", func(t *testing.T) {
		out, err := json.Marshal(ContentBlock{Type: "text", Text: ""})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !strings.Contains(string(out), `"text":""`) {
			t.Errorf("expected `\"text\":\"\"` in output, got: %s", string(out))
		}
	})
	t.Run("tool_result with empty nested text", func(t *testing.T) {
		out, err := json.Marshal(ToolResult{ToolUseID: "toolu_9", Content: ""}.ToContentBlock())
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !strings.Contains(string(out), `"text":""`) {
			t.Errorf("expected nested `\"text\":\"\"` in tool_result, got: %s", string(out))
		}
	})
	t.Run("non-empty text unchanged", func(t *testing.T) {
		out, err := json.Marshal(ContentBlock{Type: "text", Text: "hello"})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !strings.Contains(string(out), `"text":"hello"`) {
			t.Errorf("expected text preserved, got: %s", string(out))
		}
	})
}
