package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"time"

	goai "github.com/zendev-sh/goai"

	"github.com/SocialGouv/iterion/delegate"
	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/mcp"
	"github.com/SocialGouv/iterion/tool"
)

// ---------------------------------------------------------------------------
// Retry policy
// ---------------------------------------------------------------------------

// DefaultMaxAttempts is the default number of LLM call attempts (initial + retries).
const DefaultMaxAttempts = 3

// DefaultBackoffBase is the base duration for exponential backoff.
const DefaultBackoffBase = time.Second

// defaultRouterModel is the last-resort model for LLM routers when no model
// is configured and ITERION_DEFAULT_SUPERVISOR_MODEL is unset. Routing
// decisions are lightweight, so a fast/cheap model is sufficient.
const defaultRouterModel = "anthropic/claude-sonnet-4-6"

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

// DelegateInfo describes a backend execution attempt, passed to backend hooks.
type DelegateInfo struct {
	BackendName        string        // e.g. "claude_code", "codex"
	Duration           time.Duration // subprocess wall-clock time
	Tokens             int           // estimated total tokens consumed
	ExitCode           int           // process exit code
	Stderr             string        // captured stderr output
	RawOutputLen       int           // byte length of raw stdout
	ParseFallback      bool          // true if structured output fell back to text wrapper
	FormattingPassUsed bool          // true if two-pass execution was used (tools + schema)
	Error              error         // non-nil for OnDelegateError
	Attempt            int           // 1-based retry number (for OnDelegateRetry)
	Delay              time.Duration // backoff delay (for OnDelegateRetry)
}

// ---------------------------------------------------------------------------
// Event hooks
// ---------------------------------------------------------------------------

// EventHooks allows the executor to emit observability events back to the caller.
type EventHooks struct {
	OnLLMRequest    func(nodeID string, info LLMRequestInfo)
	OnLLMPrompt     func(nodeID string, systemPrompt string, userMessage string)
	OnLLMResponse   func(nodeID string, info LLMResponseInfo)
	OnLLMRetry      func(nodeID string, info RetryInfo)
	OnLLMStepFinish func(nodeID string, step LLMStepInfo)
	OnToolCall      func(nodeID string, info LLMToolCallInfo)
	// OnToolNodeResult is called for direct tool nodes (not LLM tool loops)
	// with full input/output content for detailed logging.
	OnToolNodeResult func(nodeID string, toolName string, input []byte, output string, elapsed time.Duration, err error)

	// Delegation lifecycle hooks.
	OnDelegateStarted  func(nodeID string, backendName string)
	OnDelegateFinished func(nodeID string, info DelegateInfo)
	OnDelegateError    func(nodeID string, info DelegateInfo)
	OnDelegateRetry    func(nodeID string, info DelegateInfo)
}

// ---------------------------------------------------------------------------
// Interaction errors
// ---------------------------------------------------------------------------

// ErrNeedsInteraction is returned by the executor when a delegate or LLM
// signals that it needs user input to continue. The runtime engine should
// handle this by pausing (interaction: human), auto-responding (interaction: llm),
// or deciding (interaction: llm_or_human) based on the node's InteractionMode.
type ErrNeedsInteraction struct {
	NodeID    string
	Questions map[string]interface{} // question_key → question text
	SessionID string                 // delegate session ID for re-invocation
	Backend   string                 // delegate backend name (empty for goai direct)
}

func (e *ErrNeedsInteraction) Error() string {
	return fmt.Sprintf("model: node %q needs user interaction (%d questions)", e.NodeID, len(e.Questions))
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// GoaiExecutor implements runtime.NodeExecutor by routing LLM calls
// through pluggable Backend implementations (goai, claude_code, codex, etc.).
type GoaiExecutor struct {
	registry        *Registry
	backendRegistry *delegate.Registry // backend registry (goai, claude_code, codex)
	toolRegistry    *tool.Registry     // unified tool registry (preferred)
	mcpManager      *mcp.Manager       // generic MCP discovery/call bridge
	toolPolicy      *tool.Policy       // allowlist policy for tool execution (nil = open)
	prompts         map[string]*ir.Prompt
	schemas         map[string]*ir.Schema
	vars            map[string]interface{}
	hooks           EventHooks
	retry           RetryPolicy
	workDir         string // working directory for backend subprocesses
	defaultBackend  string // workflow-level default backend (empty = use "goai")
}

// GoaiExecutorOption configures a GoaiExecutor.
type GoaiExecutorOption func(*GoaiExecutor)

// WithEventHooks sets observability callbacks on the executor.
func WithEventHooks(h EventHooks) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.hooks = h }
}

// WithToolRegistry sets the unified tool registry on the executor.
func WithToolRegistry(tr *tool.Registry) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.toolRegistry = tr }
}

// WithMCPManager sets the generic MCP manager used to lazily discover MCP tools.
func WithMCPManager(m *mcp.Manager) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.mcpManager = m }
}

