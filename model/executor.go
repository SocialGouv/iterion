package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"

	goai "github.com/zendev-sh/goai"
	"github.com/zendev-sh/goai/provider"

	"github.com/iterion-ai/iterion/ir"
)

// ---------------------------------------------------------------------------
// Retry policy
// ---------------------------------------------------------------------------

// DefaultMaxAttempts is the default number of LLM call attempts (initial + retries).
const DefaultMaxAttempts = 3

// DefaultBackoffBase is the base duration for exponential backoff.
const DefaultBackoffBase = time.Second

// RetryPolicy controls automatic retry on transient LLM errors.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts (1 = no retry). Default: 3.
	MaxAttempts int
	// BackoffBase is the base delay for exponential backoff. Default: 1s.
	BackoffBase time.Duration
}

func (rp RetryPolicy) maxAttempts() int {
	if rp.MaxAttempts <= 0 {
		return DefaultMaxAttempts
	}
	return rp.MaxAttempts
}

func (rp RetryPolicy) backoffBase() time.Duration {
	if rp.BackoffBase <= 0 {
		return DefaultBackoffBase
	}
	return rp.BackoffBase
}

// backoff returns the delay for attempt n (0-indexed) with jitter.
func (rp RetryPolicy) backoff(attempt int) time.Duration {
	base := float64(rp.backoffBase()) * math.Pow(2, float64(attempt))
	maxDelay := float64(60 * time.Second)
	if base > maxDelay {
		base = maxDelay
	}
	// Jitter: 0.5x to 1.5x.
	jitter := 0.5 + rand.Float64()
	return time.Duration(base * jitter)
}

// RetryInfo describes a retry attempt, passed to the OnLLMRetry hook.
type RetryInfo struct {
	Attempt    int           // 1-based retry number (attempt 1 = first retry)
	Error      error         // the error that triggered this retry
	StatusCode int           // HTTP status code if available
	Delay      time.Duration // backoff delay before this retry
}

// ---------------------------------------------------------------------------
// Event hooks
// ---------------------------------------------------------------------------

// EventHooks allows the executor to emit observability events back to the caller.
type EventHooks struct {
	OnLLMRequest    func(nodeID string, info goai.RequestInfo)
	OnLLMResponse   func(nodeID string, info goai.ResponseInfo)
	OnLLMRetry      func(nodeID string, info RetryInfo)
	OnLLMStepFinish func(nodeID string, step goai.StepResult)
	OnToolCall      func(nodeID string, info goai.ToolCallInfo)
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// GoaiExecutor implements runtime.NodeExecutor by delegating LLM calls
// to goai's GenerateText and GenerateObject APIs.
type GoaiExecutor struct {
	registry *Registry
	prompts  map[string]*ir.Prompt
	schemas  map[string]*ir.Schema
	vars     map[string]interface{}
	tools    map[string]goai.Tool // registered tool implementations
	hooks    EventHooks
	retry    RetryPolicy
}

// GoaiExecutorOption configures a GoaiExecutor.
type GoaiExecutorOption func(*GoaiExecutor)

// WithEventHooks sets observability callbacks on the executor.
func WithEventHooks(h EventHooks) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.hooks = h }
}

// WithToolImplementations registers tool implementations by name.
func WithToolImplementations(tools map[string]goai.Tool) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.tools = tools }
}

// WithRetryPolicy sets the retry policy for transient LLM errors.
func WithRetryPolicy(rp RetryPolicy) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.retry = rp }
}

