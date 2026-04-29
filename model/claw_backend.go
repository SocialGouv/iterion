package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"

	"github.com/SocialGouv/iterion/delegate"
)

// ClawBackend implements delegate.Backend by calling GenerateTextDirect and
// GenerateObjectDirect against api.APIClient. It wraps the direct LLM path
// into the unified Backend interface.
type ClawBackend struct {
	registry       *Registry
	hooks          EventHooks
	retry          RetryPolicy
	lifecycleHooks *hooks.Runner
}

// ClawBackendOption configures a ClawBackend at construction time.
type ClawBackendOption func(*ClawBackend)

// WithBackendLifecycleHooks installs an in-process hook runner fired
// around tool execution and at session end. A nil runner is a no-op.
func WithBackendLifecycleHooks(r *hooks.Runner) ClawBackendOption {
	return func(b *ClawBackend) { b.lifecycleHooks = r }
}

// NewClawBackend creates a new ClawBackend.
func NewClawBackend(registry *Registry, hk EventHooks, retry RetryPolicy, opts ...ClawBackendOption) *ClawBackend {
	b := &ClawBackend{
		registry: registry,
		hooks:    hk,
		retry:    retry,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Execute implements delegate.Backend.
func (b *ClawBackend) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	// Resolve API client.
	client, err := b.registry.Resolve(task.Model)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: %w", err)
	}

	// Strip the "provider/" prefix so the request body carries the bare
	// model ID. Provider routing is already done at this point (via
	// Resolve), and provider APIs (Anthropic, OpenAI) don't recognize the
	// prefixed form in the JSON body — Anthropic returns 404, OpenAI may
	// silently coerce or also reject depending on the model.
	_, modelID, err := ParseModelSpec(task.Model)
	if err != nil {
		return delegate.Result{}, fmt.Errorf("claw backend: %w", err)
	}

	// Build GenerationOptions.
	opts := GenerationOptions{
		Model:     modelID,
		MaxTokens: task.MaxTokens,
	}

	// Reasoning effort via ProviderOptions.
	if popts := providerOptsForNode(task.ReasoningEffort); popts != nil {
		opts.ProviderOptions = popts
	}

	// System prompt with ephemeral cache_control marker.
	if task.SystemPrompt != "" {
		opts.SystemBlocks = []api.ContentBlock{{
			Type:         "text",
			Text:         task.SystemPrompt,
			CacheControl: api.EphemeralCacheControl(),
		}}
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

	// In-process lifecycle hooks (audit, safety, compaction
	// observability). Nil-safe at call sites in generation.go.
	opts.Hooks = b.lifecycleHooks

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
