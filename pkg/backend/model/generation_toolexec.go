package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
)

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

// executeToolsDirect runs each tool_use block and builds tool_result content blocks.
//
// When runner is non-nil, the function fires PreToolUse before each
// Execute (a Block decision short-circuits to a synthetic refusal
// tool_result carrying the decision Reason), then either PostToolUse
// (success) or PostToolUseFailure (error) afterwards.
//
// A non-nil error return signals that the tool loop must abort and the
// caller should propagate the error up. The only case currently using
// this is *delegate.ErrAskUser (claw-code-go's native ask_user tool
// asking iterion to pause the run and surface the question to the dev).
// In every other failure mode the error is rendered into an isError=true
// tool_result and execution continues, so the LLM can recover.
func executeToolsDirect(
	ctx context.Context,
	toolUses []toolUseBlock,
	toolMap map[string]*GenerationTool,
	onToolStarted func(ToolCallInfo),
	onToolCall func(ToolCallInfo),
	runner *hooks.Runner,
	materialize func(string) string,
	policy *permission.Policy,
) ([]api.ContentBlock, error) {
	results := make([]api.ContentBlock, 0, len(toolUses))

	for _, tu := range toolUses {
		block, abort := executeOneTool(ctx, tu, toolMap, onToolStarted, onToolCall, runner, materialize, policy)
		if abort != nil {
			return results, abort
		}
		results = append(results, block)
	}

	return results, nil
}

// executeOneTool runs a single tool_use block through the full pipeline —
// lookup, input validation, lifecycle PreToolUse, permission gate,
// execution, and result shaping. Every failure mode is rendered into the
// returned isError tool_result so the LLM can recover; a non-nil abort
// error (currently only *delegate.ErrAskUser, from the native ask_user
// tool or a permission-gate Ask) signals the tool loop must stop and
// propagate it, discarding the block.
func executeOneTool(
	ctx context.Context,
	tu toolUseBlock,
	toolMap map[string]*GenerationTool,
	onToolStarted func(ToolCallInfo),
	onToolCall func(ToolCallInfo),
	runner *hooks.Runner,
	materialize func(string) string,
	policy *permission.Policy,
) (api.ContentBlock, error) {
	gt, ok := lookupGenerationTool(toolMap, tu.Name)
	if !ok {
		return unknownToolResult(tu, onToolCall), nil
	}

	// Validate that PartialJSON is well-formed JSON before either
	// firing hooks with stale/empty input or invoking Execute with
	// a payload its decoder can't parse.
	var hookInput map[string]any
	if jsonErr := json.Unmarshal([]byte(tu.PartialJSON), &hookInput); jsonErr != nil {
		return malformedInputResult(tu, jsonErr, onToolCall), nil
	}

	if refusal := preToolUseRefusal(ctx, runner, tu, hookInput, onToolCall); refusal != nil {
		return *refusal, nil
	}

	if refusal, abort := permissionGateOutcome(policy, tu, hookInput, onToolCall); abort != nil {
		return api.ContentBlock{}, abort
	} else if refusal != nil {
		return *refusal, nil
	}

	output, err := runToolExecution(ctx, gt, tu, materialize, onToolStarted, onToolCall)
	return shapeToolOutcome(ctx, runner, tu, hookInput, output, err)
}

// lookupGenerationTool resolves a tool_use name against the tool map. A
// bot prompt may name an MCP/board tool in the claude_code
// double-underscore FQN convention ("mcp__server__tool") while the claw
// in-process loop advertises it sanitized ("mcp_server_tool"); the second
// lookup bridges the two so the call dispatches.
func lookupGenerationTool(toolMap map[string]*GenerationTool, name string) (*GenerationTool, bool) {
	gt, ok := toolMap[name]
	if !ok {
		gt, ok = toolMap[canonicalMCPToolName(name)]
	}
	return gt, ok
}

// unknownToolResult renders the isError tool_result for a call naming a
// tool the map cannot resolve, and reports it on onToolCall. Quirk pinned
// by the characterization tests: unlike the other refusal paths, the
// callback info carries no ToolUseID here.
func unknownToolResult(tu toolUseBlock, onToolCall func(ToolCallInfo)) api.ContentBlock {
	block := api.ToolResult{
		ToolUseID: tu.ID,
		Content:   fmt.Sprintf("unknown tool: %s", tu.Name),
		IsError:   true,
	}.ToContentBlock()
	if onToolCall != nil {
		onToolCall(ToolCallInfo{
			ToolName:  tu.Name,
			InputSize: len(tu.PartialJSON),
			Error:     fmt.Errorf("unknown tool: %s", tu.Name),
		})
	}
	return block
}

