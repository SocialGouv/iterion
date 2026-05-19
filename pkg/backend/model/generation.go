package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

const (
	// defaultMaxSteps is the default tool-loop iteration limit.
	defaultMaxSteps = 10

	// defaultMaxTokens is the default max tokens per response.
	defaultMaxTokens = 8192

	// maxToolInputJSONSize caps the accumulated input_json_delta
	// fragments for a single tool_use block. A misbehaving provider
	// (or a malformed stream that never sends content_block_stop)
	// would otherwise grow the PartialJSON buffer without bound and
	// OOM the runner. 10 MB is well above any realistic tool input
	// while still cheap to fail loud on.
	maxToolInputJSONSize = 10 * 1024 * 1024
)

// ErrToolInputTooLarge signals that a streamed tool_use block's
// accumulated input JSON exceeded maxToolInputJSONSize.
var ErrToolInputTooLarge = errors.New("aggregateStream: tool_use input exceeded max size")

// ---------------------------------------------------------------------------
// Stream aggregation
// ---------------------------------------------------------------------------

// toolUseBlock is a collected tool_use block from a streamed response.
type toolUseBlock struct {
	ID          string
	Name        string
	PartialJSON string // concatenated input_json_delta fragments
}

// aggregatedResponse is the result of consuming a StreamEvent channel.
type aggregatedResponse struct {
	text       string
	toolUses   []toolUseBlock
	usage      Usage
	stopReason string
	err        error
}

// blockState tracks a single content block during stream aggregation.
type blockState struct {
	blockType string // "text" or "tool_use"
	text      string
	toolUse   toolUseBlock
	stopped   bool
}

// aggregateStream reads all events from ch and builds an aggregatedResponse.
// It tracks content blocks by index and concatenates deltas.
//
// On any early return, the upstream goroutine inside claw-code-go's
// StreamResponse is still trying to push the rest of the response into
// ch. If we return immediately, that goroutine blocks at the next send
// (ch is buffered ~64) and never releases the underlying TCP connection.
// A deferred drainer wraps every exit path so the upstream goroutine
// completes — the old code spawned a drainer only on the ctx-cancel
// branch and silently leaked the connection on tool-input-too-large or
// EventError early returns.
func aggregateStream(ctx context.Context, ch <-chan api.StreamEvent) aggregatedResponse {
	var res aggregatedResponse
	blocks := make(map[int]*blockState)
	drained := false
	defer func() {
		if drained {
			return
		}
		go func() {
			for range ch {
			}
		}()
	}()

	for {
		select {
		case <-ctx.Done():
			res.err = ctx.Err()
			return res
		case event, ok := <-ch:
			if !ok {
				drained = true
				res.text, res.toolUses = collectBlocks(blocks)
				for _, bs := range blocks {
					if bs.blockType == "tool_use" && !bs.stopped {
						res.err = fmt.Errorf("incomplete tool_use block: %s (content_block_stop not received)", bs.toolUse.Name)
						return res
					}
				}
				return res
			}

			switch event.Type {
			case api.EventMessageStart:
				res.usage.InputTokens = event.InputTokens
				res.usage.CacheReadTokens = event.CacheReadInputTokens
				res.usage.CacheWriteTokens = event.CacheCreationInputTokens

			case api.EventContentBlockStart:
				bs := &blockState{blockType: event.ContentBlock.Type}
				if event.ContentBlock.Type == "tool_use" {
					bs.toolUse = toolUseBlock{
						ID:   event.ContentBlock.ID,
						Name: event.ContentBlock.Name,
					}
				}
				blocks[event.ContentBlock.Index] = bs

			case api.EventContentBlockDelta:
				bs, ok := blocks[event.Index]
				if !ok {
					bs = &blockState{blockType: "text"}
					blocks[event.Index] = bs
				}
				switch event.Delta.Type {
				case "text_delta":
					bs.text += event.Delta.Text
				case "input_json_delta":
					if len(bs.toolUse.PartialJSON)+len(event.Delta.PartialJSON) > maxToolInputJSONSize {
						res.err = fmt.Errorf("%w: tool %q exceeded %d bytes", ErrToolInputTooLarge, bs.toolUse.Name, maxToolInputJSONSize)
						res.text, res.toolUses = collectBlocks(blocks)
						return res
					}
					bs.toolUse.PartialJSON += event.Delta.PartialJSON
				}

			case api.EventContentBlockStop:
				if bs, ok := blocks[event.Index]; ok {
					bs.stopped = true
				}

			case api.EventMessageDelta:
				res.usage.OutputTokens = event.Usage.OutputTokens
				res.stopReason = event.StopReason

			case api.EventError:
				if classified := ClassifyStreamError([]byte(event.ErrorMessage)); classified != nil {
					res.err = classified
				} else {
					res.err = fmt.Errorf("stream error: %s", event.ErrorMessage)
				}
				res.text, res.toolUses = collectBlocks(blocks)
				return res

			case api.EventMessageStop, api.EventPing:
				// No action needed.
			}
		}
	}
}

