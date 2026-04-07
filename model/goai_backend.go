package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/delegate"
	"github.com/SocialGouv/iterion/ir"
	goai "github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"
)

// GoaiBackend implements delegate.Backend by calling goai's GenerateText
// and GenerateObject APIs. It wraps the direct LLM path into the unified
// Backend interface.
type GoaiBackend struct {
	registry *Registry
	schemas  map[string]*ir.Schema
	hooks    EventHooks
	retry    RetryPolicy
}

// NewGoaiBackend creates a new GoaiBackend.
func NewGoaiBackend(registry *Registry, schemas map[string]*ir.Schema, hooks EventHooks, retry RetryPolicy) *GoaiBackend {
	return &GoaiBackend{
		registry: registry,
		schemas:  schemas,
		hooks:    hooks,
		retry:    retry,
	}
}

// Execute implements delegate.Backend.
func (b *GoaiBackend) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	// Resolve model.
	m, err := b.registry.Resolve(task.Model)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("goai backend: %w", err)
	}

	// Build goai options.
	var opts []goai.Option
	opts = append(opts, goai.WithMaxRetries(0))

	// Reasoning effort.
	if popts := providerOptsForNode(task.ReasoningEffort); popts != nil {
		opts = append(opts, goai.WithProviderOptions(popts))
	}

	// System prompt.
	if task.SystemPrompt != "" {
		opts = append(opts, goai.WithSystem(task.SystemPrompt))
	}

	// User message.
	userText := task.UserPrompt

	// When both tools AND output schema are present, inject schema format
	// instruction into user text (GenerateText supports tool loop,
	// GenerateObject does not).
	if task.OutputSchema != nil && task.HasTools {
		schemaJSON, _ := json.MarshalIndent(task.OutputSchema, "", "  ")
		userText += fmt.Sprintf(
			"\n\nOUTPUT FORMAT: After completing all tool operations, your final message MUST be a raw JSON object matching this schema:\n%s\nNo markdown fences, no extra text — ONLY the JSON object.",
			string(schemaJSON),
		)
	}

	if userText != "" {
		opts = append(opts, goai.WithMessages(goai.UserMessage(userText)))
	}

	// Tools.
	if len(task.ToolDefs) > 0 {
		tools := toolDefsToGoai(task.ToolDefs)
		opts = append(opts, goai.WithTools(tools...))
		maxSteps := task.ToolMaxSteps
		if maxSteps <= 0 {
			maxSteps = 5
		}
		opts = append(opts, goai.WithMaxSteps(maxSteps))
	}

	// Observability hooks.
	nodeID := task.NodeID
	if b.hooks.OnLLMRequest != nil {
		fn := b.hooks.OnLLMRequest
		opts = append(opts, goai.WithOnRequest(func(info goai.RequestInfo) {
			fn(nodeID, fromGoaiRequestInfo(info))
		}))
	}
	if b.hooks.OnLLMResponse != nil {
		fn := b.hooks.OnLLMResponse
		opts = append(opts, goai.WithOnResponse(func(info goai.ResponseInfo) {
			fn(nodeID, fromGoaiResponseInfo(info))
		}))
	}
	if b.hooks.OnLLMStepFinish != nil {
		fn := b.hooks.OnLLMStepFinish
		opts = append(opts, goai.WithOnStepFinish(func(step goai.StepResult) {
			fn(nodeID, fromGoaiStepResult(step))
		}))
	}
	if b.hooks.OnToolCall != nil {
		fn := b.hooks.OnToolCall
		opts = append(opts, goai.WithOnToolCall(func(info goai.ToolCallInfo) {
			fn(nodeID, fromGoaiToolCallInfo(info))
		}))
	}

	// Dispatch to the appropriate generation strategy.
	hasSchema := task.OutputSchema != nil
	if hasSchema && !task.HasTools {
		return b.generateStructuredWithRetry(ctx, m, task, opts)
	}
	if hasSchema && task.HasTools {
		return b.generateTextWithToolsAndSchemaRetry(ctx, m, task, opts)
	}
	return b.generateTextWithRetry(ctx, m, task, opts)
}

// ---------------------------------------------------------------------------
// Retry
// ---------------------------------------------------------------------------