// malformedInputResult renders the isError tool_result for a tool_use
// whose PartialJSON does not parse — either a truncated stream the
// upstream aggregateStream missed, or a provider that emits invalid JSON.
// Both warrant a tool_result-isError, not a silent empty-args call that
// the LLM would never recover from cleanly.
func malformedInputResult(tu toolUseBlock, jsonErr error, onToolCall func(ToolCallInfo)) api.ContentBlock {
	block := api.ToolResult{
		ToolUseID: tu.ID,
		Content:   fmt.Sprintf("malformed tool input: %v", jsonErr),
		IsError:   true,
	}.ToContentBlock()
	if onToolCall != nil {
		onToolCall(ToolCallInfo{
			ToolName:  tu.Name,
			InputSize: len(tu.PartialJSON),
			ToolUseID: tu.ID,
			Error:     fmt.Errorf("malformed tool input: %w", jsonErr),
		})
	}
	return block
}

// preToolUseRefusal fires the PreToolUse lifecycle hook and, on a Block
// decision, renders the synthetic refusal tool_result carrying the
// decision Reason (defaulting to "blocked by lifecycle hook"). A nil
// return means the call may proceed. Like the unknown-tool path, the
// callback info carries no ToolUseID (quirk pinned by the
// characterization tests).
func preToolUseRefusal(ctx context.Context, runner *hooks.Runner, tu toolUseBlock, hookInput map[string]any, onToolCall func(ToolCallInfo)) *api.ContentBlock {
	dec, _ := runner.Fire(ctx, hooks.Context{
		Event:     hooks.PreToolUse,
		ToolName:  tu.Name,
		ToolInput: hookInput,
	})
	if dec.Action != hooks.ActionBlock {
		return nil
	}
	reason := dec.Reason
	if reason == "" {
		reason = "blocked by lifecycle hook"
	}
	block := api.ToolResult{
		ToolUseID: tu.ID,
		Content:   fmt.Sprintf("tool refused: %s", reason),
		IsError:   true,
	}.ToContentBlock()
	if onToolCall != nil {
		onToolCall(ToolCallInfo{
			ToolName:  tu.Name,
			InputSize: len(tu.PartialJSON),
			Error:     fmt.Errorf("blocked by hook: %s", reason),
		})
	}
	return &block
}

// permissionGateOutcome evaluates the tool-permission gate (the
// anti-prompt-injection boundary). Evaluated AFTER the lifecycle hook (so
// hooks still observe every call) and BEFORE execution. Deny → a synthetic
// refusal tool_result the model can adapt to. Ask → a *delegate.ErrAskUser
// abort so the run pauses for human approval (mirrors claude_code's
// PreToolUse permission hook for cross-backend parity). Allow — and a
// nil/disabled policy — returns (nil, nil): fall through to execution.
func permissionGateOutcome(policy *permission.Policy, tu toolUseBlock, hookInput map[string]any, onToolCall func(ToolCallInfo)) (*api.ContentBlock, error) {
	if !policy.Enabled() {
		return nil, nil
	}
	switch dec, rule := policy.Evaluate(tu.Name, hookInput); dec {
	case permission.Deny:
		reason := permission.DenyMessage(tu.Name, hookInput, rule)
		block := api.ToolResult{
			ToolUseID: tu.ID,
			Content:   reason,
			IsError:   true,
		}.ToContentBlock()
		if onToolCall != nil {
			onToolCall(ToolCallInfo{
				ToolName:  tu.Name,
				InputSize: len(tu.PartialJSON),
				ToolUseID: tu.ID,
				Error:     errors.New(reason),
			})
		}
		return &block, nil
	case permission.Ask:
		// Suspend the run for operator approval. The captured
		// question guides the model after the operator answers
		// (and grants), so on resume the re-issued call passes the
		// now-updated gate. Stamp the pending tool_use ID exactly
		// like the ask_user suspension in shapeToolOutcome.
		return nil, &delegate.ErrAskUser{
			Question:         permission.AskPrompt(tu.Name, hookInput, rule),
			PendingToolUseID: tu.ID,
			PermissionMarker: permission.Marker(tu.Name, hookInput, rule),
		}
	}
	// permission.Allow falls through to execution.
	return nil, nil
}

