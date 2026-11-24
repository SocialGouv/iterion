package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
)

type exactDispatchApproval struct {
	runID, nodeID, toolName string
	input                   json.RawMessage
	spend                   func(context.Context) (bool, error)
}

type exactApprovalKey struct{}

// approvedDispatchInterrupted prevents automatic replay when the approved
// dispatch has started but its continuation has not been recorded.
type approvedDispatchInterrupted struct{ cause error }

func (e *approvedDispatchInterrupted) Error() string {
	return "claw: approved action may have taken effect; automatic replay is refused: " + e.cause.Error()
}
func (e *approvedDispatchInterrupted) Unwrap() error         { return e.cause }
func (e *approvedDispatchInterrupted) AmbiguousEffect() bool { return true }

// WithExactClawApproval binds a runtime-owned approval to one execution.
func WithExactClawApproval(ctx context.Context, runID, nodeID, toolName string, input json.RawMessage, spend func(context.Context) (bool, error)) context.Context {
	return context.WithValue(ctx, exactApprovalKey{}, &exactDispatchApproval{runID: runID, nodeID: nodeID, toolName: toolName, input: bytes.Clone(input), spend: spend})
}

// WithoutExactClawApproval keeps the authorization out of subsequent calls.
func WithoutExactClawApproval(ctx context.Context) context.Context {
	return context.WithValue(ctx, exactApprovalKey{}, (*exactDispatchApproval)(nil))
}

func exactClawApprovalFrom(ctx context.Context) *exactDispatchApproval {
	a, _ := ctx.Value(exactApprovalKey{}).(*exactDispatchApproval)
	if a == nil || a.runID != RunIDFromContext(ctx) || a.nodeID != NodeIDFromContext(ctx) {
		return nil
	}
	return a
}

func (a *exactDispatchApproval) authorize(ctx context.Context, tu toolUseBlock, gt *GenerationTool, policy *permission.Policy, input map[string]any, materialize func(string) string) error {
	raw := json.RawMessage(tu.PartialJSON)
	exact, err := permission.CanonicalApprovalInput(raw)
	approved, approvedErr := permission.CanonicalApprovalInput(a.input)
	if err != nil || approvedErr != nil || !bytes.Equal(exact, approved) || tu.Name != gt.Name || tu.Name != a.toolName || a.spend == nil {
		return fmt.Errorf("claw: approval does not match the registered tool call")
	}
	if verdict, _ := policy.Evaluate(gt.Name, input); verdict != permission.Ask {
		return fmt.Errorf("claw: approved call no longer requires approval under the current policy")
	}
	if materialize != nil && !keepsPlaceholders(tu.Name) && !bytes.Equal(raw, secretguard.MaterializeJSON(raw, materialize)) {
		return fmt.Errorf("claw: exact approval cannot dispatch dynamically materialized arguments")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	won, err := a.spend(ctx)
	if err != nil {
		return fmt.Errorf("claw: could not record approval consumption: %w", err)
	}
	if !won {
		return fmt.Errorf("claw: approval is stale or already consumed; the tool was not dispatched")
	}
	return nil
}

type pendingApprovalCall struct {
	Name  string
	Input json.RawMessage
}

// pendingClawApproval resolves only a single call in the final assistant turn.
// Provider IDs correlate the transcript; an earlier occurrence makes the
// continuation ambiguous even if the final block has the expected ID.
func pendingClawApproval(conversation json.RawMessage, id string) (pendingApprovalCall, error) {
	var turns []struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			ToolUseID string          `json:"tool_use_id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(conversation, &turns); err != nil {
		return pendingApprovalCall{}, fmt.Errorf("decode approval conversation: %w", err)
	}
	if id == "" || len(turns) == 0 || turns[len(turns)-1].Role != "assistant" {
		return pendingApprovalCall{}, fmt.Errorf("approval does not identify a pending assistant turn")
	}
	var call pendingApprovalCall
	for i, turn := range turns {
		for _, block := range turn.Content {
			if block.Type == "tool_result" && block.ToolUseID == id {
				return pendingApprovalCall{}, fmt.Errorf("approval tool call already has a result")
			}
			if i < len(turns)-1 {
				if block.Type == "tool_use" && block.ID == id {
					return pendingApprovalCall{}, fmt.Errorf("approval tool-use ID also occurs in an earlier turn")
				}
				continue
			}
			if block.Type != "tool_use" {
				continue
			}
			if block.ID != id || block.Name == "" || call.Name != "" {
				return pendingApprovalCall{}, fmt.Errorf("approval requires one matching tool call in the pending turn")
			}
			input, err := permission.CanonicalApprovalInput(block.Input)
			if err != nil {
				return pendingApprovalCall{}, fmt.Errorf("validate pending tool call: %w", err)
			}
			call = pendingApprovalCall{Name: block.Name, Input: input}
		}
	}
	if call.Name == "" {
		return pendingApprovalCall{}, fmt.Errorf("approval tool call is missing from the pending turn")
	}
	return call, nil
}
