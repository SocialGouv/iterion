package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// ---------------------------------------------------------------------------
// Core generation: text
// ---------------------------------------------------------------------------

// GenerateTextDirect generates text using api.APIClient.StreamResponse directly.
// It runs a tool loop: call model → execute tools → append results → repeat,
// up to MaxSteps iterations.
// guardNonEmptyConversation rejects a request that carries no message at all,
// turning the provider's opaque "messages: at least one message is required"
// 400 into a clear, actionable iterion error. The system prompt lives in
// opts.System (separate from Messages), so this fires exactly for the
// degenerate node — an entry agent/judge with an empty `user:` prompt and no
// input — never when any prior/session/tool message exists (then Messages is
// non-empty).
func guardNonEmptyConversation(messages []api.Message) error {
	if len(messages) == 0 {
		return fmt.Errorf("this node produced no message to send: give the agent/judge a `user:` prompt, or feed it input from an upstream node or workflow input — a `system:` prompt alone is not a conversation turn")
	}
	return nil
}

func GenerateTextDirect(ctx context.Context, client api.APIClient, opts GenerationOptions) (*TextResult, error) {
	if err := guardNonEmptyConversation(opts.Messages); err != nil {
		return nil, err
	}
	if opts.Hooks != nil {
		defer func() {
			_, _ = opts.Hooks.Fire(ctx, hooks.Context{Event: hooks.Stop})
		}()
	}

	maxSteps := opts.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}

	toolMap := buildToolMap(opts.Tools)

	// Copy messages to avoid mutating caller's slice.
	messages := make([]api.Message, len(opts.Messages))
	copy(messages, opts.Messages)

	var steps []StepResult
	var totalUsage Usage
	var lastText string
	var lastToolCalls []ToolCall
	var lastFinish FinishReason

	// partialResult captures whatever has been accumulated so the
	// caller can stash the conversation history for compaction-aware
	// retries even when this attempt fails. Caller should consult
	// `err` first; the partial result is best-effort.
	partial := func() *TextResult {
		return &TextResult{
			Text:         lastText,
			ToolCalls:    lastToolCalls,
			Steps:        steps,
			TotalUsage:   totalUsage,
			FinishReason: lastFinish,
			Messages:     messages,
		}
	}

	toolCallsSoFar := 0
	for step := 1; step <= maxSteps; step++ {
		// Agentic-parity lever: while ForceInitialToolUse is set and the
		// model has not yet called a single tool, pin tool_choice to "any"
		// so it MUST use a tool before it can answer — then revert to auto
		// (nil) so it can finish. Without this, gpt-5.5 (and other models)
		// under the default "auto" choice skip the tools and answer from
		// priors, producing ungrounded verdicts. No-op without tools.
		var stepToolChoice *api.ToolChoice
		if opts.ForceInitialToolUse && len(opts.Tools) > 0 && toolCallsSoFar == 0 {
			stepToolChoice = &api.ToolChoice{Type: "any"}
		}
		// callWithContextRetry builds the request, calls the model, and on
		// a context-window rejection force-compacts `messages` (in place)
		// and retries — so a backend whose real window is smaller than the
		// model's advertised one (ChatGPT-forfait) recovers instead of
		// killing the run.
		agg, err := callWithContextRetry(ctx, client, opts, &messages, stepToolChoice)
		if err != nil {
			return partial(), err
		}

		accumulateUsage(&totalUsage, agg.usage)
		finishReason := mapStopReason(agg.stopReason)
		stepToolCalls := toolCallsFromBlocks(agg.toolUses)
		toolCallsSoFar += len(stepToolCalls)

		stepResult := StepResult{
			Number:       step,
			Text:         agg.text,
			ToolCalls:    stepToolCalls,
			FinishReason: finishReason,
			Usage:        agg.usage,
			Thinking:     agg.thinkingText,
		}
		steps = append(steps, stepResult)

		if opts.OnStepFinish != nil {
			opts.OnStepFinish(stepResult)
		}

		lastText = agg.text
		lastToolCalls = stepToolCalls
		lastFinish = finishReason

		// If no tool calls or stop reason is not tool_use, we're done.
		if len(agg.toolUses) == 0 || finishReason != FinishToolCalls {
			// Fire OnTurnCapture for the final step too. The live
			// `messages` slice doesn't get this step's assistant
			// response (the loop exits), so synthesize the final
			// snapshot by appending an assistant text block. The fork
			// UX would never anchor here (final = no follow-up to
			// resume), but the timeline still wants to display the
			// turn.
			if opts.OnTurnCapture != nil {
				snap := append([]api.Message(nil), messages...)
				if agg.text != "" {
					snap = append(snap, api.Message{
						Role: "assistant",
						Content: []api.ContentBlock{{
							Type: "text",
							Text: agg.text,
						}},
					})
				}
				opts.OnTurnCapture(TurnCaptureInfo{
					Step:         step,
					Result:       stepResult,
					Conversation: snap,
				})
			}
			break
		}

		// Append assistant message with the tool_use blocks.
		messages = append(messages, assistantToolUseMessage(agg.text, agg.toolUses))

		// Execute tools and append tool_result message.
		toolResults, toolErr := executeToolsDirect(ctx, agg.toolUses, toolMap, opts.OnToolStarted, opts.OnToolCall, opts.Hooks, opts.MaterializeSecrets, opts.Permission)
		if toolErr != nil {
			// ErrAskUser (and any future suspension signal) bubbles up to
			// the backend, which converts it into iterion's pause flow.
			// At this point `messages` already contains the assistant
			// message with the pending tool_use block — capture it so the
			// backend can persist the conversation and resume mid-loop.
			// Apply pure-function compaction before marshalling so a long
			// transcript is bounded on disk; the pending tool_use stays
			// in the preserved-recent window (default 4) so its ID
			// remains addressable at resume time.
			var askErr *delegate.ErrAskUser
			if errors.As(toolErr, &askErr) {
				if convBytes, mErr := json.Marshal(maybeCompactPause(messages, opts.Model, opts.CompactThresholdRatio, opts.CompactPreserveRecent)); mErr == nil {
					askErr.Conversation = convBytes
				}
			}
			return partial(), toolErr
		}
		messages = append(messages, api.Message{
			Role:    "user",
			Content: toolResults,
		})

		// Fire OnTurnCapture at the natural end-of-iteration boundary:
		// the live `messages` slice now contains everything the NEXT
		// LLM call would see — exactly the snapshot the Fork API needs
		// to rehydrate a child claw conversation. Take a defensive
		// copy because the loop reuses the slice after this point.
		if opts.OnTurnCapture != nil {
			snap := append([]api.Message(nil), messages...)
			opts.OnTurnCapture(TurnCaptureInfo{
				Step:         step,
				Result:       stepResult,
				Conversation: snap,
			})
		}

		// Compact the running history before the next round if it's
		// grown large. No-op for short transcripts; for long ones,
		// older tool turns are summarised while the last 4 messages
		// stay verbatim, so the assistant message that just dispatched
		// tool_use blocks paired with our tool_results stays in the
		// preserved-recent window. Without this the tool loop on a
		// small-context model crashes with context_length_exceeded
		// once history exceeds the budget.
		if compacted, info, ok := maybeCompact(messages, opts.Model, opts.CompactThresholdRatio, opts.CompactPreserveRecent); ok {
			// Compaction is about to fire: give OnBeforeCompact a chance
			// to inject content (e.g. a session-memory user turn) so the
			// summary preserves it. The injected slice feeds the
			// summariser only; the live history keeps the originals.
			if opts.OnBeforeCompact != nil {
				if modified := opts.OnBeforeCompact(messages); modified != nil {
					if reCompacted, reInfo, reOk := maybeCompact(modified, opts.Model, opts.CompactThresholdRatio, opts.CompactPreserveRecent); reOk {
						compacted, info = reCompacted, reInfo
					}
				}
			}
			messages = compacted
			if opts.OnCompact != nil {
				opts.OnCompact(info)
			}
			// Re-anchor the task list after the squeeze (Grok-style reseed):
			// the todo file survived on disk, the model's view of it did not.
			if hasTodoTool(opts.Tools) {
				messages = append(messages, todoReseedMessage())
			}
		}

		// Drain operator-queued chatbox messages AFTER compaction so
		// they always land in the preserved-recent window. Consume
		// runs first so the studio inbox transitions delivered →
		// consumed in lockstep with the next request.
		if opts.Inbox != nil {
			opts.Inbox.Consume(ctx)
			if drained := opts.Inbox.Drain(ctx); len(drained) > 0 {
				messages = append(messages, buildOperatorMessage(drained))
			}
		}
	}

	return &TextResult{
		Text:         lastText,
		ToolCalls:    lastToolCalls,
		Steps:        steps,
		TotalUsage:   totalUsage,
		FinishReason: lastFinish,
		Messages:     messages,
	}, nil
}