// runToolExecution reports the call on onToolStarted, executes the tool,
// and reports the outcome on onToolCall. Secret placeholders are
// materialised into the input the tool actually executes with; the
// placeholder form (tu.PartialJSON) is what the callbacks, hooks, and
// event log persist, so the real secret never reaches the store — only
// the live tool call (Layer 1).
func runToolExecution(ctx context.Context, gt *GenerationTool, tu toolUseBlock, materialize func(string) string, onToolStarted, onToolCall func(ToolCallInfo)) (string, error) {
	if onToolStarted != nil {
		onToolStarted(ToolCallInfo{
			ToolName:  tu.Name,
			InputSize: len(tu.PartialJSON),
			ToolUseID: tu.ID,
			Input:     json.RawMessage(tu.PartialJSON),
		})
	}

	execInput := json.RawMessage(tu.PartialJSON)
	if materialize != nil {
		execInput = json.RawMessage(materialize(tu.PartialJSON))
	}
	start := time.Now()
	output, err := gt.Execute(ctx, execInput)
	dur := time.Since(start)

	if onToolCall != nil {
		onToolCall(ToolCallInfo{
			ToolName:  tu.Name,
			InputSize: len(tu.PartialJSON),
			ToolUseID: tu.ID,
			Output:    output,
			Duration:  dur,
			Error:     err,
		})
	}
	return output, err
}

// shapeToolOutcome converts an execution outcome into the tool_result
// block for the conversation, firing the observational post-hooks.
//
// Special case: ask_user requested by the LLM. The suspension aborts the
// loop and propagates up so the backend can surface the question to
// iterion's pause/resume flow. The PostToolUseFailure hook is
// intentionally NOT fired — this isn't a tool failure, it's a suspension
// request. The pending tool_use ID is stamped so the backend can craft a
// tool_result block on resume.
func shapeToolOutcome(ctx context.Context, runner *hooks.Runner, tu toolUseBlock, hookInput map[string]any, output string, err error) (api.ContentBlock, error) {
	if err != nil {
		var askErr *delegate.ErrAskUser
		if errors.As(err, &askErr) {
			askErr.PendingToolUseID = tu.ID
			return api.ContentBlock{}, askErr
		}
		// Post-tool fires are observational; the runner logs any
		// handler error itself, so we discard the (Decision, error)
		// return on purpose.
		_, _ = runner.Fire(ctx, hooks.Context{
			Event:     hooks.PostToolUseFailure,
			ToolName:  tu.Name,
			ToolInput: hookInput,
			ToolError: err,
		})
		return api.ToolResult{
			ToolUseID: tu.ID,
			Content:   fmt.Sprintf("tool error: %v", err),
			IsError:   true,
		}.ToContentBlock(), nil
	}
	_, _ = runner.Fire(ctx, hooks.Context{
		Event:      hooks.PostToolUse,
		ToolName:   tu.Name,
		ToolInput:  hookInput,
		ToolResult: output,
	})
	return api.ToolResult{
		ToolUseID: tu.ID,
		Content:   output,
	}.ToContentBlock(), nil
}

// maybeCompact runs claw's pure-function compactor with a config sized
// to the given model's context window (default trigger at 85% of the
// window, last 4 messages kept verbatim). The ratio and preserveRecent
// arguments override those defaults; pass 0 to keep them.
//
// It is a no-op for short transcripts (returns the input unchanged with
// `compacted=false`) and a bounded summarisation for long ones — the
// last preserveRecent turns are kept verbatim, so any assistant message
// holding a pending tool_use stays addressable for the next tool round
// or for resume after a pause.
func maybeCompact(messages []api.Message, model string, ratio float64, preserveRecent int) (out []api.Message, info CompactInfo, compacted bool) {
	cfg := clawrt.DefaultCompactionConfigForModel(model, ratio, preserveRecent)
	res := clawrt.CompactMessages(messages, cfg)
	if res == nil {
		return messages, CompactInfo{}, false
	}
	return res.CompactedMessages, CompactInfo{
		BeforeMessages:      len(messages),
		AfterMessages:       len(res.CompactedMessages),
		RemovedMessageCount: res.RemovedMessageCount,
	}, true
}

