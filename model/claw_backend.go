package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/delegate"
)

// ClawBackend implements delegate.Backend by calling GenerateTextDirect and
// GenerateObjectDirect against api.APIClient. It wraps the direct LLM path
// into the unified Backend interface.
type ClawBackend struct {
	registry *Registry
	hooks    EventHooks
	retry    RetryPolicy
}

// NewClawBackend creates a new ClawBackend.
func NewClawBackend(registry *Registry, hooks EventHooks, retry RetryPolicy) *ClawBackend {
	return &ClawBackend{
		registry: registry,
		hooks:    hooks,
		retry:    retry,
	}
}

// Execute implements delegate.Backend.
func (b *ClawBackend) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	// Resolve API client.
	client, err := b.registry.Resolve(task.Model)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: %w", err)
	}

	// Build GenerationOptions.
	opts := GenerationOptions{
		Model: task.Model,
	}

	// Reasoning effort via ProviderOptions.
	if popts := providerOptsForNode(task.ReasoningEffort); popts != nil {
		opts.ProviderOptions = popts
	}

	// System prompt.
	if task.SystemPrompt != "" {
		opts.System = task.SystemPrompt
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
		opts.Messages = []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: userText}}},
		}
	}

	// Tools.
	if len(task.ToolDefs) > 0 {
		opts.Tools = toolDefsToGeneration(task.ToolDefs)
		maxSteps := task.ToolMaxSteps
		if maxSteps <= 0 {
			maxSteps = 5
		}
		opts.MaxSteps = maxSteps
	}

	// Observability hooks.
	applyHooks(task.NodeID, b.hooks, &opts)

	// Dispatch to the appropriate generation strategy.
	hasSchema := task.OutputSchema != nil
	if hasSchema && !task.HasTools {
		return b.generateStructuredWithRetry(ctx, client, task, opts)
	}
	if hasSchema && task.HasTools {
		return b.generateTextWithToolsAndSchemaRetry(ctx, client, task, opts)
	}
	return b.generateTextWithRetry(ctx, client, task, opts)
}

// ---------------------------------------------------------------------------
// Retry
// ---------------------------------------------------------------------------

func (b *ClawBackend) retryLoop(ctx context.Context, nodeID string, fn func() (delegate.Result, error)) (delegate.Result, error) {
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

func (b *ClawBackend) generateStructuredWithRetry(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateStructured(ctx, client, task, opts)
	})
}

func (b *ClawBackend) generateStructured(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	// Set the explicit schema for structured output.
	genOpts := opts
	genOpts.ExplicitSchema = task.OutputSchema

	result, err := GenerateObjectDirect[map[string]interface{}](ctx, client, genOpts)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: structured generation: %w", err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]interface{})
	}

	tokens := result.TotalUsage.InputTokens + result.TotalUsage.OutputTokens
	output["_tokens"] = tokens
	output["_model"] = task.Model

	return delegate.Result{
		Output:      output,
		Tokens:      tokens,
		BackendName: delegate.BackendClaw,
	}, nil
}

func (b *ClawBackend) generateTextWithRetry(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateText(ctx, client, task, opts)
	})
}

func (b *ClawBackend) generateText(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	result, err := GenerateTextDirect(ctx, client, opts)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: text generation: %w", err)
	}

	tokens := result.TotalUsage.InputTokens + result.TotalUsage.OutputTokens
	output := map[string]interface{}{
		"text":    result.Text,
		"_tokens": tokens,
		"_model":  task.Model,
	}

	return delegate.Result{
		Output:      output,
		Tokens:      tokens,
		BackendName: delegate.BackendClaw,
	}, nil
}

func (b *ClawBackend) generateTextWithToolsAndSchemaRetry(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	return b.retryLoop(ctx, task.NodeID, func() (delegate.Result, error) {
		return b.generateTextWithToolsAndSchema(ctx, client, task, opts)
	})
}

func (b *ClawBackend) generateTextWithToolsAndSchema(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	result, err := GenerateTextDirect(ctx, client, opts)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: text+tools generation: %w", err)
	}

	text := strings.TrimSpace(result.Text)
	text = extractJSON(text)

	if text == "" {
		return delegate.Result{}, fmt.Errorf("claw backend: text+tools generation produced empty response after tool loop")
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
	output["_model"] = task.Model

	return delegate.Result{
		Output:        output,
		Tokens:        tokens,
		BackendName:   delegate.BackendClaw,
		ParseFallback: parseFallback,
	}, nil
}

// ---------------------------------------------------------------------------
// Tool conversion
// ---------------------------------------------------------------------------

// toolDefsToGeneration converts delegate.ToolDef slices to GenerationTool slices.
func toolDefsToGeneration(defs []delegate.ToolDef) []GenerationTool {
	tools := make([]GenerationTool, len(defs))
	for i, d := range defs {
		tools[i] = GenerationTool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.InputSchema,
			Execute:     d.Execute,
		}
	}
	return tools
}