// buildOperatorMessage wraps any operator-queued chat messages into a
// single synthetic user turn the LLM observes between tool iterations.
// A <system-reminder> header marks the harness provenance while stating
// explicitly that the content below carries user authority — the agent
// applies it, adjusting its current plan if relevant.
func buildOperatorMessage(texts []string) api.Message {
	var sb strings.Builder
	sb.WriteString(systemReminder("The operator queued the message(s) below mid-run. " +
		"They are user instructions: apply them from now on, adjusting your current plan if needed."))
	sb.WriteString("\n\n")
	for i, t := range texts {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(t)
	}
	return api.Message{
		Role: "user",
		Content: []api.ContentBlock{{
			Type: "text",
			Text: sb.String(),
		}},
	}
}

// assistantToolUseMessage builds the assistant turn that contains text (if any)
// followed by tool_use content blocks.
// findPendingToolUse scans a rehydrated conversation for the tool_use
// content block with the given id, returning its tool name and input.
// Used on resume to tell whether a paused tool_use was the ask_user
// infra tool or a real action the permission gate suspended.
func findPendingToolUse(msgs []api.Message, id string) (name string, input map[string]any, ok bool) {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ID == id {
				return b.Name, b.Input, true
			}
		}
	}
	return "", nil, false
}

func assistantToolUseMessage(text string, toolUses []toolUseBlock) api.Message {
	content := make([]api.ContentBlock, 0, len(toolUses)+1)
	if text != "" {
		content = append(content, api.ContentBlock{
			Type: "text",
			Text: text,
		})
	}
	for _, tu := range toolUses {
		// inputMap is the structured args the next API turn replays
		// back as the assistant message context. A nil Input on a
		// tool_use block produces a malformed-looking history that
		// confuses some providers; fall back to an empty object so
		// the block is at least syntactically intact. Malformed
		// PartialJSON at this point is rare (aggregateStream guards
		// against truncation) so we don't bubble it up — the
		// corresponding tool_result already carries the failure.
		inputMap := map[string]any{}
		if tu.PartialJSON != "" {
			_ = json.Unmarshal([]byte(tu.PartialJSON), &inputMap)
		}
		content = append(content, api.ContentBlock{
			Type:  "tool_use",
			ID:    tu.ID,
			Name:  tu.Name,
			Input: inputMap,
		})
	}
	return api.Message{Role: "assistant", Content: content}
}

