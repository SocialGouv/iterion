package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/thinktokens"
	"github.com/SocialGouv/iterion/pkg/errtrack"
)

// ---------------------------------------------------------------------------
// Request building
// ---------------------------------------------------------------------------

// buildRequest constructs a CreateMessageRequest from GenerationOptions and messages.
// extraTools and toolChoice are appended/set on top of opts.Tools.
// wireModelID strips an optional "provider/" routing prefix from a model
// spec, returning the bare model ID the wire API expects. iterion selects
// the provider via Registry.Resolve(spec); the resolved client then needs
// only the bare model on the request — claw_backend and subagent already
// pass bare, but the direct-generation callers (executeHumanLLM,
// ExecuteReviewCompanion) pass the full spec. Without this, "anthropic/
// claude-sonnet-4-6" reaches the Anthropic API verbatim and 404s (the
// openai/bedrock claw providers strip it incidentally; anthropic does not).
// A bare "claude-opus-4-8" (no slash) is returned unchanged.
func wireModelID(spec string) string {
	if i := strings.Index(spec, "/"); i >= 0 {
		return spec[i+1:]
	}
	return spec
}

// llmSpanOp is the operation name every in-process provider call is
// traced under, so one query in Sentry covers them all.
const llmSpanOp = "llm.generate"

// modelProvider is wireModelID's other half: the routing prefix of a
// model spec ("anthropic/claude-opus-5" → "anthropic"), or "default"
// when the spec is bare and the registry picks the provider.
func modelProvider(spec string) string {
	if i := strings.Index(spec, "/"); i >= 0 {
		return spec[:i]
	}
	return "default"
}

func buildRequest(opts GenerationOptions, messages []api.Message, extraTools []api.Tool, toolChoice *api.ToolChoice) (api.CreateMessageRequest, error) {
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	req := api.CreateMessageRequest{
		Model:       wireModelID(opts.Model),
		MaxTokens:   maxTokens,
		Messages:    messages,
		Temperature: opts.Temperature,
		ToolChoice:  toolChoice,
	}

	// SystemBlocks takes precedence over the authored System string for
	// cache_control support. Keep both wire representations populated:
	// Anthropic/Bedrock/Vertex prefer SystemBlocks, while the OpenAI and
	// Foundry transports only consume System. Without the mirrored string,
	// OpenAI Responses silently receives no authored instructions.
	if len(opts.SystemBlocks) > 0 {
		req.SystemBlocks = opts.SystemBlocks
		systemTexts := make([]string, 0, len(opts.SystemBlocks))
		for _, block := range opts.SystemBlocks {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				systemTexts = append(systemTexts, block.Text)
			}
		}
		req.System = strings.Join(systemTexts, "\n\n")
	} else {
		req.System = opts.System
	}

	// Map provider-specific options.
	if opts.ProviderOptions != nil {
		if re, ok := opts.ProviderOptions["reasoning_effort"].(string); ok && re != "" {
			req.ReasoningEffort = re
		}
	}

	// Anthropic rejects extended thinking when tool_choice forces a specific
	// tool ("Thinking may not be enabled when tool_choice forces tool use").
	// Structured output (GenerateObjectDirect) always forces the synthetic
	// tool, so on a model with adaptive thinking on by default (e.g.
	// claude-sonnet-4-6) the call 400s. Force thinking off for forced-tool
	// requests. Harmless for OpenAI (the field is ignored by the openai
	// provider's request conversion).
	if toolChoice != nil && (toolChoice.Type == "tool" || toolChoice.Type == "any") {
		req.Thinking = &api.ThinkingConfig{Type: "off"}
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
	total.ReasoningTokens += step.ReasoningTokens
	total.ThinkingMs += step.ThinkingMs
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
//
// This is iterion's ONE hand-instrumented tracing seam: every in-process
// provider call funnels through here, and one call is a unit of work
// worth timing. A run is not — it lasts minutes to hours, so it gets no
// transaction of its own.
func callAndAggregate(
	ctx context.Context,
	client api.APIClient,
	req api.CreateMessageRequest,
	opts GenerationOptions,
) (*aggregatedResponse, error) {
	ctx, span := errtrack.StartSpan(ctx, llmSpanOp, opts.Model)
	span.SetTag("llm.provider", modelProvider(opts.Model))
	span.SetTag("llm.model", wireModelID(opts.Model))

	start := time.Now()
	ch, err := client.StreamResponse(ctx, req)
	if err != nil {
		span.Finish(err)
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
	span.SetData("llm.input_tokens", agg.usage.InputTokens)
	span.SetData("llm.output_tokens", agg.usage.OutputTokens)
	span.SetData("llm.cache_read_tokens", agg.usage.CacheReadTokens)
	span.Finish(agg.err)

	// Thinking metrics. Preferred source is the provider's exact billed
	// count (usage.output_tokens_details, captured from message_delta);
	// when absent, approximate by re-encoding the accumulated thinking
	// text. Timing is measured from the stream (start→stop of each
	// thinking block).
	if agg.usage.ReasoningTokens == 0 {
		agg.usage.ReasoningTokens = thinktokens.Count(agg.thinkingText)
	}
	agg.usage.ThinkingMs = agg.thinkingMs

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
