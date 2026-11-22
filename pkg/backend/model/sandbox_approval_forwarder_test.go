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

// The approval channel gate side: with the runner forwarder installed,
// an Ask forwards and the decision shapes the tool_result - the tool is
// never executed on this side. The in-process ErrAskUser raise is
// untouched (permission_gate_test.go covers it).

func sandboxApprovalTools(executed *bool) map[string]*GenerationTool {
	return map[string]*GenerationTool{
		"bash": {
			Name: "bash",
			Execute: func(_ context.Context, _ json.RawMessage) (string, error) {
				*executed = true
				return "ran", nil
			},
		},
	}
}

// approvalBashCall builds the tool_use under the name the claw registry
// advertises for the shell built-in - lowercase, the name the placement
// table classifies.
func approvalBashCall(cmd string) []toolUseBlock {
	in, _ := json.Marshal(map[string]any{"command": cmd})
	return []toolUseBlock{{ID: "tu_1", Name: "bash", PartialJSON: string(in)}}
}

func askPolicy() *permission.Policy {
	pol, err := permission.NewPolicy(permission.ModeAsk, nil, nil, nil)
	if err != nil {
		panic(err)
	}
	return pol
}

// TestExecuteToolsDirect_ApprovalChannelAllowCarriesTheResult: the
// launcher allow decision carries the REAL result - the runner never
// executes the call, and the transcript gets exactly the output the
// launcher returned.
func TestExecuteToolsDirect_ApprovalChannelAllowCarriesTheResult(t *testing.T) {
	t.Parallel()
	pol := askPolicy()
	var executed bool
	var gotAsk delegate.PermissionAskData
	fwd := func(_ context.Context, ask delegate.PermissionAskData) (delegate.PermissionDecisionData, error) {
		gotAsk = ask
		return delegate.PermissionDecisionData{Decision: delegate.PermissionDecisionAllow, Output: "approved-result"}, nil
	}
	ctx := WithSandboxApprovalForwarder(context.Background(), fwd)
	results, gErr := executeToolsDirect(ctx, approvalBashCall("echo hi"), sandboxApprovalTools(&executed), nil, nil, nil, nil, pol)
	if gErr != nil {
		t.Fatalf("unexpected error: %v", gErr)
	}
	if executed {
		t.Error("the runner must not execute the call the launcher already executed")
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	raw, _ := json.Marshal(results[0])
	if !strings.Contains(string(raw), "approved-result") {
		t.Errorf("tool_result must carry the launcher output: %s", raw)
	}
	if gotAsk.ToolName != "bash" {
		t.Errorf("ask tool = %q, want bash", gotAsk.ToolName)
	}
	if gotAsk.Placement != delegate.PermissionPlacementSandbox {
		t.Errorf("ask placement = %q, want sandbox", gotAsk.Placement)
	}
	if string(gotAsk.Input) != `{"command":"echo hi"}` {
		t.Errorf("ask input = %s, want the exact canonical bytes", gotAsk.Input)
	}
}

// TestExecuteToolsDirect_ApprovalChannelDenyRendersRefusal: a deny is a
// refusal tool_result the model can adapt to, not a node failure.
func TestExecuteToolsDirect_ApprovalChannelDenyRendersRefusal(t *testing.T) {
	t.Parallel()
	pol := askPolicy()
	var executed bool
	fwd := func(_ context.Context, _ delegate.PermissionAskData) (delegate.PermissionDecisionData, error) {
		return delegate.PermissionDecisionData{Decision: delegate.PermissionDecisionDeny, Error: "denied by the operator"}, nil
	}
	ctx := WithSandboxApprovalForwarder(context.Background(), fwd)
	results, gErr := executeToolsDirect(ctx, approvalBashCall("curl http://x"), sandboxApprovalTools(&executed), nil, nil, nil, nil, pol)
	if gErr != nil {
		t.Fatalf("deny must not fail the node: %v", gErr)
	}
	if executed {
		t.Error("denied tool must NOT execute")
	}
	raw, _ := json.Marshal(results[0])
	if !strings.Contains(string(raw), "denied by the operator") || !strings.Contains(string(raw), "is_error") {
		t.Errorf("deny must render an isError refusal naming the reason: %s", raw)
	}
}

// TestExecuteToolsDirect_ApprovalChannelFailureAborts: a channel failure
// mid-ask aborts the loop - the call never executed, so there is no
// half-executed state, and the node fails with the reason.
func TestExecuteToolsDirect_ApprovalChannelFailureAborts(t *testing.T) {
	t.Parallel()
	pol := askPolicy()
	var executed bool
	fwd := func(_ context.Context, _ delegate.PermissionAskData) (delegate.PermissionDecisionData, error) {
		return delegate.PermissionDecisionData{}, errors.New("channel closed mid-ask")
	}
	ctx := WithSandboxApprovalForwarder(context.Background(), fwd)
	_, gErr := executeToolsDirect(ctx, approvalBashCall("echo hi"), sandboxApprovalTools(&executed), nil, nil, nil, nil, pol)
	if gErr == nil || !strings.Contains(gErr.Error(), "channel closed mid-ask") {
		t.Fatalf("channel failure must abort the loop with the reason, got %v", gErr)
	}
	if executed {
		t.Error("the call never got a decision - it must not execute")
	}
}

// TestExecuteToolsDirect_ApprovalChannelUnknownDecisionAborts: anything
// that is not allow/deny is a protocol breach on the decision, refused
// like any other unknown traffic.
func TestExecuteToolsDirect_ApprovalChannelUnknownDecisionAborts(t *testing.T) {
	t.Parallel()
	pol := askPolicy()
	var executed bool
	fwd := func(_ context.Context, _ delegate.PermissionAskData) (delegate.PermissionDecisionData, error) {
		return delegate.PermissionDecisionData{Decision: "maybe"}, nil
	}
	ctx := WithSandboxApprovalForwarder(context.Background(), fwd)
	_, gErr := executeToolsDirect(ctx, approvalBashCall("echo hi"), sandboxApprovalTools(&executed), nil, nil, nil, nil, pol)
	if gErr == nil || !strings.Contains(gErr.Error(), "unknown decision") {
		t.Fatalf("unknown decision must abort, got %v", gErr)
	}
}