// WithToolPolicy sets the tool execution policy on the executor.
// When set, every tool call is checked against the allowlist before
// execution. A denied tool produces an explicit error.
func WithToolPolicy(p *tool.Policy) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.toolPolicy = p }
}

// WithRetryPolicy sets the retry policy for transient LLM errors.
func WithRetryPolicy(rp RetryPolicy) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.retry = rp }
}

// WithBackendRegistry sets the backend registry on the executor.
// When set, nodes with a `backend` property are executed via the named
// backend instead of the default goai backend.
func WithBackendRegistry(dr *delegate.Registry) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.backendRegistry = dr }
}

// WithWorkDir sets the working directory for backend subprocesses.
// When set, backend nodes will run their CLI in this directory.
func WithWorkDir(dir string) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.workDir = dir }
}

// WithDefaultBackend sets the workflow-level default backend.
func WithDefaultBackend(name string) GoaiExecutorOption {
	return func(e *GoaiExecutor) { e.defaultBackend = name }
}

// NewGoaiExecutor creates a GoaiExecutor for a given workflow.
func NewGoaiExecutor(registry *Registry, wf *ir.Workflow, opts ...GoaiExecutorOption) *GoaiExecutor {
	e := &GoaiExecutor{
		registry:       registry,
		prompts:        wf.Prompts,
		schemas:        wf.Schemas,
		defaultBackend: wf.DefaultBackend,
	}
	for _, opt := range opts {
		opt(e)
	}

	// Ensure a "goai" backend is always available in the registry.
	if e.backendRegistry == nil {
		e.backendRegistry = delegate.NewRegistry()
	}
	if _, err := e.backendRegistry.Resolve("goai"); err != nil {
		e.backendRegistry.Register("goai", NewGoaiBackend(registry, wf.Schemas, e.hooks, e.retry))
	}

	return e
}

// Close releases resources held by the executor, including MCP server
// connections. It should be called when the executor is no longer needed.
func (e *GoaiExecutor) Close() error {
	if e.mcpManager != nil {
		return e.mcpManager.Close()
	}
	return nil
}

// SetVars sets the workflow variables for the current run.
// Must be called before Execute.
func (e *GoaiExecutor) SetVars(vars map[string]interface{}) {
	e.vars = vars
}

// resolveBackendName returns the effective backend name for a node.
// Resolution chain: node.Backend → workflow default → env ITERION_DEFAULT_BACKEND → "goai".
func (e *GoaiExecutor) resolveBackendName(node *ir.Node) string {
	if node.Backend != "" {
		return node.Backend
	}
	if e.defaultBackend != "" {
		return e.defaultBackend
	}
	if env := os.Getenv("ITERION_DEFAULT_BACKEND"); env != "" {
		return env
	}
	return "goai"
}

// Execute implements runtime.NodeExecutor.
func (e *GoaiExecutor) Execute(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	switch node.Kind {
	case ir.NodeAgent, ir.NodeJudge:
		return e.executeBackend(ctx, node, input)
	case ir.NodeHuman:
		return e.executeHumanLLM(ctx, node, input)
	case ir.NodeRouter:
		if node.RouterMode == ir.RouterLLM {
			return e.executeLLMRouterUnified(ctx, node, input)
		}
		// Deterministic routers are pass-throughs handled by the engine.
		return input, nil
	case ir.NodeTool:
		return e.executeToolNode(ctx, node, input)
	default:
		return nil, fmt.Errorf("model: unsupported node kind %q for execution", node.Kind)
	}
}