func (b *GoaiBackend) retryLoop(ctx context.Context, nodeID string, fn func() (delegate.Result, error)) (delegate.Result, error) {
	maxAttempts := b.retry.maxAttempts()
	result, err := fn()
	for attempt := 1; err != nil && isRetryable(err) && attempt < maxAttempts; attempt++ {
		delay := b.retry.backoff(attempt - 1)

		if b.hooks.OnLLMRetry != nil {
			b.hooks.OnLLMRetry(nodeID, RetryInfo{
				Attempt:    attempt,
				Error:      err,
				StatusCode: statusCodeOf(err),
				Delay:      delay,
			})
		}

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return delegate.Result{}, ctx.Err()
		}

		result, err = fn()
	}
	return result, err
}

// ---------------------------------------------------------------------------
// Generation strategies
// ---------------------------------------------------------------------------

func (b *GoaiBackend) generateStructuredWithRetry(ctx context.Context, m provider.LanguageModel, task delegate.Task, opts []goai.Option) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateStructured(ctx, m, task, opts)
	})
}

func (b *GoaiBackend) generateStructured(ctx context.Context, m provider.LanguageModel, task delegate.Task, opts []goai.Option) (delegate.Result, error) {
	// Clone opts to avoid aliasing bugs when called repeatedly from retryLoop.
	callOpts := make([]goai.Option, len(opts), len(opts)+1)
	copy(callOpts, opts)
	callOpts = append(callOpts, goai.WithExplicitSchema(task.OutputSchema))

	result, err := goai.GenerateObject[map[string]interface{}](ctx, m, callOpts...)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("goai backend: structured generation: %w", err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]interface{})
	}

	tokens := result.Usage.InputTokens + result.Usage.OutputTokens
	output["_tokens"] = tokens
	output["_model"] = m.ModelID()

	return delegate.Result{
		Output:      output,
		Tokens:      tokens,
		BackendName: "goai",
	}, nil
}

func (b *GoaiBackend) generateTextWithRetry(ctx context.Context, m provider.LanguageModel, task delegate.Task, opts []goai.Option) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateText(ctx, m, task, opts)
	})
}

func (b *GoaiBackend) generateText(ctx context.Context, m provider.LanguageModel, task delegate.Task, opts []goai.Option) (delegate.Result, error) {
	result, err := goai.GenerateText(ctx, m, opts...)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("goai backend: text generation: %w", err)
	}

	tokens := result.TotalUsage.InputTokens + result.TotalUsage.OutputTokens
	output := map[string]interface{}{
		"text":    result.Text,
		"_tokens": tokens,
		"_model":  m.ModelID(),
	}

	return delegate.Result{
		Output:      output,
		Tokens:      tokens,
		BackendName: "goai",
	}, nil
}

func (b *GoaiBackend) generateTextWithToolsAndSchemaRetry(ctx context.Context, m provider.LanguageModel, task delegate.Task, opts []goai.Option) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateTextWithToolsAndSchema(ctx, m, task, opts)
	})
}

func (b *GoaiBackend) generateTextWithToolsAndSchema(ctx context.Context, m provider.LanguageModel, task delegate.Task, opts []goai.Option) (delegate.Result, error) {
	result, err := goai.GenerateText(ctx, m, opts...)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("goai backend: text+tools generation: %w", err)
	}

	text := strings.TrimSpace(result.Text)
	text = extractJSON(text)

	if text == "" {
		return delegate.Result{}, fmt.Errorf("goai backend: text+tools generation produced empty response after tool loop")
	}

	var output map[string]interface{}
	parseFallback := false
	if err := json.Unmarshal([]byte(text), &output); err != nil {
		output = map[string]interface{}{
			"text": text,
		}
		parseFallback = true
	}

	tokens := result.TotalUsage.InputTokens + result.TotalUsage.OutputTokens
	output["_tokens"] = tokens
	output["_model"] = m.ModelID()

	return delegate.Result{
		Output:        output,
		Tokens:        tokens,
		BackendName:   "goai",
		ParseFallback: parseFallback,
	}, nil
}

// ---------------------------------------------------------------------------
// Tool conversion
// ---------------------------------------------------------------------------

// toolDefsToGoai converts delegate.ToolDef slices to goai.Tool slices.
func toolDefsToGoai(defs []delegate.ToolDef) []goai.Tool {
	tools := make([]goai.Tool, len(defs))
	for i, d := range defs {
		tools[i] = goai.Tool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
			Execute:     d.Execute,
		}
	}
	return tools
}

// goaiToolsToDefs converts goai.Tool slices to delegate.ToolDef slices.
func goaiToolsToDefs(tools []goai.Tool) []delegate.ToolDef {
	defs := make([]delegate.ToolDef, len(tools))
	for i, t := range tools {
		defs[i] = delegate.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
			Execute:     t.Execute,
		}
	}
	return defs
}
