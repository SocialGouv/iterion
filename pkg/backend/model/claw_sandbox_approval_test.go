package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
)

// The launcher handler decision table: what servePermissionAsk answers
// for each binding outcome. The tool is an MCP-family name - launcher
// placed - with an Execute closure that records its invocation.

func approvalHandlerTools(executed *bool) map[string]delegate.ToolDef {
	return map[string]delegate.ToolDef{
		"mcp_srv_greet": {
			Name: "mcp_srv_greet",
			Execute: func(_ context.Context, input json.RawMessage) (string, error) {
				*executed = true
				return "greeted", nil
			},
		},
	}
}

func approvalAsk(toolName, input string) delegate.PermissionAskData {
	return delegate.PermissionAskData{
		ToolName:  toolName,
		Placement: delegate.PermissionPlacementLauncher,
		Input:     json.RawMessage(input),
		Question:  "Allow mcp_srv_greet?",
	}
}

func approvalTask(t *testing.T, mode permission.Mode, allowRule bool) delegate.Task {
	t.Helper()
	var allow []string
	if allowRule {
		allow = []string{"mcp_srv_greet"}
	}
	pol, err := permission.NewPolicy(mode, allow, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return delegate.Task{NodeID: "node-1", Model: "anthropic/claude-opus-5", Permission: pol}
}

func TestServePermissionAskDecisionTable(t *testing.T) {
	t.Parallel()
	ctx := WithRunID(WithNodeID(context.Background(), "node-1"), "run-1")
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	tests := []struct {
		name       string
		mode       permission.Mode
		toolName   string
		allowRule  bool
		input      string
		claim      bool
		claimInput string
		spendWon   bool
		want       string
		wantSpent  bool
		wantExec   bool
	}{
		{name: "unregistered tool is denied", mode: permission.ModeAsk, toolName: "mcp_other", input: `{"x":1}`, want: delegate.PermissionDecisionDeny, wantSpent: false, wantExec: false},
		{name: "sandbox-placed tool is denied", mode: permission.ModeAsk, toolName: "bash", input: `{"command":"ls"}`, want: delegate.PermissionDecisionDeny, wantSpent: false, wantExec: false},
		{name: "policy allow rule executes without consuming", mode: permission.ModeDeny, allowRule: true, input: `{"x":1}`, want: delegate.PermissionDecisionAllow, wantSpent: false, wantExec: true},
		{name: "policy deny is refused", mode: permission.ModeDeny, input: `{"x":1}`, want: delegate.PermissionDecisionDeny, wantSpent: false, wantExec: false},
		{name: "matching claim spends and executes", mode: permission.ModeAsk, input: `{"x":1}`, claim: true, claimInput: `{"x":1}`, spendWon: true, want: delegate.PermissionDecisionAllow, wantSpent: true, wantExec: true},
		{name: "no claim defers to a human", mode: permission.ModeAsk, input: `{"x":1}`, want: delegate.PermissionDecisionDefer, wantSpent: false, wantExec: false},
		{name: "different input defers as a new approval", mode: permission.ModeAsk, input: `{"x":2}`, claim: true, claimInput: `{"x":1}`, want: delegate.PermissionDecisionDefer, wantSpent: false, wantExec: false},
		{name: "lost spend denies without executing", mode: permission.ModeAsk, input: `{"x":1}`, claim: true, claimInput: `{"x":1}`, spendWon: false, want: delegate.PermissionDecisionDeny, wantSpent: true, wantExec: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toolName := tc.toolName
			if toolName == "" {
				toolName = "mcp_srv_greet"
			}
			askCtx := ctx
			spent := false
			if tc.claim {
				askCtx = WithExactClawApproval(ctx, "run-1", "node-1", "mcp_srv_greet", json.RawMessage(tc.claimInput), func(context.Context) (bool, error) {
					spent = true
					return tc.spendWon, nil
				})
			}
			var executed bool
			got, err := b.servePermissionAsk(askCtx, approvalTask(t, tc.mode, tc.allowRule), approvalHandlerTools(&executed), []string{"mcp_srv_greet"}, approvalAsk(toolName, tc.input))
			if err != nil {
				t.Fatalf("handler error: %v", err)
			}
			if got.Decision != tc.want {
				t.Fatalf("decision = %q, want %q (%s)", got.Decision, tc.want, got.Error)
			}
			if spent != tc.wantSpent {
				t.Fatalf("spend called = %v, want %v", spent, tc.wantSpent)
			}
			if executed != tc.wantExec {
				t.Fatalf("executed = %v, want %v", executed, tc.wantExec)
			}
		})
	}
}

// TestServePermissionAskAllowCarriesTheRealResult: an execution failure
// is an allow whose deny slot carries the error - the tool RAN.
func TestServePermissionAskAllowCarriesTheRealResult(t *testing.T) {
	t.Parallel()
	ctx := WithRunID(WithNodeID(context.Background(), "node-1"), "run-1")
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	td := map[string]delegate.ToolDef{
		"mcp_srv_greet": {
			Name: "mcp_srv_greet",
			Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
				return "", errors.New("upstream 503")
			},
		},
	}
	pol, _ := permission.NewPolicy(permission.ModeDeny, []string{"mcp_srv_greet"}, nil, nil)
	task := delegate.Task{NodeID: "node-1", Model: "m", Permission: pol}
	got, err := b.servePermissionAsk(ctx, task, td, []string{"mcp_srv_greet"}, approvalAsk("mcp_srv_greet", `{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != delegate.PermissionDecisionAllow || !strings.Contains(got.Error, "upstream 503") {
		t.Fatalf("execution failure must stay an allow carrying the error: %+v", got)
	}
}