// ---------------------------------------------------------------------------
// Core generation: structured output
// ---------------------------------------------------------------------------

// GenerateObjectDirect generates structured output by injecting a synthetic tool
// with the given schema and forcing the model to call it. The tool_use input
// is parsed as the result object of type T.
func GenerateObjectDirect[T any](ctx context.Context, client api.APIClient, opts GenerationOptions) (*ObjectResult[T], error) {
	if opts.Hooks != nil {
		defer func() {
			_, _ = opts.Hooks.Fire(ctx, hooks.Context{Event: hooks.Stop})
		}()
	}

	schemaName := opts.SchemaName
	if schemaName == "" {
		schemaName = "structured_output"
	}

	if len(opts.ExplicitSchema) == 0 {
		return nil, fmt.Errorf("GenerateObjectDirect requires ExplicitSchema to be set")
	}
	if err := guardNonEmptyConversation(opts.Messages); err != nil {
		return nil, err
	}

	var inputSchema api.InputSchema
	if err := json.Unmarshal(opts.ExplicitSchema, &inputSchema); err != nil {
		return nil, fmt.Errorf("parse ExplicitSchema: %w", err)
	}

	syntheticTool := api.Tool{
		Name:        schemaName,
		Description: "Return the structured output matching the required schema.",
		InputSchema: inputSchema,
	}
	toolChoice := &api.ToolChoice{Type: "tool", Name: schemaName}

	// Copy messages to avoid mutating caller's slice.
	messages := make([]api.Message, len(opts.Messages))
	copy(messages, opts.Messages)

	// Build a request-only opts overlay: zero out Tools so buildRequest only
	// includes the synthetic tool via extraTools.
	reqOpts := opts
	reqOpts.Tools = nil

	req, err := buildRequest(reqOpts, messages, []api.Tool{syntheticTool}, toolChoice)
	if err != nil {
		return nil, err
	}

	fireOnRequest(opts, len(messages))

	agg, err := callAndAggregate(ctx, client, req, opts)
	if err != nil {
		return nil, err
	}
	if agg.err != nil {
		return nil, agg.err
	}

	var totalUsage Usage
	accumulateUsage(&totalUsage, agg.usage)
	finishReason := mapStopReason(agg.stopReason)

	stepResult := StepResult{
		Number:       1,
		Text:         agg.text,
		ToolCalls:    toolCallsFromBlocks(agg.toolUses),
		FinishReason: finishReason,
		Usage:        agg.usage,
		Thinking:     agg.thinkingText,
	}

	if opts.OnStepFinish != nil {
		opts.OnStepFinish(stepResult)
	}

	// Find the synthetic tool_use block.
	for _, tu := range agg.toolUses {
		if tu.Name == schemaName {
			if tu.PartialJSON == "" {
				return nil, fmt.Errorf("parse structured output: model returned tool_use %q with empty input (stream may have been interrupted before content_block_stop)", schemaName)
			}
			var obj T
			if err := json.Unmarshal([]byte(tu.PartialJSON), &obj); err != nil {
				// Cap the raw payload in the error so a 5 MB
				// truncated JSON doesn't flood logs.
				raw := tu.PartialJSON
				if len(raw) > 500 {
					raw = raw[:500] + "…"
				}
				return nil, fmt.Errorf("parse structured output: %w (raw: %s)", err, raw)
			}
			return &ObjectResult[T]{
				Object:       obj,
				Text:         agg.text,
				Steps:        []StepResult{stepResult},
				TotalUsage:   totalUsage,
				FinishReason: finishReason,
			}, nil
		}
	}

	return nil, fmt.Errorf("model did not produce a %q tool_use block", schemaName)
}
