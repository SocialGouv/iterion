package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

func TestClawApprovalCapturePreservesDispatchInput(t *testing.T) {
	t.Parallel()
	policy, err := permission.NewPolicy(permission.ModeAsk, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := toolUseBlock{ID: "choice", Name: "choose_label", PartialJSON: `{"weight":9007199254740993}`}
	tools := map[string]*GenerationTool{call.Name: {
		Name: call.Name,
		Execute: func(context.Context, json.RawMessage) (string, error) {
			t.Fatal("tool ran before approval")
			return "", nil
		},
	}}
	ctx := context.Background()
	_, err = executeToolsDirect(ctx, []toolUseBlock{call}, tools, nil, nil, nil, nil, policy)
	var ask *delegate.ErrAskUser
	if !errors.As(err, &ask) {
		t.Fatalf("expected permission pause, got %v", err)
	}
	messages := []api.Message{assistantToolUseMessage("", []toolUseBlock{call})}
	if err := stashPauseConversation(ctx, ask, messages, GenerationOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := ask.PermissionMarker["exact_input_json"]; got != call.PartialJSON {
		t.Fatalf("displayed input = %v, want %s", got, call.PartialJSON)
	}
	pending, err := pendingClawApproval(ask.Conversation, ask.PendingToolUseID)
	if err != nil {
		t.Fatal(err)
	}
	if string(pending.Input) != call.PartialJSON || string(ask.PermissionInput) != call.PartialJSON {
		t.Fatalf("pending=%s captured=%s, want %s", pending.Input, ask.PermissionInput, call.PartialJSON)
	}
}

func TestClawApprovalCaptureRefusesArgumentsRequiringRedaction(t *testing.T) {
	t.Parallel()
	call := toolUseBlock{ID: "choice", Name: "choose_label", PartialJSON: `{"label":"cyan"}`}
	ask := &delegate.ErrAskUser{
		PendingToolUseID: call.ID,
		PermissionMarker: permission.Marker(call.Name, nil, ""),
		PermissionInput:  json.RawMessage(call.PartialJSON),
	}
	opts := GenerationOptions{RedactApprovalText: func(string) string { return "[redacted]" }}
	err := stashPauseConversation(context.Background(), ask, []api.Message{assistantToolUseMessage("", []toolUseBlock{call})}, opts)
	if err == nil || errors.As(err, new(*delegate.ErrAskUser)) {
		t.Fatalf("expected a refusal rather than a human approval pause, got %v", err)
	}
}

func TestClawApprovalCaptureRejectsInconsistentTranscript(t *testing.T) {
	t.Parallel()
	call := toolUseBlock{ID: "choice", Name: "choose_label", PartialJSON: `{"label":"cyan"}`}
	for _, tc := range []struct{ name, tool, input string }{
		{"different tool", "inspect_label", call.PartialJSON},
		{"different input", call.Name, `{"label":"blue"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ask := &delegate.ErrAskUser{
				PendingToolUseID: call.ID,
				PermissionMarker: permission.Marker(tc.tool, nil, ""),
				PermissionInput:  json.RawMessage(tc.input),
			}
			if err := stashPauseConversation(context.Background(), ask, []api.Message{assistantToolUseMessage("", []toolUseBlock{call})}, GenerationOptions{}); err == nil {
				t.Fatal("inconsistent transcript was accepted for approval")
			}
		})
	}
}

func TestClawBackendDispatchesExactApprovalOnce(t *testing.T) {
	t.Parallel()
	b, captured := newCapturingBackend()
	ctx := WithNodeID(WithRunID(context.Background(), "run"), "agent")
	const input = `{"label":"cyan"}`
	spent, calls := false, 0
	ctx = WithExactClawApproval(ctx, "run", "agent", "choose_label", json.RawMessage(input), func(context.Context) (bool, error) {
		if spent {
			return false, nil
		}
		spent = true
		return true, nil
	})
	policy, err := permission.NewPolicy(permission.ModeAsk, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task := delegate.Task{
		NodeID: "agent", Model: "test/test-model", Permission: policy, HasTools: true,
		ResumeConversation:     json.RawMessage(`[{"role":"assistant","content":[{"type":"tool_use","id":"choice","name":"choose_label","input":` + input + `}]}]`),
		ResumePendingToolUseID: "choice", ResumeAnswer: "allow once",
		ToolDefs: []delegate.ToolDef{{Name: "choose_label", Execute: func(callCtx context.Context, raw json.RawMessage) (string, error) {
			if !spent || string(raw) != input || exactClawApprovalFrom(callCtx) != nil {
				t.Fatal("dispatch did not receive exactly the authorized input after consumption")
			}
			calls++
			return "label selected", nil
		}}},
	}
	if _, err := b.Execute(ctx, task); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(captured.requests) != 1 {
		t.Fatalf("tool calls = %d, model requests = %d, want one each", calls, len(captured.requests))
	}
	messages := captured.requests[0].Messages
	blocks := messages[len(messages)-1].Content
	if len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "choice" || len(blocks[0].Content) != 1 || blocks[0].Content[0].Text != "label selected" {
		t.Fatalf("model did not receive the actual tool result: %+v", blocks)
	}
	if _, err := b.Execute(ctx, task); err == nil || calls != 1 || len(captured.requests) != 1 {
		t.Fatalf("spent approval was not stopped before dispatch: calls=%d, requests=%d, err=%v", calls, len(captured.requests), err)
	}
}

// The approved call's verdict reaches the model either way: an ordinary tool
// error becomes an is_error tool_result and the generation continues — only
// a dispatch that never returned (cancelled context) is ambiguous.
func TestClawApprovedToolOutcomeShapesItsResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		toolErr error
	}{
		{"ordinary tool error", errors.New("no such file")},
		{"cancelled dispatch", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, captured := newCapturingBackend()
			ctx := WithNodeID(WithRunID(context.Background(), "run"), "agent")
			const input = `{"label":"cyan"}`
			spent := false
			ctx = WithExactClawApproval(ctx, "run", "agent", "choose_label", json.RawMessage(input), func(context.Context) (bool, error) {
				if spent {
					return false, nil
				}
				spent = true
				return true, nil
			})
			policy, err := permission.NewPolicy(permission.ModeAsk, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			task := delegate.Task{
				NodeID: "agent", Model: "test/test-model", Permission: policy, HasTools: true,
				ResumeConversation:     json.RawMessage(`[{"role":"assistant","content":[{"type":"tool_use","id":"choice","name":"choose_label","input":` + input + `}]}]`),
				ResumePendingToolUseID: "choice", ResumeAnswer: "allow once",
				ToolDefs: []delegate.ToolDef{{Name: "choose_label", Execute: func(context.Context, json.RawMessage) (string, error) {
					if !spent {
						t.Fatal("dispatch ran before consumption")
					}
					return "", tc.toolErr
				}}},
			}
			_, err = b.Execute(ctx, task)
			if tc.toolErr == context.Canceled {
				var amb interface{ AmbiguousEffect() bool }
				if !errors.As(err, &amb) || !amb.AmbiguousEffect() {
					t.Fatalf("cancelled dispatch = %v, want an ambiguous effect", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ordinary tool error failed the generation: %v", err)
			}
			messages := captured.requests[0].Messages
			blocks := messages[len(messages)-1].Content
			if len(blocks) != 1 || blocks[0].Type != "tool_result" || !blocks[0].IsError || len(blocks[0].Content) != 1 || !strings.Contains(blocks[0].Content[0].Text, "no such file") {
				t.Fatalf("model did not receive the tool's error result: %+v", blocks)
			}
		})
	}
}

func TestPendingClawApproval(t *testing.T) {
	t.Parallel()
	const call = `{"type":"tool_use","id":"a","name":"choose_label","input":{"weight":9007199254740993,"name":"cyan"}}`
	const final = `{"role":"assistant","content":[` + call + `]}`
	for _, tc := range []struct {
		name, transcript, id string
		valid                bool
	}{
		{"sole pending call", `[` + final + `]`, "a", true},
		{"unknown id", `[` + final + `]`, "b", false},
		{"empty id", `[` + final + `]`, "", false},
		{"previous occurrence", `[` + final + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"}]},` + final + `]`, "a", false},
		{"already answered", `[` + final + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"a"}]}]`, "a", false},
		{"result in pending turn", `[{"role":"assistant","content":[` + call + `,{"type":"tool_result","tool_use_id":"a"}]}]`, "a", false},
		{"multiple pending calls", `[{"role":"assistant","content":[` + call + `,{"type":"tool_use","id":"b","name":"choose_label","input":{}}]}]`, "a", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pendingClawApproval(json.RawMessage(tc.transcript), tc.id)
			if !tc.valid {
				if err == nil {
					t.Fatal("expected the approval candidate to be rejected")
				}
				return
			}
			if err != nil || got.Name != "choose_label" || string(got.Input) != `{"name":"cyan","weight":9007199254740993}` {
				t.Fatalf("pending approval = (%q, %s), error = %v", got.Name, got.Input, err)
			}
		})
	}
}