// NewGoaiExecutor creates a GoaiExecutor for a given workflow.
func NewGoaiExecutor(registry *Registry, wf *ir.Workflow, opts ...GoaiExecutorOption) *GoaiExecutor {
	e := &GoaiExecutor{
		registry: registry,
		prompts:  wf.Prompts,
		schemas:  wf.Schemas,
		tools:    make(map[string]goai.Tool),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// SetVars sets the workflow variables for the current run.
// Must be called before Execute.
func (e *GoaiExecutor) SetVars(vars map[string]interface{}) {
	e.vars = vars
}

// Execute implements runtime.NodeExecutor.
func (e *GoaiExecutor) Execute(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	switch node.Kind {
	case ir.NodeAgent, ir.NodeJudge:
		return e.executeLLM(ctx, node, input)
	case ir.NodeRouter:
		// Routers are deterministic pass-throughs handled by the engine.
		return input, nil
	case ir.NodeTool:
		return e.executeToolNode(ctx, node, input)
	default:
		return nil, fmt.Errorf("model: unsupported node kind %q for execution", node.Kind)
	}
}

// executeLLM handles agent and judge nodes by calling goai.
func (e *GoaiExecutor) executeLLM(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	// Resolve model.
	m, err := e.registry.Resolve(node.Model)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: %w", node.ID, err)
	}

	// Build goai options.
	var opts []goai.Option

	// Disable goai's internal retry — we handle retries ourselves for
	// per-attempt event emission.
	opts = append(opts, goai.WithMaxRetries(0))

	// System prompt.
	if node.SystemPrompt != "" {
		if p, ok := e.prompts[node.SystemPrompt]; ok {
			systemText := e.resolveTemplate(p.Body, input)
			opts = append(opts, goai.WithSystem(systemText))
		}
	}

	// User message from user prompt or input.
	userText := e.buildUserMessage(node, input)
	if userText != "" {
		opts = append(opts, goai.WithMessages(goai.UserMessage(userText)))
	}

	// Tools.
	if len(node.Tools) > 0 {
		var tools []goai.Tool
		for _, toolName := range node.Tools {
			if t, ok := e.tools[toolName]; ok {
				tools = append(tools, t)
			}
		}
		if len(tools) > 0 {
			opts = append(opts, goai.WithTools(tools...))
			maxSteps := node.ToolMaxSteps
			if maxSteps <= 0 {
				maxSteps = 5
			}
			opts = append(opts, goai.WithMaxSteps(maxSteps))
		}
	}

	// Observability hooks — OnRequest, OnResponse, OnStepFinish, OnToolCall
	// are wired as goai callbacks. Retry hooks are handled by our retry loop.
	nodeID := node.ID
	if e.hooks.OnLLMRequest != nil {
		fn := e.hooks.OnLLMRequest
		opts = append(opts, goai.WithOnRequest(func(info goai.RequestInfo) {
			fn(nodeID, info)
		}))
	}
	if e.hooks.OnLLMResponse != nil {
		fn := e.hooks.OnLLMResponse
		opts = append(opts, goai.WithOnResponse(func(info goai.ResponseInfo) {
			fn(nodeID, info)
		}))
	}
	if e.hooks.OnLLMStepFinish != nil {
		fn := e.hooks.OnLLMStepFinish
		opts = append(opts, goai.WithOnStepFinish(func(step goai.StepResult) {
			fn(nodeID, step)
		}))
	}
	if e.hooks.OnToolCall != nil {
		fn := e.hooks.OnToolCall
		opts = append(opts, goai.WithOnToolCall(func(info goai.ToolCallInfo) {
			fn(nodeID, info)
		}))
	}

	// Dispatch to structured or text generation with retry.
	if node.OutputSchema != "" {
		return e.generateStructuredWithRetry(ctx, m, node, opts)
	}
	return e.generateTextWithRetry(ctx, m, node, opts)
}

// ---------------------------------------------------------------------------
// Retry wrapper
// ---------------------------------------------------------------------------

// isRetryable returns true if err is a transient goai error that should be retried.
func isRetryable(err error) bool {
	var apiErr *goai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsRetryable
	}
	return false
}

// statusCodeOf extracts the HTTP status code from an APIError, or 0.
func statusCodeOf(err error) int {
	var apiErr *goai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// retryLoop runs fn up to maxAttempts times, emitting llm_retry events
// and applying exponential backoff for retryable errors.
func (e *GoaiExecutor) retryLoop(ctx context.Context, nodeID string, fn func() (map[string]interface{}, error)) (map[string]interface{}, error) {
	maxAttempts := e.retry.maxAttempts()

	result, err := fn()
	for attempt := 1; err != nil && isRetryable(err) && attempt < maxAttempts; attempt++ {
		delay := e.retry.backoff(attempt - 1)

		// Emit retry event.
		if e.hooks.OnLLMRetry != nil {
			e.hooks.OnLLMRetry(nodeID, RetryInfo{
				Attempt:    attempt,
				Error:      err,
				StatusCode: statusCodeOf(err),
				Delay:      delay,
			})
		}

		// Backoff.
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}

		result, err = fn()
	}
	return result, err
}

// ---------------------------------------------------------------------------
// Generation with retry
// ---------------------------------------------------------------------------

// generateTextWithRetry wraps generateText in the retry loop.
func (e *GoaiExecutor) generateTextWithRetry(ctx context.Context, m provider.LanguageModel, node *ir.Node, opts []goai.Option) (map[string]interface{}, error) {
	return e.retryLoop(ctx, node.ID, func() (map[string]interface{}, error) {
		return e.generateText(ctx, m, node, opts)
	})
}

// generateStructuredWithRetry wraps generateStructured in the retry loop.
func (e *GoaiExecutor) generateStructuredWithRetry(ctx context.Context, m provider.LanguageModel, node *ir.Node, opts []goai.Option) (map[string]interface{}, error) {
	return e.retryLoop(ctx, node.ID, func() (map[string]interface{}, error) {
		return e.generateStructured(ctx, m, node, opts)
	})
}

// ---------------------------------------------------------------------------
// Core generation
// ---------------------------------------------------------------------------