// executeBackend is the unified execution path for agent and judge nodes.
// It resolves the backend, builds a Task, and dispatches to the backend.
func (e *GoaiExecutor) executeBackend(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	backendName := e.resolveBackendName(node)

	if e.backendRegistry == nil {
		return nil, fmt.Errorf("model: node %q uses backend %q but no backend registry configured", node.ID, backendName)
	}

	backend, err := e.backendRegistry.Resolve(backendName)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: %w", node.ID, err)
	}

	// Build system prompt.
	var systemText string
	if node.SystemPrompt != "" {
		if p, ok := e.prompts[node.SystemPrompt]; ok {
			systemText = e.resolveTemplate(p.Body, input)
		}
	}

	// Build user message.
	userText := e.buildUserMessage(node, input)

	// Emit prompt content for observability.
	if e.hooks.OnLLMPrompt != nil {
		e.hooks.OnLLMPrompt(node.ID, systemText, userText)
	}

	// Build output schema JSON if structured output is expected.
	var outputSchema json.RawMessage
	if node.OutputSchema != "" {
		if schema, ok := e.schemas[node.OutputSchema]; ok {
			outputSchema, _ = SchemaToJSON(schema)
		}
	}

	task := delegate.Task{
		NodeID:             node.ID,
		SystemPrompt:       systemText,
		UserPrompt:         userText,
		AllowedTools:       node.Tools,
		OutputSchema:       outputSchema,
		Model:              os.ExpandEnv(node.Model),
		HasTools:           len(node.Tools) > 0,
		ToolMaxSteps:       node.ToolMaxSteps,
		WorkDir:            e.workDir,
		ReasoningEffort:    resolveReasoningEffort(node, input),
		InteractionEnabled: node.Interaction != ir.InteractionNone,
	}

	// Resolve full tool definitions for backends that manage tool loops internally.
	if len(node.Tools) > 0 {
		tools, toolErr := e.resolveToolsForNode(ctx, node, node.Tools)
		if toolErr != nil {
			return nil, fmt.Errorf("model: node %q: %w", node.ID, toolErr)
		}
		task.ToolDefs = goaiToolsToDefs(tools)
	}

	// Session continuity.
	if node.Session == ir.SessionInherit || node.Session == ir.SessionFork {
		if sid, ok := input["_session_id"].(string); ok && sid != "" {
			task.SessionID = sid
			if node.Session == ir.SessionFork {
				task.ForkSession = true
			}
		}
	}

	// Emit backend started event.
	if e.hooks.OnDelegateStarted != nil {
		e.hooks.OnDelegateStarted(node.ID, backendName)
	}

	result, err := e.retryDelegateLoop(ctx, node.ID, backendName, func() (delegate.Result, error) {
		return backend.Execute(ctx, task)
	})
	if err != nil {
		if e.hooks.OnDelegateError != nil {
			bn := result.BackendName
			if bn == "" {
				bn = backendName
			}
			e.hooks.OnDelegateError(node.ID, DelegateInfo{
				BackendName:        bn,
				Duration:           result.Duration,
				Tokens:             result.Tokens,
				ExitCode:           result.ExitCode,
				Stderr:             result.Stderr,
				RawOutputLen:       result.RawOutputLen,
				ParseFallback:      result.ParseFallback,
				FormattingPassUsed: result.FormattingPassUsed,
				Error:              err,
			})
		}
		return nil, fmt.Errorf("model: node %q: backend %q failed: %w", node.ID, backendName, err)
	}

	// Emit backend finished event.
	if e.hooks.OnDelegateFinished != nil {
		e.hooks.OnDelegateFinished(node.ID, DelegateInfo{
			BackendName:        result.BackendName,
			Duration:           result.Duration,
			Tokens:             result.Tokens,
			ExitCode:           result.ExitCode,
			Stderr:             result.Stderr,
			RawOutputLen:       result.RawOutputLen,
			ParseFallback:      result.ParseFallback,
			FormattingPassUsed: result.FormattingPassUsed,
		})
	}

	// Flag if structured output parsing fell back to text wrapper.
	if result.ParseFallback {
		result.Output["_parse_fallback"] = true
	}

	// Attach metadata.
	if result.Output["_tokens"] == nil {
		result.Output["_tokens"] = result.Tokens
	}
	result.Output["_backend"] = backendName

	// Expose session ID for downstream nodes.
	if result.SessionID != "" {
		result.Output["_session_id"] = result.SessionID
	}

	// Validate output against schema if present.
	if node.OutputSchema != "" {
		if schema, ok := e.schemas[node.OutputSchema]; ok {
			if err := ValidateOutput(result.Output, schema); err != nil {
				return nil, fmt.Errorf("model: node %q: structured output invalid: %w", node.ID, err)
			}
		}
	}

	// Check if the backend signaled that it needs user interaction.
	if node.Interaction != ir.InteractionNone {
		if needsInteraction, ok := result.Output["_needs_interaction"].(bool); ok && needsInteraction {
			questions, _ := result.Output["_interaction_questions"].(map[string]interface{})
			if questions == nil {
				questions = map[string]interface{}{"input": "The backend needs your input to continue."}
			}
			delete(result.Output, "_needs_interaction")
			delete(result.Output, "_interaction_questions")
			return nil, &ErrNeedsInteraction{
				NodeID:    node.ID,
				Questions: questions,
				SessionID: result.SessionID,
				Backend:   backendName,
			}
		}
	}

	return result.Output, nil
}