// collectBlocks extracts text and tool_use blocks from the block state map,
// ordered by block index.
func collectBlocks(blocks map[int]*blockState) (string, []toolUseBlock) {
	if len(blocks) == 0 {
		return "", nil
	}

	maxIdx := 0
	for idx := range blocks {
		if idx > maxIdx {
			maxIdx = idx
		}
	}

	var text string
	var toolUses []toolUseBlock
	for i := 0; i <= maxIdx; i++ {
		bs, ok := blocks[i]
		if !ok {
			continue
		}
		switch bs.blockType {
		case "text":
			text += bs.text
		case "tool_use":
			toolUses = append(toolUses, bs.toolUse)
		}
	}
	return text, toolUses
}

// ---------------------------------------------------------------------------
// Request building
// ---------------------------------------------------------------------------

// buildRequest constructs a CreateMessageRequest from GenerationOptions and messages.
// extraTools and toolChoice are appended/set on top of opts.Tools.
func buildRequest(opts GenerationOptions, messages []api.Message, extraTools []api.Tool, toolChoice *api.ToolChoice) (api.CreateMessageRequest, error) {
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	req := api.CreateMessageRequest{
		Model:       opts.Model,
		MaxTokens:   maxTokens,
		Messages:    messages,
		Temperature: opts.Temperature,
		ToolChoice:  toolChoice,
	}

	// SystemBlocks takes precedence over System string for cache_control support.
	if len(opts.SystemBlocks) > 0 {
		req.SystemBlocks = opts.SystemBlocks
	} else {
		req.System = opts.System
	}

	// Map provider-specific options.
	if opts.ProviderOptions != nil {
		if re, ok := opts.ProviderOptions["reasoning_effort"].(string); ok && re != "" {
			req.ReasoningEffort = re
		}
	}

	for _, gt := range opts.Tools {
		var schema api.InputSchema
		if len(gt.InputSchema) > 0 {
			if err := json.Unmarshal(gt.InputSchema, &schema); err != nil {
				return api.CreateMessageRequest{}, fmt.Errorf("invalid InputSchema for tool %q: %w", gt.Name, err)
			}
		}
		req.Tools = append(req.Tools, api.Tool{
			Name:        gt.Name,
			Description: gt.Description,
			InputSchema: schema,
		})
	}
	req.Tools = append(req.Tools, extraTools...)

	// Mark the last tool as the cache breakpoint for the tools array prefix.
	if n := len(req.Tools); n > 0 {
		req.Tools[n-1].CacheControl = api.EphemeralCacheControl()
	}

	return req, nil
}