// generateStructured uses goai.GenerateObject with an explicit JSON schema.
func (e *GoaiExecutor) generateStructured(ctx context.Context, m provider.LanguageModel, node *ir.Node, opts []goai.Option) (map[string]interface{}, error) {
	schema, ok := e.schemas[node.OutputSchema]
	if !ok {
		return nil, fmt.Errorf("model: node %q references unknown schema %q", node.ID, node.OutputSchema)
	}

	jsonSchema, err := SchemaToJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: schema conversion: %w", node.ID, err)
	}

	opts = append(opts, goai.WithExplicitSchema(jsonSchema))

	result, err := goai.GenerateObject[map[string]interface{}](ctx, m, opts...)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: structured generation: %w", node.ID, err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]interface{})
	}

	// Strict validation: every required field must be present.
	// No hidden JSON repair — fail explicitly on schema mismatch.
	if err := ValidateOutput(output, schema); err != nil {
		return nil, fmt.Errorf("model: node %q: structured output invalid: %w", node.ID, err)
	}

	// Attach usage metadata.
	output["_tokens"] = result.Usage.InputTokens + result.Usage.OutputTokens
	output["_model"] = m.ModelID()

	return output, nil
}

// generateText uses goai.GenerateText for free-form text output.
func (e *GoaiExecutor) generateText(ctx context.Context, m provider.LanguageModel, node *ir.Node, opts []goai.Option) (map[string]interface{}, error) {
	result, err := goai.GenerateText(ctx, m, opts...)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: text generation: %w", node.ID, err)
	}

	output := map[string]interface{}{
		"text":    result.Text,
		"_tokens": result.TotalUsage.InputTokens + result.TotalUsage.OutputTokens,
		"_model":  m.ModelID(),
	}

	return output, nil
}

// ---------------------------------------------------------------------------
// Tool node execution
// ---------------------------------------------------------------------------

// executeToolNode runs a tool node (direct command, no LLM).
func (e *GoaiExecutor) executeToolNode(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	toolName := node.Command
	tool, ok := e.tools[toolName]
	if !ok {
		return nil, fmt.Errorf("model: tool node %q references unregistered tool %q", node.ID, toolName)
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: marshal input: %w", node.ID, err)
	}

	start := time.Now()
	outputStr, err := tool.Execute(ctx, inputJSON)
	if e.hooks.OnToolCall != nil {
		e.hooks.OnToolCall(node.ID, goai.ToolCallInfo{
			ToolName:  toolName,
			InputSize: len(inputJSON),
			Duration:  time.Since(start),
			Error:     err,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: execute: %w", node.ID, err)
	}

	// Try to parse tool output as JSON map, otherwise wrap as text.
	var output map[string]interface{}
	if jsonErr := json.Unmarshal([]byte(outputStr), &output); jsonErr != nil {
		output = map[string]interface{}{"result": outputStr}
	}

	return output, nil
}

// ---------------------------------------------------------------------------
// Template resolution
// ---------------------------------------------------------------------------

// buildUserMessage constructs the user message for an LLM call.
func (e *GoaiExecutor) buildUserMessage(node *ir.Node, input map[string]interface{}) string {
	// If the node has a user prompt template, resolve it.
	if node.UserPrompt != "" {
		if p, ok := e.prompts[node.UserPrompt]; ok {
			return e.resolveTemplate(p.Body, input)
		}
	}

	// Fallback: serialize input as the user message.
	if len(input) == 0 {
		return ""
	}

	b, err := json.Marshal(input)
	if err != nil {
		return fmt.Sprintf("%v", input)
	}
	return string(b)
}

// resolveTemplate substitutes {{...}} references in a prompt body.
func (e *GoaiExecutor) resolveTemplate(body string, input map[string]interface{}) string {
	var b strings.Builder
	remaining := body

	for {
		start := strings.Index(remaining, "{{")
		if start == -1 {
			b.WriteString(remaining)
			break
		}
		end := strings.Index(remaining[start:], "}}")
		if end == -1 {
			b.WriteString(remaining)
			break
		}
		end += start + 2

		b.WriteString(remaining[:start])

		ref := strings.TrimSpace(remaining[start+2 : end-2])
		val, resolved := e.resolveTemplateRef(ref, input)
		if resolved {
			b.WriteString(val)
		} else {
			// Keep unresolved refs as-is.
			b.WriteString(remaining[start:end])
		}

		remaining = remaining[end:]
	}

	return b.String()
}

// resolveTemplateRef resolves a single "namespace.path" reference.
// Returns the resolved value and true, or ("", false) if unresolvable.
func (e *GoaiExecutor) resolveTemplateRef(ref string, input map[string]interface{}) (string, bool) {
	parts := strings.SplitN(ref, ".", 2)
	if len(parts) < 2 {
		return "", false
	}

	namespace := parts[0]
	key := parts[1]

	switch namespace {
	case "input":
		if v, ok := input[key]; ok {
			return formatValue(v), true
		}
	case "vars":
		if e.vars != nil {
			if v, ok := e.vars[key]; ok {
				return formatValue(v), true
			}
		}
	}

	return "", false
}

// formatValue converts an interface value to a string for template substitution.
func formatValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case nil:
		return ""
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}
