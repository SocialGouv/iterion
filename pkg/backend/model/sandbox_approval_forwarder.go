package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// The approval channel's runner side (#3513): the context-carried
// forwarder the in-container runner installs, which the shared permission
// gate consults on an Ask verdict. The in-process path never installs it,
// so in-process pauses (#3510) are untouched; a runner without the
// announcement never installs it either, and the bootstrap twin
// (claw_runner.go) already refused that case before any model call.
//
// The forwarder emits the runner's permission_ask envelope and blocks
// pending the launcher's permission_decision. The decision carries the
// REAL tool result: the launcher consumed the approval and executed the
// call itself, so the runner appends the result to the transcript and
// never executes the call — and the approval never reaches any
// PolicyConfig.

// sandboxApprovalForwarderKey is the context key for the runner-installed
// forwarder. Unexported: only the runner installs it, and the gate reads
// it back.
type sandboxApprovalForwarderKey struct{}

// SandboxApprovalForwarder carries one permission Ask across the sandbox
// IPC. Errors abort the tool loop: the ask never reached a decision and
// the call never executed, so there is nothing to shape — the node fails
// with the reason.
type SandboxApprovalForwarder func(ctx context.Context, ask delegate.PermissionAskData) (delegate.PermissionDecisionData, error)

// WithSandboxApprovalForwarder installs the runner's approval forwarder.
// The gate consults it ONLY in the runner process.
func WithSandboxApprovalForwarder(ctx context.Context, fwd SandboxApprovalForwarder) context.Context {
	return context.WithValue(ctx, sandboxApprovalForwarderKey{}, fwd)
}

func sandboxApprovalForwarderFrom(ctx context.Context) SandboxApprovalForwarder {
	fwd, _ := ctx.Value(sandboxApprovalForwarderKey{}).(SandboxApprovalForwarder)
	return fwd
}

// sandboxApprovalAskOutcome is the gate's Ask outcome when the approval
// channel is announced: forward the captured exact call and block for the
// decision. Allow carries the real tool result; deny renders the usual
// refusal tool_result the model can adapt to; a channel failure or an
// unknown decision aborts the loop — the call never executed, so there is
// no half-executed state to report, and the node fails with the reason.
func sandboxApprovalAskOutcome(ctx context.Context, fwd SandboxApprovalForwarder, tu toolUseBlock, hookInput map[string]any, rule string, onToolCall func(ToolCallInfo)) (*api.ContentBlock, error) {
	exact, err := permission.CanonicalApprovalInput(json.RawMessage(tu.PartialJSON))
	if err != nil {
		return refusalOutcome("cannot request approval: "+err.Error(), tu, onToolCall), nil
	}
	placement, _ := tool.SandboxPlacementOf(tu.Name)
	decision, err := fwd(ctx, delegate.PermissionAskData{
		ToolName:  tu.Name,
		Placement: placement.String(),
		Input:     exact,
		Question:  permission.AskPrompt(tu.Name, hookInput, rule),
		Marker:    permission.Marker(tu.Name, hookInput, rule),
	})
	if err != nil {
		return nil, fmt.Errorf("permission ask for %s: %w", tu.Name, err)
	}
	switch decision.Decision {
	case delegate.PermissionDecisionAllow:
		content, isErr := decision.Output, decision.Error != ""
		if isErr {
			content = decision.Error
		}
		block := api.ToolResult{ToolUseID: tu.ID, Content: content, IsError: isErr}.ToContentBlock()
		if onToolCall != nil {
			var callErr error
			if isErr {
				callErr = errors.New(decision.Error)
			}
			info := toolCallInfo(tu)
			info.Output = decision.Output
			info.Error = callErr
			onToolCall(info)
		}
		return &block, nil
	case delegate.PermissionDecisionDeny:
		return refusalOutcome(decision.Error, tu, onToolCall), nil
	case delegate.PermissionDecisionDefer:
		// The launcher is suspending the RUN for this approval: raise the
		// same pause the in-process gate raises. It flows back through the
		// terminal result to the engine's checkpoint; the human answers on
		// resume and the re-asked approval is served from the held claim.
		// The pause prompt is LAUNCHER-COMPOSED — the decision carries the
		// prompt this side regenerated from its own policy verdict, and
		// display wording never binds the authorization (tool+input do).
		// The local prompt is only the fallback for a launcher that sent
		// no words at all.
		question := decision.Error
		if question == "" {
			question = permission.AskPrompt(tu.Name, hookInput, rule)
		}
		return nil, &delegate.ErrAskUser{
			Question:         question,
			PendingToolUseID: tu.ID,
			PermissionMarker: permission.Marker(tu.Name, hookInput, rule),
			PermissionInput:  exact,
		}
	default:
		return nil, fmt.Errorf("permission ask for %s: the launcher returned the unknown decision %q", tu.Name, decision.Decision)
	}
}