// buildToolMap creates a name→GenerationTool lookup.
func buildToolMap(tools []GenerationTool) map[string]*GenerationTool {
	m := make(map[string]*GenerationTool, len(tools))
	for i := range tools {
		m[tools[i].Name] = &tools[i]
	}
	return m
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// accumulateUsage adds step usage into the running total.
func accumulateUsage(total *Usage, step Usage) {
	total.InputTokens += step.InputTokens
	total.OutputTokens += step.OutputTokens
	total.TotalTokens = total.InputTokens + total.OutputTokens
	total.CacheReadTokens += step.CacheReadTokens
	total.CacheWriteTokens += step.CacheWriteTokens
}

// toolCallsFromBlocks converts aggregated tool_use blocks to ToolCall values.
func toolCallsFromBlocks(toolUses []toolUseBlock) []ToolCall {
	if len(toolUses) == 0 {
		return nil
	}
	calls := make([]ToolCall, len(toolUses))
	for i, tu := range toolUses {
		calls[i] = ToolCall{
			ID:    tu.ID,
			Name:  tu.Name,
			Input: json.RawMessage(tu.PartialJSON),
		}
	}
	return calls
}

// fireOnRequest calls the OnRequest hook if set.
func fireOnRequest(opts GenerationOptions, messageCount int) {
	if opts.OnRequest != nil {
		var reasoning string
		if opts.ProviderOptions != nil {
			if re, ok := opts.ProviderOptions["reasoning_effort"].(string); ok {
				reasoning = re
			}
		}
		opts.OnRequest(RequestInfo{
			Model:           opts.Model,
			MessageCount:    messageCount,
			ToolCount:       len(opts.Tools),
			ReasoningEffort: reasoning,
			Timestamp:       time.Now(),
		})
	}
}

// callAndAggregate calls StreamResponse, aggregates the stream, fires the
// OnResponse hook, and returns the aggregated result. On StreamResponse
// failure it fires OnResponse with the error and returns nil, err.
func callAndAggregate(
	ctx context.Context,
	client api.APIClient,
	req api.CreateMessageRequest,
	opts GenerationOptions,
) (*aggregatedResponse, error) {
	start := time.Now()
	ch, err := client.StreamResponse(ctx, req)
	if err != nil {
		if opts.OnResponse != nil {
			opts.OnResponse(ResponseInfo{
				Latency: time.Since(start),
				Error:   err,
			})
		}
		return nil, err
	}

	agg := aggregateStream(ctx, ch)
	latency := time.Since(start)

	finishReason := mapStopReason(agg.stopReason)
	if opts.OnResponse != nil {
		opts.OnResponse(ResponseInfo{
			Latency:      latency,
			Usage:        agg.usage,
			FinishReason: finishReason,
			Error:        agg.err,
		})
	}

	return &agg, nil
}

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
) ([]api.ContentBlock, error) {
	results := make([]api.ContentBlock, 0, len(toolUses))

	for _, tu := range toolUses {
		gt, ok := toolMap[tu.Name]
		if !ok {
			results = append(results, api.ToolResult{
				ToolUseID: tu.ID,
				Content:   fmt.Sprintf("unknown tool: %s", tu.Name),
				IsError:   true,
			}.ToContentBlock())
			if onToolCall != nil {
				onToolCall(ToolCallInfo{
					ToolName:  tu.Name,
					InputSize: len(tu.PartialJSON),
					Error:     fmt.Errorf("unknown tool: %s", tu.Name),
				})
			}
			continue
		}

		// Validate that PartialJSON is well-formed JSON before either
		// firing hooks with stale/empty input or invoking Execute with
		// a payload its decoder can't parse. A malformed PartialJSON
		// at this point indicates either a truncated stream the
		// upstream aggregateStream missed, or a provider that emits
		// invalid JSON; both warrant a tool_result-isError, not a
		// silent empty-args call that the LLM would never recover
		// from cleanly.
		var hookInput map[string]any
		if jsonErr := json.Unmarshal([]byte(tu.PartialJSON), &hookInput); jsonErr != nil {
			results = append(results, api.ToolResult{
				ToolUseID: tu.ID,
				Content:   fmt.Sprintf("malformed tool input: %v", jsonErr),
				IsError:   true,
			}.ToContentBlock())
			if onToolCall != nil {
				onToolCall(ToolCallInfo{
					ToolName:  tu.Name,
					InputSize: len(tu.PartialJSON),
					ToolUseID: tu.ID,
					Error:     fmt.Errorf("malformed tool input: %w", jsonErr),
				})
			}
			continue
		}

		if dec, _ := runner.Fire(ctx, hooks.Context{
			Event:     hooks.PreToolUse,
			ToolName:  tu.Name,
			ToolInput: hookInput,
		}); dec.Action == hooks.ActionBlock {
			reason := dec.Reason
			if reason == "" {
				reason = "blocked by lifecycle hook"
			}
			results = append(results, api.ToolResult{
				ToolUseID: tu.ID,
				Content:   fmt.Sprintf("tool refused: %s", reason),
				IsError:   true,
			}.ToContentBlock())
			if onToolCall != nil {
				onToolCall(ToolCallInfo{
					ToolName:  tu.Name,
					InputSize: len(tu.PartialJSON),
					Error:     fmt.Errorf("blocked by hook: %s", reason),
				})
			}
			continue
		}

		if onToolStarted != nil {
			onToolStarted(ToolCallInfo{
				ToolName:  tu.Name,
				InputSize: len(tu.PartialJSON),
				ToolUseID: tu.ID,
				Input:     json.RawMessage(tu.PartialJSON),
			})
		}

		start := time.Now()
		output, err := gt.Execute(ctx, json.RawMessage(tu.PartialJSON))
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

		if err != nil {
			// Special case: ask_user requested by the LLM. Abort the loop
			// and propagate up so the backend can surface the question to
			// iterion's pause/resume flow. The PostToolUseFailure hook is
			// intentionally NOT fired — this isn't a tool failure, it's a
			// suspension request. Stamp the pending tool_use ID so the
			// backend can craft a tool_result block on resume.
			var askErr *delegate.ErrAskUser
			if errors.As(err, &askErr) {
				askErr.PendingToolUseID = tu.ID
				return results, askErr
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
			results = append(results, api.ToolResult{
				ToolUseID: tu.ID,
				Content:   fmt.Sprintf("tool error: %v", err),
				IsError:   true,
			}.ToContentBlock())
		} else {
			_, _ = runner.Fire(ctx, hooks.Context{
				Event:      hooks.PostToolUse,
				ToolName:   tu.Name,
				ToolInput:  hookInput,
				ToolResult: output,
			})
			results = append(results, api.ToolResult{
				ToolUseID: tu.ID,
				Content:   output,
			}.ToContentBlock())
		}
	}

	return results, nil
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

// ---------------------------------------------------------------------------
// Finish reason mapping
// ---------------------------------------------------------------------------

// mapStopReason converts an Anthropic stop_reason string to a FinishReason.
func mapStopReason(reason string) FinishReason {
	switch reason {
	case "end_turn", "stop":
		return FinishStop
	case "tool_use":
		return FinishToolCalls
	case "max_tokens":
		return FinishLength
	case "content_filter":
		return FinishContentFilter
	default:
		return FinishOther
	}
}

// ---------------------------------------------------------------------------
// Core generation: text
// ---------------------------------------------------------------------------

// GenerateTextDirect generates text using api.APIClient.StreamResponse directly.
// It runs a tool loop: call model → execute tools → append results → repeat,
// up to MaxSteps iterations.
func GenerateTextDirect(ctx context.Context, client api.APIClient, opts GenerationOptions) (*TextResult, error) {
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

	for step := 1; step <= maxSteps; step++ {
		req, err := buildRequest(opts, messages, nil, nil)
		if err != nil {
			return partial(), err
		}

		fireOnRequest(opts, len(messages))

		agg, err := callAndAggregate(ctx, client, req, opts)
		if err != nil {
			return partial(), err
		}
		if agg.err != nil {
			return partial(), agg.err
		}

		accumulateUsage(&totalUsage, agg.usage)
		finishReason := mapStopReason(agg.stopReason)
		stepToolCalls := toolCallsFromBlocks(agg.toolUses)

		stepResult := StepResult{
			Number:       step,
			Text:         agg.text,
			ToolCalls:    stepToolCalls,
			FinishReason: finishReason,
			Usage:        agg.usage,
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
		toolResults, toolErr := executeToolsDirect(ctx, agg.toolUses, toolMap, opts.OnToolStarted, opts.OnToolCall, opts.Hooks)
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
// The "[OPERATOR MESSAGE]" prefix is conventional rather than
// load-bearing: the agent can see them, react if relevant, or
// continue its plan otherwise.
func buildOperatorMessage(texts []string) api.Message {
	var sb strings.Builder
	sb.WriteString("[OPERATOR MESSAGE]\n")
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
