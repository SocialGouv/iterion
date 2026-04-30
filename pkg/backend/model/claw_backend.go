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

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
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
		Model:                 modelID,
		MaxTokens:             task.MaxTokens,
		CompactThresholdRatio: task.CompactThresholdRatio,
		CompactPreserveRecent: task.CompactPreserveRecent,
	}

	// Reasoning effort via ProviderOptions.
	if popts := providerOptsForNode(task.ReasoningEffort); popts != nil {
		opts.ProviderOptions = popts
	}

	// System prompt (optionally augmented with the interaction protocol)
	// with ephemeral cache_control marker.
	systemText := task.SystemPromptWithInteraction()
	if systemText != "" {
		opts.SystemBlocks = []api.ContentBlock{{
			Type:         "text",
			Text:         systemText,
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

	// Resume mode: the persisted conversation already contains the original
	// user prompt and the assistant message with the pending tool_use block.
	// Replace opts.Messages with that conversation plus a new user-role
	// message carrying a tool_result that answers the captured ask_user
	// call. This rehydrates the LLM's mid-tool-loop state across the pause
	// without re-rendering the prompt or relying on the [PRIOR INTERACTION]
	// suffix.
	if len(task.ResumeConversation) > 0 {
		var prior []api.Message
		if err := json.Unmarshal(task.ResumeConversation, &prior); err != nil {
			return delegate.Result{}, fmt.Errorf("claw backend: decode resume conversation: %w", err)
		}
		if task.ResumePendingToolUseID == "" {
			return delegate.Result{}, fmt.Errorf("claw backend: resume conversation set but pending tool_use ID is empty")
		}
		answer := task.ResumeAnswer
		if answer == "" {
			answer = "(no answer provided)"
		}
		prior = append(prior, api.Message{
			Role: "user",
			Content: []api.ContentBlock{api.ToolResult{
				ToolUseID: task.ResumePendingToolUseID,
				Content:   answer,
			}.ToContentBlock()},
		})
		opts.Messages = prior
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

// askUserResult converts a *delegate.ErrAskUser into the standard
// _needs_interaction Result iterion's executor expects. Used by every
// generation path so an LLM-issued ask_user call surfaces uniformly
// regardless of which generation strategy ran (structured / text /
// text+tools+schema). Conversation + PendingToolUseID propagate through
// Result so the runtime can persist them in the checkpoint, enabling
// mid-tool-loop resume on the next turn.
func askUserResult(err error) (delegate.Result, bool) {
	var ask *delegate.ErrAskUser
	if !errors.As(err, &ask) {
		return delegate.Result{}, false
	}
	return delegate.Result{
		Output: map[string]interface{}{
			"_needs_interaction": true,
			"_interaction_questions": map[string]interface{}{
				delegate.AskUserQuestionKey: ask.Question,
			},
		},
		BackendName:         delegate.BackendClaw,
		PendingConversation: ask.Conversation,
		PendingToolUseID:    ask.PendingToolUseID,
	}, true
}

func (b *ClawBackend) generateStructured(ctx context.Context, client api.APIClient, task delegate.Task, opts GenerationOptions) (delegate.Result, error) {
	// Set the explicit schema for structured output.
	genOpts := opts
	genOpts.ExplicitSchema = task.OutputSchema

	result, err := GenerateObjectDirect[map[string]interface{}](ctx, client, genOpts)
	if err != nil {
		if r, ok := askUserResult(err); ok {
			return r, nil
		}
		return delegate.Result{}, fmt.Errorf("claw backend: structured generation: %w", err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]interface{})
	}

	tokens := cost.Annotate(output, task.Model, result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens)

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
	opts = applySessionMessages(ctx, task.NodeID, opts)
	result, err := GenerateTextDirect(ctx, client, opts)
	captureSessionMessages(ctx, task.NodeID, result)
	if err != nil {
		if r, ok := askUserResult(err); ok {
			return r, nil
		}
		return delegate.Result{}, fmt.Errorf("claw backend: text generation: %w", err)
	}

	output := map[string]interface{}{"text": result.Text}
	tokens := cost.Annotate(output, task.Model, result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens)

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
	opts = applySessionMessages(ctx, task.NodeID, opts)
	result, err := GenerateTextDirect(ctx, client, opts)
	captureSessionMessages(ctx, task.NodeID, result)
	if err != nil {
		if r, ok := askUserResult(err); ok {
			return r, nil
		}
		return delegate.Result{}, fmt.Errorf("claw backend: text+tools generation: %w", err)
	}

	text := strings.TrimSpace(result.Text)
	text = extractJSON(text)

	// Try the cheap path first: parse the tool-loop's final text as JSON.
	// If the model already committed to structured output, we're done.
	if text != "" {
		var output map[string]interface{}
		if err := json.Unmarshal([]byte(text), &output); err == nil {
			tokens := cost.Annotate(output, task.Model, result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens)
			return delegate.Result{
				Output:      output,
				Tokens:      tokens,
				BackendName: delegate.BackendClaw,
			}, nil
		}
	}

	// Recovery pass — fires when the tool loop produced either no
	// final text (MaxSteps exhausted, model kept calling tools) OR a
	// non-JSON narrative response ("No findings.", "I reviewed X..."
	// — common with gpt-5.5 when the schema feels heavy). Same
	// conversation history, NO tools, schema enforced via
	// GenerateObjectDirect. The model is now obliged to produce
	// structured output on its next turn. Mirrors claude_code's
	// two-pass formatting.
	recoveryOpts := opts
	recoveryOpts.Messages = result.Messages
	recoveryOpts.Tools = nil
	recoveryOpts.MaxSteps = 1
	recoveryOpts.ExplicitSchema = task.OutputSchema

	obj, recErr := GenerateObjectDirect[map[string]interface{}](ctx, client, recoveryOpts)
	if recErr == nil && obj != nil && obj.Object != nil {
		tokens := cost.Annotate(obj.Object, task.Model,
			result.TotalUsage.InputTokens+obj.TotalUsage.InputTokens,
			result.TotalUsage.OutputTokens+obj.TotalUsage.OutputTokens)
		return delegate.Result{
			Output:             obj.Object,
			Tokens:             tokens,
			BackendName:        delegate.BackendClaw,
			FormattingPassUsed: true,
		}, nil
	}

	// Last-ditch: surface whatever text we got as a parse-fallback so
	// the runtime's existing structured-output retry path can decide
	// what to do. The error from the recovery pass is logged for
	// post-mortem.
	if text == "" {
		return delegate.Result{}, fmt.Errorf("claw backend: text+tools generation produced empty response after tool loop and structured-output recovery failed: %v", recErr)
	}
	output := map[string]interface{}{"text": text}
	tokens := cost.Annotate(output, task.Model, result.TotalUsage.InputTokens, result.TotalUsage.OutputTokens)
	return delegate.Result{
		Output:        output,
		Tokens:        tokens,
		BackendName:   delegate.BackendClaw,
		ParseFallback: true,
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