// maybeCompactPause is a thin wrapper over maybeCompact for the pause
// path that already discards the info struct (the pause checkpoint
// records the conversation, not the compaction event).
func maybeCompactPause(messages []api.Message, model string, ratio float64, preserveRecent int) []api.Message {
	out, _, _ := maybeCompact(messages, model, ratio, preserveRecent)
	return out
}

// maxContextCompactRetries bounds the reactive force-compaction that
// runs when the backend REJECTS a request for exceeding its real context
// window. Threshold compaction (maybeCompact) sizes itself to the model's
// ADVERTISED window (e.g. gpt-5's 1.05M), but the active backend may
// enforce a smaller one — most notably an OpenAI model driven through the
// ChatGPT-forfait endpoint, whose effective context is far below the
// API's. In that case the estimate stays under the advertised window so
// threshold compaction never fires, and without this reactive pass the
// tool loop dies mid-run with context_length_exceeded.
const maxContextCompactRetries = 4

// contextRetryTargets are the shrinking force-compaction token budgets
// tried, in order, on a context-window rejection — stepped well below
// common forfait caps so a compacted retry fits even when the backend's
// real window is unknown.
var contextRetryTargets = []int{256_000, 128_000, 64_000, 32_000}

// contextWindowMarkers identifies a backend's rejection of a request
// for exceeding the model's context window. claw's markers live in an
// internal package, so we mirror them here (provider-agnostic).
var contextWindowMarkers = []string{
	"context_length_exceeded", "maximum context length", "context window",
	"context length", "too many tokens", "prompt is too long",
	"input is too long", "request is too large",
}

// isContextWindowError reports whether err is the backend rejecting a
// request for exceeding the model's context window.
func isContextWindowError(err error) bool {
	if err == nil {
		return false
	}
	return strutil.ContainsAnyFold(err.Error(), contextWindowMarkers)
}

// forceCompactToTokens force-compacts messages to a target token budget,
// independent of the model's advertised window. Returns the compacted
// slice and true only when it actually shrank the history (so the caller
// stops retrying once the transcript can't get any smaller).
func forceCompactToTokens(messages []api.Message, targetTokens, preserveRecent int) ([]api.Message, bool) {
	if preserveRecent <= 0 {
		preserveRecent = clawrt.DefaultCompactionPreserveRecent
	}
	res := clawrt.CompactMessages(messages, clawrt.CompactionConfig{
		PreserveRecentMessages: preserveRecent,
		MaxEstimatedTokens:     targetTokens,
	})
	if res == nil || len(res.CompactedMessages) == 0 || len(res.CompactedMessages) >= len(messages) {
		return messages, false
	}
	return res.CompactedMessages, true
}

// callWithContextRetry runs one model call and, on a context-window
// rejection, force-compacts the (pointer-shared) history to a shrinking
// target and retries, up to maxContextCompactRetries. It mutates
// *messages in place so the compaction persists into the rest of the
// tool loop. Non-context errors and exhausted retries surface unchanged.
func callWithContextRetry(ctx context.Context, client api.APIClient, opts GenerationOptions, messages *[]api.Message, toolChoice *api.ToolChoice) (*aggregatedResponse, error) {
	for attempt := 0; ; attempt++ {
		req, err := buildRequest(opts, *messages, nil, toolChoice)
		if err != nil {
			return nil, err
		}
		fireOnRequest(opts, len(*messages))
		agg, callErr := callAndAggregate(ctx, client, req, opts)
		e := callErr
		if e == nil && agg != nil {
			e = agg.err
		}
		if e == nil {
			return agg, nil
		}
		if !isContextWindowError(e) || attempt >= maxContextCompactRetries {
			return agg, e
		}
		target := contextRetryTargets[len(contextRetryTargets)-1]
		if attempt < len(contextRetryTargets) {
			target = contextRetryTargets[attempt]
		}
		compacted, ok := forceCompactToTokens(*messages, target, opts.CompactPreserveRecent)
		if !ok {
			return agg, e // can't shrink further → surface the original error
		}
		*messages = compacted
		// Same reseed as the threshold path: after a forced squeeze the
		// model must re-read its todo list before continuing.
		if hasTodoTool(opts.Tools) {
			*messages = append(*messages, todoReseedMessage())
		}
		if opts.OnContextCompactRetry != nil {
			opts.OnContextCompactRetry(attempt+1, e, len(compacted), target)
		}
	}
}