// executeHumanLLM handles human nodes in llm or llm_or_human interaction mode.
// It calls goai directly with mode-specific
// schema handling for llm_or_human (wrapper schema with needs_human_input).
func (e *GoaiExecutor) executeHumanLLM(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	if node.Interaction == ir.InteractionHuman || node.Interaction == ir.InteractionNone {
		return nil, fmt.Errorf("model: human node %q in %s interaction mode should not be executed by the model layer", node.ID, node.Interaction)
	}

	// Resolve model (expand env var references).
	m, err := e.registry.Resolve(os.ExpandEnv(node.Model))
	if err != nil {
		return nil, fmt.Errorf("model: human node %q: %w", node.ID, err)
	}

	// Build goai options.
	var opts []goai.Option
	opts = append(opts, goai.WithMaxRetries(0))

	// Reasoning effort (dynamic override from input, then static node property).
	if popts := providerOptsForNode(resolveReasoningEffort(node, input)); popts != nil {
		opts = append(opts, goai.WithProviderOptions(popts))
	}

	// System prompt.
	var systemText string
	if node.SystemPrompt != "" {
		if p, ok := e.prompts[node.SystemPrompt]; ok {
			systemText = e.resolveTemplate(p.Body, input)
			opts = append(opts, goai.WithSystem(systemText))
		}
	}

	// User message from input.
	userText := e.buildUserMessage(node, input)
	if userText != "" {
		opts = append(opts, goai.WithMessages(goai.UserMessage(userText)))
	}

	// Emit prompt content for observability.
	if e.hooks.OnLLMPrompt != nil {
		e.hooks.OnLLMPrompt(node.ID, systemText, userText)
	}

	// Observability hooks.
	nodeID := node.ID
	if e.hooks.OnLLMRequest != nil {
		fn := e.hooks.OnLLMRequest
		opts = append(opts, goai.WithOnRequest(func(info goai.RequestInfo) {
			fn(nodeID, fromGoaiRequestInfo(info))
		}))
	}
	if e.hooks.OnLLMResponse != nil {
		fn := e.hooks.OnLLMResponse
		opts = append(opts, goai.WithOnResponse(func(info goai.ResponseInfo) {
			fn(nodeID, fromGoaiResponseInfo(info))
		}))
	}

	// Determine the schema to use.
	schema, ok := e.schemas[node.OutputSchema]
	if !ok {
		return nil, fmt.Errorf("model: human node %q references unknown schema %q", node.ID, node.OutputSchema)
	}

	// For llm_or_human, wrap the schema with needs_human_input field.
	if node.Interaction == ir.InteractionLLMOrHuman {
		schema = wrapSchemaWithHumanFlag(schema)
	}

	jsonSchema, err := SchemaToJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("model: human node %q: schema conversion: %w", node.ID, err)
	}
	opts = append(opts, goai.WithExplicitSchema(jsonSchema))

	result, err := goai.GenerateObject[map[string]interface{}](ctx, m, opts...)
	if err != nil {
		return nil, fmt.Errorf("model: human node %q: structured generation: %w", node.ID, err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]interface{})
	}

	// Attach usage metadata.
	output["_tokens"] = result.Usage.InputTokens + result.Usage.OutputTokens
	output["_model"] = m.ModelID()

	return output, nil
}

// wrapSchemaWithHumanFlag creates a copy of the schema with an additional
// needs_human_input boolean field for auto_or_pause mode.
func wrapSchemaWithHumanFlag(schema *ir.Schema) *ir.Schema {
	fields := make([]*ir.SchemaField, len(schema.Fields), len(schema.Fields)+1)
	copy(fields, schema.Fields)
	fields = append(fields, &ir.SchemaField{
		Name: "needs_human_input",
		Type: ir.FieldTypeBool,
	})
	return &ir.Schema{
		Name:   schema.Name + "_auto_or_pause",
		Fields: fields,
	}
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

// ---------------------------------------------------------------------------
// Backend retry
// ---------------------------------------------------------------------------

// isDelegateRetryable determines whether a backend execution error is transient
// and worth retrying. Only signal-based kills (exit codes 128+, indicating
// OOM, SIGTERM, etc.) and I/O errors are retried. Permanent failures like
// exit 1 (application error), exit 2 (misuse), or exit 127 (command not
// found) are not retried.
func isDelegateRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Subprocess killed by signal (OOM, timeout, etc.).
	if strings.Contains(msg, "signal:") {
		return true
	}
	// Exit status: only retry signal-based exits (128+). Lower exit codes
	// indicate permanent failures that retrying won't fix.
	if strings.Contains(msg, "exit status") {
		code := extractExitCode(msg)
		// exit 128+ means the process was killed by a signal (128+N).
		// These are typically transient (OOM killer, timeout, etc.).
		return code >= 128
	}
	// Process could not start (resource exhaustion).
	if strings.Contains(msg, "failed to start") {
		return true
	}
	// Stdout reading failure (broken pipe, etc.).
	if strings.Contains(msg, "reading stdout") {
		return true
	}
	return false
}

// extractExitCode parses an exit code from an error message containing
// "exit status N". Returns -1 if no valid code is found.
func extractExitCode(msg string) int {
	const prefix = "exit status "
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return -1
	}
	rest := msg[idx+len(prefix):]
	// Parse the integer, stopping at first non-digit.
	n := 0
	found := false
	for _, c := range rest {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
			found = true
		} else {
			break
		}
	}
	if !found {
		return -1
	}
	return n
}

