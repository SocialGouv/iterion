package model

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

const (
	// defaultMaxSteps is the default tool-loop iteration limit.
	defaultMaxSteps = 10

	// defaultMaxTokens is the default max tokens per response.
	defaultMaxTokens = 8192
)

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
func aggregateStream(ctx context.Context, ch <-chan api.StreamEvent) aggregatedResponse {
	var res aggregatedResponse
	blocks := make(map[int]*blockState)

	for {
		select {
		case <-ctx.Done():
			res.err = ctx.Err()
			return res
		case event, ok := <-ch:
			if !ok {
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
		System:      opts.System,
		Messages:    messages,
		Temperature: opts.Temperature,
		ToolChoice:  toolChoice,
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
		opts.OnRequest(RequestInfo{
			Model:        opts.Model,
			MessageCount: messageCount,
			ToolCount:    len(opts.Tools),
			Timestamp:    time.Now(),
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
func executeToolsDirect(
	ctx context.Context,
	toolUses []toolUseBlock,
	toolMap map[string]*GenerationTool,
	onToolCall func(ToolCallInfo),
) []api.ContentBlock {
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

		start := time.Now()
		output, err := gt.Execute(ctx, json.RawMessage(tu.PartialJSON))
		dur := time.Since(start)

		if onToolCall != nil {
			onToolCall(ToolCallInfo{
				ToolName:  tu.Name,
				InputSize: len(tu.PartialJSON),
				Duration:  dur,
				Error:     err,
			})
		}

		if err != nil {
			results = append(results, api.ToolResult{
				ToolUseID: tu.ID,
				Content:   fmt.Sprintf("tool error: %v", err),
				IsError:   true,
			}.ToContentBlock())
		} else {
			results = append(results, api.ToolResult{
				ToolUseID: tu.ID,
				Content:   output,
			}.ToContentBlock())
		}
	}

	return results
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

	for step := 1; step <= maxSteps; step++ {
		req, err := buildRequest(opts, messages, nil, nil)
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
			break
		}

		// Append assistant message with the tool_use blocks.
		messages = append(messages, assistantToolUseMessage(agg.text, agg.toolUses))

		// Execute tools and append tool_result message.
		toolResults := executeToolsDirect(ctx, agg.toolUses, toolMap, opts.OnToolCall)
		messages = append(messages, api.Message{
			Role:    "user",
			Content: toolResults,
		})
	}

	return &TextResult{
		Text:         lastText,
		ToolCalls:    lastToolCalls,
		Steps:        steps,
		TotalUsage:   totalUsage,
		FinishReason: lastFinish,
	}, nil
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
		var inputMap map[string]any
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
			var obj T
			if err := json.Unmarshal([]byte(tu.PartialJSON), &obj); err != nil {
				return nil, fmt.Errorf("parse structured output: %w (raw: %s)", err, tu.PartialJSON)
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
