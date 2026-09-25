package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

func TestOpus55StructuredAutoTool(t *testing.T) {
	for _, tc := range []struct {
		name, tool, payload string
		wantError           bool
	}{
		{"dynamic JSON", "result", `{"payload":{"unexpected":[{"nested":true}]}}`, false},
		{"wrong tool", "other", `{}`, true},
		{"invalid JSON", "result", `{`, true},
		{"plain text", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := toolUseEvents("t1", tc.tool, tc.payload, 11, 7)
			if tc.tool == "" {
				events = textEvents("{}", 11, 7)
			}
			client := newMockClient(events)
			out, err := GenerateObjectDirect[map[string]any](context.Background(), client, GenerationOptions{
				Model: "anthropic/claude-opus-5-5", SchemaName: "result",
				ExplicitSchema: json.RawMessage(`{"type":"object","properties":{"payload":{"type":["object","array","string","number","boolean","null"]}}}`),
				Messages:       []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Return the payload"}}}},
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v, wantError=%v", err, tc.wantError)
			}
			if out == nil || out.TotalUsage.OutputTokens != 7 {
				t.Fatalf("lost billed usage: %+v", out)
			}
			req := client.getCalls()[0]
			if req.ToolChoice != nil || req.Thinking != nil {
				t.Fatalf("incompatible controls: choice=%+v thinking=%+v", req.ToolChoice, req.Thinking)
			}
			if len(req.Tools) != 1 || req.Tools[0].Name != "result" {
				t.Fatal("lost synthetic tool")
			}
			if !strings.Contains(req.Messages[len(req.Messages)-1].Content[0].Text, `"result"`) {
				t.Fatal("missing final instruction naming tool")
			}
			if !tc.wantError && out.Object["payload"] == nil {
				t.Fatal("lost arbitrary JSON payload")
			}
		})
	}
}

func TestOpus55RequiresExecutedInitialTool(t *testing.T) {
	announced := toolUseEvents("t1", "read", `{}`, 11, 7)
	for i := range announced {
		if announced[i].Type == api.EventMessageDelta {
			announced[i].StopReason = "end_turn"
		}
	}
	for _, tc := range []struct {
		name      string
		first     []api.StreamEvent
		maxSteps  int
		wantError bool
	}{
		{"no tool", textEvents("answer", 11, 7), 1, true},
		{"announced only", announced, 1, true},
		{"unknown", toolUseEvents("t1", "unknown", `{}`, 11, 7), 1, true},
		{"malformed", toolUseEvents("t1", "read", `{`, 11, 7), 1, true},
		{"actual execution", toolUseEvents("t1", "read", `{}`, 11, 7), 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newMockClient(tc.first, textEvents("observed answer", 11, 7))
			executed, observed := 0, 0
			out, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
				Model: "claude-opus-5-5", MaxSteps: tc.maxSteps, ForceInitialToolUse: true,
				Messages:      []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Review evidence"}}}},
				Tools:         []GenerationTool{{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { executed++; return "evidence", nil }}},
				OnToolStarted: func(ToolCallInfo) { observed++ },
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v, wantError=%v", err, tc.wantError)
			}
			if out == nil || out.TotalUsage.OutputTokens == 0 {
				t.Fatal("lost partial usage")
			}
			if observed != executed {
				t.Fatalf("observer=%d execution=%d", observed, executed)
			}
			if !tc.wantError && executed != 1 {
				t.Fatalf("executions=%d", executed)
			}
			if req := client.getCalls()[0]; req.ToolChoice != nil || req.Thinking != nil {
				t.Fatalf("incompatible request controls: %+v %+v", req.ToolChoice, req.Thinking)
			}
		})
	}
}