// retryDelegateLoop retries a backend execution call with exponential backoff.
func (e *GoaiExecutor) retryDelegateLoop(ctx context.Context, nodeID string, backendName string, fn func() (delegate.Result, error)) (delegate.Result, error) {
	maxAttempts := e.retry.maxAttempts()

	result, err := fn()
	for attempt := 1; err != nil && isDelegateRetryable(err) && attempt < maxAttempts; attempt++ {
		delay := e.retry.backoff(attempt - 1)

		log.Printf("model: node %q: delegate retry %d/%d after error: %v (backoff %s)",
			nodeID, attempt, maxAttempts-1, err, delay.Round(time.Millisecond))

		if e.hooks.OnDelegateRetry != nil {
			e.hooks.OnDelegateRetry(nodeID, DelegateInfo{
				BackendName: backendName,
				Attempt:     attempt,
				Error:       err,
				Delay:       delay,
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
// Generation with retry
// ---------------------------------------------------------------------------

// extractJSON extracts a JSON object from text that may contain markdown
// fences or surrounding commentary. Returns the raw JSON string.
func extractJSON(text string) string {
	text = strings.TrimSpace(text)

	// Strip markdown code fences.
	if strings.HasPrefix(text, "```json") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		return strings.TrimSpace(text)
	}
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		return strings.TrimSpace(text)
	}

	// If text starts with {, it's already JSON.
	if strings.HasPrefix(text, "{") {
		return text
	}

	// Try to find embedded JSON object in the text.
	start := strings.Index(text, "{")
	if start >= 0 {
		// Find the matching closing brace.
		depth := 0
		for i := start; i < len(text); i++ {
			switch text[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return text[start : i+1]
				}
			}
		}
	}

	return text
}

// ---------------------------------------------------------------------------
// Tool node execution
// ---------------------------------------------------------------------------

// executeToolNode runs a tool node (direct command, no LLM).
// The tool policy is checked before execution; denied tools produce an
// explicit error with the tool_called hook fired (Error != nil).
func (e *GoaiExecutor) executeToolNode(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	// When the command contains template refs ({{input.X}}), resolve them
	// and execute as a direct shell command. Otherwise, use the tool registry.
	if len(node.CommandRefs) > 0 {
		return e.executeToolNodeShell(ctx, node, input)
	}

	toolName := node.Command

	// Policy check before resolution — fail fast on denied tools.
	if e.toolPolicy != nil {
		if err := e.toolPolicy.Check(toolName); err != nil {
			if e.hooks.OnToolCall != nil {
				e.hooks.OnToolCall(node.ID, LLMToolCallInfo{
					ToolName: toolName,
					Error:    err,
				})
			}
			return nil, fmt.Errorf("model: tool node %q: %w", node.ID, err)
		}
	}

	resolved, ok, err := e.resolveSingleToolForNode(ctx, node, toolName)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: %w", node.ID, err)
	}
	if !ok {
		return nil, fmt.Errorf("model: tool node %q references unregistered tool %q", node.ID, toolName)
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: marshal input: %w", node.ID, err)
	}

	start := time.Now()
	outputStr, err := resolved.Execute(ctx, inputJSON)
	duration := time.Since(start)
	if e.hooks.OnToolCall != nil {
		e.hooks.OnToolCall(node.ID, LLMToolCallInfo{
			ToolName:  toolName,
			InputSize: len(inputJSON),
			Duration:  duration,
			Error:     err,
		})
	}
	// Emit detailed tool I/O via the prompt hook (reused for tool node logging).
	if e.hooks.OnToolNodeResult != nil {
		e.hooks.OnToolNodeResult(node.ID, toolName, inputJSON, outputStr, duration, err)
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

// executeToolNodeShell handles tool nodes whose command contains {{...}}
// template references. Templates are resolved from the node's input map,
// and the resulting string is executed as a shell command via sh -c.
func (e *GoaiExecutor) executeToolNodeShell(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	// Resolve template references in the command.
	resolved := resolveCommandTemplate(node.Command, node.CommandRefs, input)

	// Expand environment variables in the resolved command.
	resolved = os.ExpandEnv(resolved)

	toolName := "shell:" + node.ID

	start := time.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", resolved)
	out, err := cmd.CombinedOutput()
	outputStr := string(out)
	duration := time.Since(start)

	if e.hooks.OnToolCall != nil {
		e.hooks.OnToolCall(node.ID, LLMToolCallInfo{
			ToolName: toolName,
			Duration: duration,
			Error:    err,
		})
	}
	if e.hooks.OnToolNodeResult != nil {
		e.hooks.OnToolNodeResult(node.ID, toolName, []byte(resolved), outputStr, duration, err)
	}
	if err != nil {
		return nil, fmt.Errorf("model: tool node %q: shell command failed: %w\noutput: %s", node.ID, err, outputStr)
	}

	// Try to parse output as JSON, otherwise wrap as text.
	var output map[string]interface{}
	if jsonErr := json.Unmarshal([]byte(outputStr), &output); jsonErr != nil {
		output = map[string]interface{}{"result": strings.TrimSpace(outputStr)}
	}

	return output, nil
}

// resolveCommandTemplate substitutes {{input.X}} references in a command
// string with values from the input map. Values are shell-escaped to prevent
// command injection when the resolved string is passed to sh -c.
func resolveCommandTemplate(command string, refs []*ir.Ref, input map[string]interface{}) string {
	resolved := command
	for _, ref := range refs {
		var val interface{}
		if ref.Kind == ir.RefInput && len(ref.Path) > 0 {
			val = input[ref.Path[0]]
		}
		if val == nil {
			continue
		}
		escaped := shellEscape(fmt.Sprint(val))
		resolved = strings.ReplaceAll(resolved, ref.Raw, escaped)
	}
	return resolved
}

// shellEscape wraps a value in single quotes, escaping any embedded single
// quotes. This produces a string safe for interpolation into sh -c commands.
func shellEscape(s string) string {
	// Replace each ' with '\'': end current quote, insert escaped quote, reopen quote.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------------------
// LLM router execution
// ---------------------------------------------------------------------------

// buildRouterSchema creates an auto-generated schema for LLM routers.
// Single mode: {selected_route: string(enum), reasoning: string}
// Multi mode:  {selected_routes: string[](enum), reasoning: string}
func buildRouterSchema(node *ir.Node, candidates []string) *ir.Schema {
	if node.RouterMulti {
		return &ir.Schema{
			Name: node.ID + "_route_selection",
			Fields: []*ir.SchemaField{
				{Name: "selected_routes", Type: ir.FieldTypeStringArray, EnumValues: candidates},
				{Name: "reasoning", Type: ir.FieldTypeString},
			},
		}
	}
	return &ir.Schema{
		Name: node.ID + "_route_selection",
		Fields: []*ir.SchemaField{
			{Name: "selected_route", Type: ir.FieldTypeString, EnumValues: candidates},
			{Name: "reasoning", Type: ir.FieldTypeString},
		},
	}
}

// routerRoutingInstruction returns the standard instruction appended to LLM
// router system prompts, shared by both direct and delegated paths.
func routerRoutingInstruction(candidates []string) string {
	return fmt.Sprintf(
		"\n\nYou are a routing decision maker. Based on the input context, select the most appropriate route(s) from the available options: %v.\nRespond with your selection using the required output format.",
		candidates,
	)
}

// executeLLMRouterUnified is the unified LLM router path that works with any backend.
func (e *GoaiExecutor) executeLLMRouterUnified(ctx context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	backendName := e.resolveBackendName(node)

	if e.backendRegistry == nil {
		return nil, fmt.Errorf("model: llm router %q uses backend %q but no backend registry configured", node.ID, backendName)
	}

	backend, err := e.backendRegistry.Resolve(backendName)
	if err != nil {
		return nil, fmt.Errorf("model: llm router %q: %w", node.ID, err)
	}

	// Extract route candidates injected by the engine.
	candidatesRaw, ok := input["_route_candidates"]
	if !ok {
		return nil, fmt.Errorf("model: llm router %q: no _route_candidates in input", node.ID)
	}
	var candidates []string
	switch v := candidatesRaw.(type) {
	case []string:
		candidates = v
	case []interface{}:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("model: llm router %q: _route_candidates contains non-string element", node.ID)
			}
			candidates = append(candidates, s)
		}
	default:
		return nil, fmt.Errorf("model: llm router %q: _route_candidates is %T, expected []string", node.ID, candidatesRaw)
	}

	// Build clean input (without internal keys) for the prompt.
	cleanInput := make(map[string]interface{})
	for k, v := range input {
		if !strings.HasPrefix(k, "_") {
			cleanInput[k] = v
		}
	}

	// Build system prompt with routing instruction.
	var systemText string
	if node.SystemPrompt != "" {
		if p, ok := e.prompts[node.SystemPrompt]; ok {
			systemText = e.resolveTemplate(p.Body, cleanInput)
		}
	}
	systemText += routerRoutingInstruction(candidates)

	// User message.
	userText := e.buildUserMessage(node, cleanInput)

	// Emit prompt content for observability.
	if e.hooks.OnLLMPrompt != nil {
		e.hooks.OnLLMPrompt(node.ID, systemText, userText)
	}

	// Auto-generate schema from candidates.
	schema := buildRouterSchema(node, candidates)
	jsonSchema, err := SchemaToJSON(schema)
	if err != nil {
		return nil, fmt.Errorf("model: llm router %q: schema: %w", node.ID, err)
	}

	// Resolve model for the router (with fallback chain).
	expanded := os.ExpandEnv(node.Model)
	if expanded == "" {
		expanded = os.Getenv("ITERION_DEFAULT_SUPERVISOR_MODEL")
	}
	if expanded == "" {
		expanded = defaultRouterModel
	}

	task := delegate.Task{
		NodeID:          node.ID,
		SystemPrompt:    systemText,
		UserPrompt:      userText,
		OutputSchema:    jsonSchema,
		Model:           expanded,
		WorkDir:         e.workDir,
		ReasoningEffort: resolveReasoningEffort(node, input),
	}

	// Emit backend started event.
	if e.hooks.OnDelegateStarted != nil {
		e.hooks.OnDelegateStarted(node.ID, backendName)
	}

	result, err := e.retryDelegateLoop(ctx, node.ID, backendName, func() (delegate.Result, error) {
		return backend.Execute(ctx, task)
	})
	if err != nil {
		if e.hooks.OnDelegateError != nil {
			bn := result.BackendName
			if bn == "" {
				bn = backendName
			}
			e.hooks.OnDelegateError(node.ID, DelegateInfo{
				BackendName:        bn,
				Duration:           result.Duration,
				Tokens:             result.Tokens,
				ExitCode:           result.ExitCode,
				Stderr:             result.Stderr,
				RawOutputLen:       result.RawOutputLen,
				ParseFallback:      result.ParseFallback,
				FormattingPassUsed: result.FormattingPassUsed,
				Error:              err,
			})
		}
		return nil, fmt.Errorf("model: llm router %q: backend %q failed: %w", node.ID, backendName, err)
	}

	// Emit backend finished event.
	if e.hooks.OnDelegateFinished != nil {
		e.hooks.OnDelegateFinished(node.ID, DelegateInfo{
			BackendName:        result.BackendName,
			Duration:           result.Duration,
			Tokens:             result.Tokens,
			ExitCode:           result.ExitCode,
			Stderr:             result.Stderr,
			RawOutputLen:       result.RawOutputLen,
			ParseFallback:      result.ParseFallback,
			FormattingPassUsed: result.FormattingPassUsed,
		})
	}

	output := result.Output

	// If structured output parsing fell back to text wrapper, attempt JSON
	// extraction from the text. Routers must produce structured output.
	if result.ParseFallback {
		if textVal, ok := output["text"].(string); ok {
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(textVal), &parsed) == nil {
				output = parsed
			} else {
				return nil, fmt.Errorf("model: llm router %q: backend returned unstructured text, cannot determine route selection", node.ID)
			}
		}
	}

	// Strict validation against the router schema.
	if err := ValidateOutput(output, schema); err != nil {
		return nil, fmt.Errorf("model: llm router %q: output invalid: %w", node.ID, err)
	}

	// Attach metadata.
	if output["_tokens"] == nil {
		output["_tokens"] = result.Tokens
	}
	output["_backend"] = backendName

	return output, nil
}

// ---------------------------------------------------------------------------
// Reasoning effort resolution
// ---------------------------------------------------------------------------

// resolveReasoningEffort determines the effective reasoning effort for a node.
// It checks for a dynamic override in the input map via the reserved key
// "_reasoning_effort", then falls back to the static node property.
//
// The "_reasoning_effort" key uses an underscore prefix to distinguish it from
// user-defined schema fields. It allows upstream nodes to dynamically control
// the reasoning effort of downstream nodes via edge with-mappings, e.g.:
//
//	router -> agent with {_reasoning_effort: "high"}
//
// Valid values are defined in ir.ValidReasoningEfforts: low, medium, high, extra_high.
// Invalid dynamic values are silently ignored (falls back to the static property).
func resolveReasoningEffort(node *ir.Node, input map[string]interface{}) string {
	if v, ok := input["_reasoning_effort"]; ok {
		if s, ok := v.(string); ok && ir.ValidReasoningEfforts[s] {
			return s
		}
	}
	return node.ReasoningEffort
}

// providerOptsForNode builds the goai ProviderOptions map from the resolved
// reasoning effort. Returns nil if no provider options are needed.
func providerOptsForNode(effort string) map[string]any {
	if effort == "" {
		return nil
	}
	return map[string]any{"reasoning_effort": effort}
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

// maxTemplateExpansionSize is the maximum allowed size of a resolved template.
// Prevents OOM from extremely large input values injected into prompts.
const maxTemplateExpansionSize = 5 * 1024 * 1024 // 5 MB

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

		// Guard against excessive expansion from large input values.
		// Truncate at the limit rather than appending the remaining template.
		if b.Len() > maxTemplateExpansionSize {
			log.Printf("model: template expansion exceeded %d bytes, truncating", maxTemplateExpansionSize)
			break
		}
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

// ---------------------------------------------------------------------------
// Tool resolution helpers
// ---------------------------------------------------------------------------

// resolveToolsForNode resolves a list of tool names to goai.Tool instances for
// a specific node, ensuring that only tools from the node's active MCP servers
// are exposed. Wildcard entries like "mcp.<server>.*" are expanded to all tools
// discovered from that server.
func (e *GoaiExecutor) resolveToolsForNode(ctx context.Context, node *ir.Node, names []string) ([]goai.Tool, error) {
	// Expand wildcards (e.g. mcp.claude_code.*) into concrete tool names.
	expanded, err := e.expandWildcards(ctx, node, names)
	if err != nil {
		return nil, err
	}

	if err := e.ensureMCPServers(ctx, node, expanded); err != nil {
		return nil, err
	}

	var tools []goai.Tool
	for _, name := range expanded {
		t, ok, err := e.resolveSingleToolForNode(ctx, node, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if e.toolPolicy != nil {
			t = e.guardTool(t)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// expandWildcards replaces wildcard entries ("mcp.<server>.*") with the
// concrete tool names discovered from that MCP server.
func (e *GoaiExecutor) expandWildcards(ctx context.Context, node *ir.Node, names []string) ([]string, error) {
	var expanded []string
	for _, name := range names {
		if !tool.IsMCPWildcard(name) {
			expanded = append(expanded, name)
			continue
		}
		server, err := tool.ParseMCPWildcard(name)
		if err != nil {
			return nil, fmt.Errorf("model: invalid wildcard %q: %w", name, err)
		}
		// Ensure the server is connected so its tools are in the registry.
		if e.mcpManager != nil && e.toolRegistry != nil {
			if err := e.mcpManager.EnsureServers(ctx, e.toolRegistry, []string{server}); err != nil {
				return nil, fmt.Errorf("model: ensure MCP server %q for wildcard: %w", server, err)
			}
		}
		if e.toolRegistry == nil {
			return nil, fmt.Errorf("model: wildcard %q requires a tool registry", name)
		}
		serverTools := e.toolRegistry.ListByServer(server)
		if len(serverTools) == 0 {
			log.Printf("model: warning: wildcard %q matched no tools (server %q may not be started or has no tools)", name, server)
		}
		for _, td := range serverTools {
			expanded = append(expanded, td.QualifiedName)
		}
	}
	return expanded, nil
}

// resolveSingleToolForNode resolves one tool name in the context of a node.
func (e *GoaiExecutor) resolveSingleToolForNode(ctx context.Context, node *ir.Node, name string) (goai.Tool, bool, error) {
	if err := e.ensureMCPServers(ctx, node, []string{name}); err != nil {
		return goai.Tool{}, false, err
	}

	if e.toolRegistry == nil {
		return goai.Tool{}, false, fmt.Errorf("no tool registry configured")
	}

	td, err := e.toolRegistry.Resolve(name)
	if err != nil {
		return goai.Tool{}, false, err
	}
	if err := e.checkNodeToolAccess(node, td.QualifiedName); err != nil {
		return goai.Tool{}, false, err
	}
	return td.ToGoaiTool(), true, nil
}

func (e *GoaiExecutor) ensureMCPServers(ctx context.Context, node *ir.Node, names []string) error {
	if e.mcpManager == nil || e.toolRegistry == nil {
		return nil
	}
	servers := activeMCPServersForNames(node, names)
	if len(servers) == 0 {
		return nil
	}
	return e.mcpManager.EnsureServers(ctx, e.toolRegistry, servers)
}

func activeMCPServersForNames(node *ir.Node, names []string) []string {
	if node == nil || len(node.ActiveMCPServers) == 0 {
		return nil
	}
	active := make(map[string]struct{}, len(node.ActiveMCPServers))
	for _, server := range node.ActiveMCPServers {
		active[server] = struct{}{}
	}

	seen := make(map[string]struct{})
	var servers []string
	for _, name := range names {
		var server string
		// Support wildcard patterns like "mcp.claude_code.*".
		if tool.IsMCPWildcard(name) {
			s, err := tool.ParseMCPWildcard(name)
			if err != nil {
				continue
			}
			server = s
		} else {
			s, _, err := tool.ParseMCPName(name)
			if err != nil {
				continue
			}
			server = s
		}
		if _, ok := active[server]; !ok {
			continue
		}
		if _, ok := seen[server]; ok {
			continue
		}
		seen[server] = struct{}{}
		servers = append(servers, server)
	}
	return servers
}

func (e *GoaiExecutor) checkNodeToolAccess(node *ir.Node, qualified string) error {
	server, _, err := tool.ParseMCPName(qualified)
	if err != nil {
		return nil
	}
	if node == nil {
		return fmt.Errorf("model: MCP tool %q requires a node context", qualified)
	}
	if len(node.ActiveMCPServers) == 0 {
		return nil
	}
	for _, active := range node.ActiveMCPServers {
		if active == server {
			return nil
		}
	}
	return fmt.Errorf("model: node %q cannot access MCP tool %q because server %q is not active", node.ID, qualified, server)
}

// ---------------------------------------------------------------------------
// Policy guard
// ---------------------------------------------------------------------------

// guardTool wraps a goai.Tool's Execute function with a policy check.
// If the tool is denied, Execute returns an ErrToolDenied error without
// invoking the underlying implementation.
func (e *GoaiExecutor) guardTool(t goai.Tool) goai.Tool {
	original := t.Execute
	name := t.Name
	policy := e.toolPolicy
	t.Execute = func(ctx context.Context, input json.RawMessage) (string, error) {
		if err := policy.Check(name); err != nil {
			return "", err
		}
		return original(ctx, input)
	}
	return t
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
