package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
)

// The launcher side of the approval channel (#3513): the handler behind
// MultiplexerHandler.OnPermissionAsk. It binds the candidate the runner
// forwarded against THIS side's truth - the registered tool identity, the
// placement, the current policy verdict on the exact call, and any held
// approval claim - and answers:
//
//   - policy Allow (an allow rule or an operator grant): execute, nothing
//     consumed - the runner's ask was just narrower than this side's gate.
//   - policy Deny: the refusal, no dispatch.
//   - policy Ask with a matching held claim (the human already approved
//     this exact call on resume): spend, execute, answer allow with the
//     REAL result. The approval never reaches any PolicyConfig.
//   - policy Ask without one: defer - the runner raises the pause and a
//     human answers on resume.

func permissionDecisionDeny(format string, args ...any) delegate.PermissionDecisionData {
	return delegate.PermissionDecisionData{Decision: delegate.PermissionDecisionDeny, Error: fmt.Sprintf(format, args...)}
}

func permissionDecisionDefer(reason string) delegate.PermissionDecisionData {
	return delegate.PermissionDecisionData{Decision: delegate.PermissionDecisionDefer, Error: reason}
}

func (b *ClawBackend) servePermissionAsk(ctx context.Context, task delegate.Task, toolByName map[string]delegate.ToolDef, toolNames []string, ask delegate.PermissionAskData) (delegate.PermissionDecisionData, error) {
	// Bind to the registered tool identity: the runner forwards the name
	// the launcher advertised, so anything else is not one an honest
	// runner sends (the same rule OnToolCall applies).
	td, ok := toolByName[ask.ToolName]
	if !ok {
		return permissionDecisionDeny("tool %q was not advertised to this node", ask.ToolName), nil
	}
	// This slice serves launcher-placed tools only: the launcher consumes
	// the approval AND executes the call, so it must own the execution -
	// it cannot certify a container-local one.
	if placement, reason := tool.SandboxPlacementOf(td.Name); placement != tool.PlacementLauncher {
		return permissionDecisionDeny("tool %q executes in the container (%s); a sandbox-placed approval cannot be served by the launcher", td.Name, reason), nil
	}
	// The policy verdict on THIS exact call, evaluated on this side - the
	// runner's verdict is a request, never the answer.
	var args map[string]any
	if len(ask.Input) > 0 {
		if err := json.Unmarshal(ask.Input, &args); err != nil {
			return permissionDecisionDeny("decode input for the permission gate: %v", err), nil
		}
	}
	dec, rule, spelling := evaluateForwardedCall(task.Permission, forwardedToolSpellings(td), args)
	switch dec {
	case permission.Allow:
		return b.executeApprovedForwardedCall(ctx, task, td, toolNames, ask)
	case permission.Deny:
		return permissionDecisionDeny("%s", permission.DenyMessage(spelling, args, rule)), nil
	}
	// Ask: a matching held claim spends and executes; otherwise the run
	// suspends for a human.
	exact, err := permission.CanonicalApprovalInput(ask.Input)
	if err != nil {
		return permissionDecisionDeny("cannot bind the approval: %v", err), nil
	}
	a := exactClawApprovalFrom(ctx)
	if a == nil || a.toolName != td.Name || !bytes.Equal(a.input, exact) {
		return permissionDecisionDefer(permission.AskPrompt(td.Name, args, rule)), nil
	}
	won, err := a.spend(ctx)
	if err != nil {
		return permissionDecisionDeny("could not record approval consumption: %v", err), nil
	}
	if !won {
		return permissionDecisionDeny("approval is stale or already consumed; the tool was not dispatched"), nil
	}
	return b.executeApprovedForwardedCall(ctx, task, td, toolNames, ask)
}

// executeApprovedForwardedCall runs the call on the launcher, through the
// same ToolDef closure a forwarded tool_call uses, and answers allow with
// the real result - an execution failure is an allow whose deny slot
// carries the error (the tool RAN and failed; the model reads the error
// result either way).
func (b *ClawBackend) executeApprovedForwardedCall(ctx context.Context, task delegate.Task, td delegate.ToolDef, toolNames []string, ask delegate.PermissionAskData) (delegate.PermissionDecisionData, error) {
	if td.Execute == nil {
		return permissionDecisionDeny("tool %q has no Execute closure (engine misconfiguration)", td.Name), nil
	}
	output, err := td.Execute(tool.WithNodeExecutableTools(ctx, toolNames), ask.Input)
	if err != nil {
		return delegate.PermissionDecisionData{Decision: delegate.PermissionDecisionAllow, Error: err.Error()}, nil
	}
	return delegate.PermissionDecisionData{Decision: delegate.PermissionDecisionAllow, Output: output}, nil
}
