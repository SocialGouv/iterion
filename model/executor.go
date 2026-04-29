package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"

	"github.com/SocialGouv/iterion/delegate"
	"github.com/SocialGouv/iterion/ir"
	iterlog "github.com/SocialGouv/iterion/log"
	"github.com/SocialGouv/iterion/mcp"
	"github.com/SocialGouv/iterion/tool"
)

// ErrCompactionUnsupported is the sentinel ClawExecutor.Compact returns
// when the backend has no in-process conversation handle to drop. The
// runtime re-exports it (runtime.ErrCompactionUnsupported is an alias)
// so the engine's `errors.Is` check works without importing model
// directly. Lives here because runtime imports model, not the reverse.
var ErrCompactionUnsupported = errors.New("model: compaction not supported by executor")

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

	// OnNodeFinished fires after a node's executor returns successfully.
	// The output map carries iterion's conventional usage keys (`_tokens`,
	// `_cost_usd`, `_model`) so observers (e.g. the Prometheus exporter)
	// can attribute cost and tokens per-node without re-parsing the event
	// log.
	OnNodeFinished func(nodeID string, output map[string]interface{})
}

// ChainHooks composes two EventHooks so callbacks registered on either
// side run in order (a then b) for every event. Either side may leave
// any callback nil; the result keeps the non-nil one without an extra
// closure.
func ChainHooks(a, b EventHooks) EventHooks {
	return EventHooks{
		OnLLMRequest: func() func(string, LLMRequestInfo) {
			if a.OnLLMRequest == nil {
				return b.OnLLMRequest
			}
			if b.OnLLMRequest == nil {
				return a.OnLLMRequest
			}
			return func(n string, i LLMRequestInfo) { a.OnLLMRequest(n, i); b.OnLLMRequest(n, i) }
		}(),
		OnLLMPrompt: func() func(string, string, string) {
			if a.OnLLMPrompt == nil {
				return b.OnLLMPrompt
			}
			if b.OnLLMPrompt == nil {
				return a.OnLLMPrompt
			}
			return func(n, s, u string) { a.OnLLMPrompt(n, s, u); b.OnLLMPrompt(n, s, u) }
		}(),
		OnLLMResponse: func() func(string, LLMResponseInfo) {
			if a.OnLLMResponse == nil {
				return b.OnLLMResponse
			}
			if b.OnLLMResponse == nil {
				return a.OnLLMResponse
			}
			return func(n string, i LLMResponseInfo) { a.OnLLMResponse(n, i); b.OnLLMResponse(n, i) }
		}(),
		OnLLMRetry: func() func(string, RetryInfo) {
			if a.OnLLMRetry == nil {
				return b.OnLLMRetry
			}
			if b.OnLLMRetry == nil {
				return a.OnLLMRetry
			}
			return func(n string, i RetryInfo) { a.OnLLMRetry(n, i); b.OnLLMRetry(n, i) }
		}(),
		OnLLMStepFinish: func() func(string, LLMStepInfo) {
			if a.OnLLMStepFinish == nil {
				return b.OnLLMStepFinish
			}
			if b.OnLLMStepFinish == nil {
				return a.OnLLMStepFinish
			}
			return func(n string, s LLMStepInfo) { a.OnLLMStepFinish(n, s); b.OnLLMStepFinish(n, s) }
		}(),
		OnToolCall: func() func(string, LLMToolCallInfo) {
			if a.OnToolCall == nil {
				return b.OnToolCall
			}
			if b.OnToolCall == nil {
				return a.OnToolCall
			}
			return func(n string, i LLMToolCallInfo) { a.OnToolCall(n, i); b.OnToolCall(n, i) }
		}(),
		OnToolNodeResult: func() func(string, string, []byte, string, time.Duration, error) {
			if a.OnToolNodeResult == nil {
				return b.OnToolNodeResult
			}
			if b.OnToolNodeResult == nil {
				return a.OnToolNodeResult
			}
			return func(n, t string, in []byte, out string, e time.Duration, err error) {
				a.OnToolNodeResult(n, t, in, out, e, err)
				b.OnToolNodeResult(n, t, in, out, e, err)
			}
		}(),
		OnDelegateStarted: func() func(string, string) {
			if a.OnDelegateStarted == nil {
				return b.OnDelegateStarted
			}
			if b.OnDelegateStarted == nil {
				return a.OnDelegateStarted
			}
			return func(n, bn string) { a.OnDelegateStarted(n, bn); b.OnDelegateStarted(n, bn) }
		}(),
		OnDelegateFinished: func() func(string, DelegateInfo) {
			if a.OnDelegateFinished == nil {
				return b.OnDelegateFinished
			}
			if b.OnDelegateFinished == nil {
				return a.OnDelegateFinished
			}
			return func(n string, i DelegateInfo) { a.OnDelegateFinished(n, i); b.OnDelegateFinished(n, i) }
		}(),
		OnDelegateError: func() func(string, DelegateInfo) {
			if a.OnDelegateError == nil {
				return b.OnDelegateError
			}
			if b.OnDelegateError == nil {
				return a.OnDelegateError
			}
			return func(n string, i DelegateInfo) { a.OnDelegateError(n, i); b.OnDelegateError(n, i) }
		}(),
		OnDelegateRetry: func() func(string, DelegateInfo) {
			if a.OnDelegateRetry == nil {
				return b.OnDelegateRetry
			}
			if b.OnDelegateRetry == nil {
				return a.OnDelegateRetry
			}
			return func(n string, i DelegateInfo) { a.OnDelegateRetry(n, i); b.OnDelegateRetry(n, i) }
		}(),
		OnNodeFinished: func() func(string, map[string]interface{}) {
			if a.OnNodeFinished == nil {
				return b.OnNodeFinished
			}
			if b.OnNodeFinished == nil {
				return a.OnNodeFinished
			}
			return func(n string, o map[string]interface{}) { a.OnNodeFinished(n, o); b.OnNodeFinished(n, o) }
		}(),
	}
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
	Backend   string                 // delegate backend name (empty for claw direct)
}

func (e *ErrNeedsInteraction) Error() string {
	return fmt.Sprintf("model: node %q needs user interaction (%d questions)", e.NodeID, len(e.Questions))
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// ClawExecutor implements runtime.NodeExecutor by routing LLM calls
// through pluggable Backend implementations (claw, claude_code, codex, etc.).
type ClawExecutor struct {
	registry        *Registry
	backendRegistry *delegate.Registry // backend registry (claw, claude_code, codex)
	toolRegistry    *tool.Registry     // unified tool registry (preferred)
	mcpManager      *mcp.Manager       // generic MCP discovery/call bridge
	toolPolicy      tool.ToolChecker   // allowlist policy for tool execution (nil = open)
	prompts         map[string]*ir.Prompt
	schemas         map[string]*ir.Schema
	vars            map[string]interface{}
	hooks           EventHooks
	retry           RetryPolicy
	logger          *iterlog.Logger
	workDir         string // working directory for backend subprocesses
	defaultBackend  string // workflow-level default backend (empty = use "claw")
	lifecycleHooks  *hooks.Runner

	// sessions holds per-(runID, nodeID) accumulated message lists
	// so the recovery dispatcher's CompactAndRetry path has
	// something to actually compact. The claw backend reads this
	// store via ctx values plumbed by executeBackend.
	sessions *nodeSessionStore
	// currentRunID is set by executeNode at the top of each call
	// and cleared at the bottom. Compact reads it because the
	// runtime.Compactor structural interface only carries nodeID.
	currentRunID string
}

// ClawExecutorOption configures a ClawExecutor.
type ClawExecutorOption func(*ClawExecutor)

// WithEventHooks sets observability callbacks on the executor.
func WithEventHooks(h EventHooks) ClawExecutorOption {
	return func(e *ClawExecutor) { e.hooks = h }
}

// WithToolRegistry sets the unified tool registry on the executor.
func WithToolRegistry(tr *tool.Registry) ClawExecutorOption {
	return func(e *ClawExecutor) { e.toolRegistry = tr }
}

// WithMCPManager sets the generic MCP manager used to lazily discover MCP tools.
func WithMCPManager(m *mcp.Manager) ClawExecutorOption {
	return func(e *ClawExecutor) { e.mcpManager = m }
}

// WithToolPolicy sets the tool execution policy on the executor.
// When set, every tool call is checked against the allowlist before
// execution. A denied tool produces an explicit error.
func WithToolPolicy(p tool.ToolChecker) ClawExecutorOption {
	return func(e *ClawExecutor) { e.toolPolicy = p }
}

// WithRetryPolicy sets the retry policy for transient LLM errors.
func WithRetryPolicy(rp RetryPolicy) ClawExecutorOption {
	return func(e *ClawExecutor) { e.retry = rp }
}

// WithBackendRegistry sets the backend registry on the executor.
// When set, nodes with a `backend` property are executed via the named
// backend instead of the default claw backend.
func WithBackendRegistry(dr *delegate.Registry) ClawExecutorOption {
	return func(e *ClawExecutor) { e.backendRegistry = dr }
}

// WithWorkDir sets the working directory for backend subprocesses.
// When set, backend nodes will run their CLI in this directory.
func WithWorkDir(dir string) ClawExecutorOption {
	return func(e *ClawExecutor) { e.workDir = dir }
}

// WithDefaultBackend sets the workflow-level default backend.
func WithDefaultBackend(name string) ClawExecutorOption {
	return func(e *ClawExecutor) { e.defaultBackend = name }
}

// WithLogger sets a leveled logger for the executor.
func WithLogger(l *iterlog.Logger) ClawExecutorOption {
	return func(e *ClawExecutor) { e.logger = l }
}

// WithLifecycleHooks installs an in-process lifecycle hook runner.
// When set, the runner is consulted around every tool execution
// (PreToolUse, PostToolUse, PostToolUseFailure) and at session end
// (Stop). Build the runner once via hooks.NewRunner, register
// callbacks with runner.Register(event, handler), then pass it here.
//
// A nil runner disables the integration (default).
func WithLifecycleHooks(r *hooks.Runner) ClawExecutorOption {
	return func(e *ClawExecutor) { e.lifecycleHooks = r }
}

// LifecycleHooks returns the runner installed via WithLifecycleHooks
// (nil if none). It is intended for backends that need to forward the
// runner into their own generation paths.
func (e *ClawExecutor) LifecycleHooks() *hooks.Runner {
	return e.lifecycleHooks
}

// EvictRun drops every per-node session belonging to the given run.
// The runtime engine calls this when a run terminates (success,
// terminal failure, or cancellation) so a long-lived executor
// shared across runs does not leak session state from failed nodes.
func (e *ClawExecutor) EvictRun(runID string) {
	if e.sessions != nil {
		e.sessions.evictRun(runID)
	}
}

// Compact satisfies the runtime.Compactor structural interface.
//
// ClawExecutor maintains a session-per-node store of messages
// accumulated during the previous attempt. On Compact, the pure
// CompactSessionPure helper from claw-code-go is applied to that
// list — the next retry's claw backend prepends the (now smaller)
// list to its opts.Messages so the LLM sees a summarised history
// instead of the full pre-overflow conversation.
//
// When no session is wired (non-claw backends, or a node that has
// never been executed) Compact returns ErrCompactionUnsupported, the
// recovery dispatcher logs the gap, and the retry runs without
// special treatment — the same behaviour as before session tracking
// existed.
func (e *ClawExecutor) Compact(ctx context.Context, nodeID string) error {
	if e.sessions == nil {
		return fmt.Errorf("claw executor (node %q): %w", nodeID, ErrCompactionUnsupported)
	}
	runID := RunIDFromContext(ctx)
	if runID == "" {
		return fmt.Errorf("claw executor (node %q): no run ID in context: %w", nodeID, ErrCompactionUnsupported)
	}
	removed, fired := e.sessions.compact(runID, nodeID, clawrt.DefaultCompactionConfig())
	if !fired {
		// Either no session for this node yet (non-claw backend, first
		// attempt) or the session was already small enough to skip.
		return fmt.Errorf("claw executor (node %q): nothing to compact: %w", nodeID, ErrCompactionUnsupported)
	}
	if e.logger != nil {
		e.logger.Info("recovery: compacted node %q session (%d messages dropped)", nodeID, removed)
	}
	return nil
}

// NewClawExecutor creates a ClawExecutor for a given workflow.
func NewClawExecutor(registry *Registry, wf *ir.Workflow, opts ...ClawExecutorOption) *ClawExecutor {
	e := &ClawExecutor{
		registry:       registry,
		prompts:        wf.Prompts,
		schemas:        wf.Schemas,
		defaultBackend: wf.DefaultBackend,
		sessions:       newNodeSessionStore(),
	}
	for _, opt := range opts {
		opt(e)
	}

	if e.backendRegistry == nil {
		e.backendRegistry = delegate.NewRegistry()
	}

	return e
}

// MCPHealthCheck verifies that the listed MCP servers are reachable by
// connecting and sending an MCP ping. Should be called before execution
// starts to fail fast on misconfigured servers.
func (e *ClawExecutor) MCPHealthCheck(ctx context.Context, servers []string) error {
	if e.mcpManager == nil {
		return nil
	}
	return e.mcpManager.HealthCheck(ctx, servers)
}

// Close releases resources held by the executor, including MCP server
// connections. It should be called when the executor is no longer needed.
func (e *ClawExecutor) Close() error {
	if e.mcpManager != nil {
		return e.mcpManager.Close()
	}
	return nil
}

// SetVars sets the workflow variables for the current run.
// Must be called before Execute.
func (e *ClawExecutor) SetVars(vars map[string]interface{}) {
	e.vars = vars
}

// resolveBackendName returns the effective backend name for a node.
// Resolution chain: node.Backend → workflow default → env ITERION_DEFAULT_BACKEND → "claw".
func (e *ClawExecutor) resolveBackendName(node ir.Node) string {
	var backend string
	switch n := node.(type) {
	case *ir.AgentNode:
		backend = n.Backend
	case *ir.JudgeNode:
		backend = n.Backend
	case *ir.RouterNode:
		backend = n.Backend
	}
	if backend != "" {
		return backend
	}
	if e.defaultBackend != "" {
		return e.defaultBackend
	}
	if env := os.Getenv("ITERION_DEFAULT_BACKEND"); env != "" {
		return env
	}
	return delegate.BackendClaw
}

// Execute implements runtime.NodeExecutor.
func (e *ClawExecutor) Execute(ctx context.Context, node ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	// Promote the engine-supplied run ID into the richer
	// runtimeContext that backends read for session-aware retries.
	runID := RunIDFromContext(ctx)
	if runID != "" && e.sessions != nil {
		ctx = withRuntimeContext(ctx, runID, e.sessions)
	}

	output, err := e.executeNode(ctx, node, input)
	if err == nil {
		// Successful node completion: drop any session messages so
		// the store doesn't grow without bound across long runs.
		// Sessions are preserved on error so the recovery dispatcher
		// has something to compact for the next attempt.
		if e.sessions != nil && runID != "" {
			e.sessions.evict(runID, node.NodeID())
		}
		if output != nil && e.hooks.OnNodeFinished != nil {
			e.hooks.OnNodeFinished(node.NodeID(), output)
		}
	}
	return output, err
}

func (e *ClawExecutor) executeNode(ctx context.Context, node ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	switch n := node.(type) {
	case *ir.AgentNode:
		return e.executeBackend(ctx, n, input)
	case *ir.JudgeNode:
		return e.executeBackend(ctx, n, input)
	case *ir.HumanNode:
		return e.executeHumanLLM(ctx, n, input)
	case *ir.RouterNode:
		if n.RouterMode == ir.RouterLLM {
			return e.executeLLMRouterUnified(ctx, n, input)
		}
		// Deterministic routers are pass-throughs handled by the engine.
		return input, nil
	case *ir.ToolNode:
		return e.executeToolNode(ctx, n, input)
	default:
		return nil, fmt.Errorf("model: unsupported node kind %q for execution", node.NodeKind())
	}
}

// backendFields holds the common fields extracted from AgentNode or JudgeNode
// for the executeBackend unified path.
type backendFields struct {
	id               string
	model            string
	backend          string
	systemPrompt     string
	userPrompt       string
	reasoningEffort  string
	outputSchema     string
	tools            []string
	toolMaxSteps     int
	maxTokens        int
	session          ir.SessionMode
	interaction      ir.InteractionMode
	activeMCPServers []string
}

func extractBackendFields(node ir.Node) backendFields {
	switch n := node.(type) {
	case *ir.AgentNode:
		return backendFields{
			id: n.ID, model: n.Model, backend: n.Backend,
			systemPrompt: n.SystemPrompt, userPrompt: n.UserPrompt,
			reasoningEffort: n.ReasoningEffort, outputSchema: n.OutputSchema,
			tools: n.Tools, toolMaxSteps: n.ToolMaxSteps,
			maxTokens:        n.MaxTokens,
			session:          n.Session,
			interaction:      n.Interaction,
			activeMCPServers: n.ActiveMCPServers,
		}
	case *ir.JudgeNode:
		return backendFields{
			id: n.ID, model: n.Model, backend: n.Backend,
			systemPrompt: n.SystemPrompt, userPrompt: n.UserPrompt,
			reasoningEffort: n.ReasoningEffort, outputSchema: n.OutputSchema,
			tools: n.Tools, toolMaxSteps: n.ToolMaxSteps,
			maxTokens:        n.MaxTokens,
			session:          n.Session,
			interaction:      n.Interaction,
			activeMCPServers: n.ActiveMCPServers,
		}
	default:
		panic(fmt.Sprintf("model: extractBackendFields called with unsupported node type %T", node))
	}
}

// executeBackend is the unified execution path for agent and judge nodes.
// It resolves the backend, builds a Task, and dispatches to the backend.
func (e *ClawExecutor) executeBackend(ctx context.Context, node ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	f := extractBackendFields(node)
	backendName := e.resolveBackendName(node)

	if e.backendRegistry == nil {
		return nil, fmt.Errorf("model: node %q uses backend %q but no backend registry configured", f.id, backendName)
	}

	backend, err := e.backendRegistry.Resolve(backendName)
	if err != nil {
		return nil, fmt.Errorf("model: node %q: %w", f.id, err)
	}

	// Build system prompt.
	var systemText string
	if f.systemPrompt != "" {
		if p, ok := e.prompts[f.systemPrompt]; ok {
			systemText = e.resolveTemplate(p.Body, input)
		}
	}

	// Build user message.
	userText := e.buildUserMessage(f.userPrompt, input)

	// Emit prompt content for observability.
	if e.hooks.OnLLMPrompt != nil {
		e.hooks.OnLLMPrompt(f.id, systemText, userText)
	}

	// Build output schema JSON if structured output is expected.
	var outputSchema json.RawMessage
	if f.outputSchema != "" {
		if schema, ok := e.schemas[f.outputSchema]; ok {
			outputSchema, _ = SchemaToJSON(schema)
		}
	}

	effort := resolveReasoningEffort(f.reasoningEffort, input)

	task := delegate.Task{
		NodeID:             f.id,
		SystemPrompt:       systemText,
		UserPrompt:         userText,
		AllowedTools:       f.tools,
		OutputSchema:       outputSchema,
		Model:              os.ExpandEnv(f.model),
		HasTools:           len(f.tools) > 0,
		ToolMaxSteps:       f.toolMaxSteps,
		MaxTokens:          f.maxTokens,
		WorkDir:            e.workDir,
		ReasoningEffort:    effort,
		InteractionEnabled: f.interaction != ir.InteractionNone,
	}

	// Resolve full tool definitions for backends that manage tool loops
	// internally (claw). CLI-based backends (claude_code, codex) handle tools
	// natively via AllowedTools and do not need ToolDefs.
	if len(f.tools) > 0 && backendName == delegate.BackendClaw {
		toolDefs, toolErr := e.resolveToolsForNode(ctx, node, f.tools)
		if toolErr != nil {
			return nil, fmt.Errorf("model: node %q: %w", f.id, toolErr)
		}
		task.ToolDefs = toolDefs
	}

	// Session continuity.
	if f.session == ir.SessionInherit || f.session == ir.SessionFork {
		if sid, ok := input["_session_id"].(string); ok && sid != "" {
			task.SessionID = sid
			if f.session == ir.SessionFork {
				task.ForkSession = true
			}
		}
	}

	// Emit backend started event.
	if e.hooks.OnDelegateStarted != nil {
		e.hooks.OnDelegateStarted(f.id, backendName)
	}

	result, err := e.retryDelegateLoop(ctx, f.id, backendName, func() (delegate.Result, error) {
		return backend.Execute(ctx, task)
	})
	if err != nil {
		if e.hooks.OnDelegateError != nil {
			bn := result.BackendName
			if bn == "" {
				bn = backendName
			}
			e.hooks.OnDelegateError(f.id, DelegateInfo{
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
		return nil, fmt.Errorf("model: node %q: backend %q failed: %w", f.id, backendName, err)
	}

	// Emit backend finished event.
	if e.hooks.OnDelegateFinished != nil {
		e.hooks.OnDelegateFinished(f.id, DelegateInfo{
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
	if f.outputSchema != "" {
		if schema, ok := e.schemas[f.outputSchema]; ok {
			if err := ValidateOutput(result.Output, schema); err != nil {
				// If parsing fell back to text wrapper, the backend likely
				// returned non-JSON output (transient SDK issue). Retry once
				// before giving up.
				if result.ParseFallback {
					e.logger.Warn("node %q: structured output validation failed with parse fallback, retrying backend: %v", f.id, err)
					retryResult, retryErr := backend.Execute(ctx, task)
					if retryErr == nil && !retryResult.ParseFallback {
						result = retryResult
						// Re-attach metadata and re-validate.
						if result.Output["_tokens"] == nil {
							result.Output["_tokens"] = result.Tokens
						}
						result.Output["_backend"] = backendName
						if result.SessionID != "" {
							result.Output["_session_id"] = result.SessionID
						}
						if retryValErr := ValidateOutput(result.Output, schema); retryValErr != nil {
							return nil, fmt.Errorf("model: node %q: structured output invalid after retry: %w", f.id, retryValErr)
						}
						goto validated
					}
				}
				return nil, fmt.Errorf("model: node %q: structured output invalid: %w", f.id, err)
			}
		}
	}
validated:

	// Check if the backend signaled that it needs user interaction.
	if f.interaction != ir.InteractionNone {
		if needsInteraction, ok := result.Output["_needs_interaction"].(bool); ok && needsInteraction {
			questions, _ := result.Output["_interaction_questions"].(map[string]interface{})
			if questions == nil {
				questions = map[string]interface{}{"input": "The backend needs your input to continue."}
			}
			delete(result.Output, "_needs_interaction")
			delete(result.Output, "_interaction_questions")
			return nil, &ErrNeedsInteraction{
				NodeID:    f.id,
				Questions: questions,
				SessionID: result.SessionID,
				Backend:   backendName,
			}
		}
	}

	return result.Output, nil
}

// executeHumanLLM handles human nodes in llm or llm_or_human interaction mode.
// It calls GenerateObjectDirect against api.APIClient with mode-specific
// schema handling for llm_or_human (wrapper schema with needs_human_input).
func (e *ClawExecutor) executeHumanLLM(ctx context.Context, node *ir.HumanNode, input map[string]interface{}) (map[string]interface{}, error) {
	if node.Interaction == ir.InteractionHuman || node.Interaction == ir.InteractionNone {
		return nil, fmt.Errorf("model: human node %q in %s interaction mode should not be executed by the model layer", node.ID, node.Interaction)
	}

	// Resolve API client (expand env var references).
	modelSpec := os.ExpandEnv(node.Model)
	client, err := e.registry.Resolve(modelSpec)
	if err != nil {
		return nil, fmt.Errorf("model: human node %q: %w", node.ID, err)
	}

	// Build GenerationOptions.
	genOpts := GenerationOptions{
		Model: modelSpec,
	}

	// Reasoning effort (dynamic override from input, then static node property).
	if popts := providerOptsForNode(resolveReasoningEffort("", input)); popts != nil {
		genOpts.ProviderOptions = popts
	}

	// System prompt.
	var systemText string
	if node.SystemPrompt != "" {
		if p, ok := e.prompts[node.SystemPrompt]; ok {
			systemText = e.resolveTemplate(p.Body, input)
			genOpts.System = systemText
		}
	}

	// User message from input.
	userText := e.buildUserMessage("", input)

	// Emit prompt content for observability.
	if e.hooks.OnLLMPrompt != nil {
		e.hooks.OnLLMPrompt(node.ID, systemText, userText)
	}

	if userText != "" {
		genOpts.Messages = []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: userText}}},
		}
	}

	// Observability hooks.
	applyHooks(node.ID, e.hooks, &genOpts)

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
	genOpts.ExplicitSchema = jsonSchema

	result, err := GenerateObjectDirect[map[string]interface{}](ctx, client, genOpts)
	if err != nil {
		return nil, fmt.Errorf("model: human node %q: structured generation: %w", node.ID, err)
	}

	output := result.Object
	if output == nil {
		output = make(map[string]interface{})
	}

	// Attach usage metadata.
	output["_tokens"] = result.TotalUsage.InputTokens + result.TotalUsage.OutputTokens
	output["_model"] = modelSpec

	return output, nil
}

// ExecuteHumanLLMForInteraction handles delegate interaction requests by
// creating a synthetic HumanNode from the original node's InteractionFields
// and calling executeHumanLLM. The questions from the ErrNeedsInteraction
// become the input, and the interaction schema is synthesized from the
// question keys.
//
// Returns:
//   - answers: LLM-generated answers for each question
//   - needsHuman: true if the LLM decided to escalate (llm_or_human mode only)
//   - err: any error from model execution
func (e *ClawExecutor) ExecuteHumanLLMForInteraction(
	ctx context.Context,
	nodeID string,
	ni *ErrNeedsInteraction,
	fields ir.InteractionFields,
) (answers map[string]interface{}, needsHuman bool, err error) {
	// Build synthetic schema from question keys.
	schemaFields := make([]*ir.SchemaField, 0, len(ni.Questions))
	for key := range ni.Questions {
		sanitized := sanitizeSchemaKey(key)
		schemaFields = append(schemaFields, &ir.SchemaField{
			Name: sanitized,
			Type: ir.FieldTypeString,
		})
	}
	syntheticSchema := &ir.Schema{
		Name:   nodeID + "_interaction",
		Fields: schemaFields,
	}

	// Register the synthetic schema so executeHumanLLM can find it.
	schemaName := syntheticSchema.Name
	e.schemas[schemaName] = syntheticSchema

	// Build synthetic HumanNode.
	node := &ir.HumanNode{
		BaseNode: ir.BaseNode{ID: nodeID + "_interaction"},
		SchemaFields: ir.SchemaFields{
			OutputSchema: schemaName,
		},
		InteractionFields: fields,
		Model:             fields.InteractionModel,
		SystemPrompt:      fields.InteractionPrompt,
	}

	// Build input from questions (question_key → question text).
	input := make(map[string]interface{}, len(ni.Questions))
	for k, v := range ni.Questions {
		input[sanitizeSchemaKey(k)] = v
	}

	output, err := e.executeHumanLLM(ctx, node, input)
	if err != nil {
		return nil, false, fmt.Errorf("model: interaction LLM for node %q: %w", nodeID, err)
	}

	// Check if the LLM decided to escalate (llm_or_human mode).
	if v, ok := output["needs_human_input"]; ok {
		if b, ok := v.(bool); ok {
			needsHuman = b
		}
		delete(output, "needs_human_input")
	}

	// Strip metadata keys.
	delete(output, "_tokens")
	delete(output, "_model")

	return output, needsHuman, nil
}

// sanitizeSchemaKey replaces characters that are invalid in JSON Schema
// property names with underscores. This ensures question keys containing
// special characters (spaces, dots, etc.) produce valid schema fields.
func sanitizeSchemaKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	result := b.String()
	if result == "" {
		return "input"
	}
	return result
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

// isRetryable returns true if err is a transient LLM API error that should be
// retried. Recognises both iterion's local *APIError (used for stream-decoded
// errors) and claw-code-go's *clawapi.APIError (returned by provider HTTP
// clients on non-2xx responses, e.g. 429 / 5xx).
func isRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsRetryable
	}
	var clawErr *api.APIError
	if errors.As(err, &clawErr) {
		return clawErr.IsRetryable()
	}
	return false
}

// statusCodeOf extracts the HTTP status code from a recognised API error
// type, or 0 when the error is not an API error.
func statusCodeOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	var clawErr *api.APIError
	if errors.As(err, &clawErr) {
		return clawErr.StatusCode
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
func (e *ClawExecutor) retryDelegateLoop(ctx context.Context, nodeID string, backendName string, fn func() (delegate.Result, error)) (delegate.Result, error) {
	maxAttempts := e.retry.maxAttempts()

	result, err := fn()
	for attempt := 1; err != nil && isDelegateRetryable(err) && attempt < maxAttempts; attempt++ {
		delay := e.retry.backoff(attempt - 1)

		e.logger.Warn("node %q: delegate retry %d/%d after error: %v (backoff %s)",
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
func (e *ClawExecutor) executeToolNode(ctx context.Context, node *ir.ToolNode, input map[string]interface{}) (map[string]interface{}, error) {
	// When the command contains template refs ({{input.X}}) or looks like a
	// shell command (contains spaces or shell operators), execute as a direct
	// shell command. Otherwise, use the tool registry.
	if len(node.CommandRefs) > 0 || looksLikeShellCommand(node.Command) {
		return e.executeToolNodeShell(ctx, node, input)
	}

	toolName := node.Command

	// Policy check before resolution — fail fast on denied tools.
	if e.toolPolicy != nil {
		pctx := tool.PolicyContext{
			Ctx:      ctx,
			NodeID:   node.ID,
			NodeKind: ir.NodeTool.String(),
			ToolName: toolName,
			Vars:     e.vars,
		}
		if err := e.toolPolicy.CheckContext(pctx); err != nil {
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
func (e *ClawExecutor) executeToolNodeShell(ctx context.Context, node *ir.ToolNode, input map[string]interface{}) (map[string]interface{}, error) {
	// Resolve template references in the command.
	resolved := resolveCommandTemplate(node.Command, node.CommandRefs, input, e.vars)

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

// looksLikeShellCommand returns true if the command string looks like a shell
// command rather than a bare tool name. Tool names are simple identifiers
// (e.g. "read_file", "bash"), while shell commands contain spaces, operators,
// or path separators.
func looksLikeShellCommand(cmd string) bool {
	return strings.ContainsAny(cmd, " \t|&;><$`(){}\"'/")
}

// resolveCommandTemplate substitutes {{input.X}} and {{vars.X}} references in
// a command string with values from the input map and workflow variables.
// Values are shell-escaped to prevent command injection when the resolved
// string is passed to sh -c.
func resolveCommandTemplate(command string, refs []*ir.Ref, input map[string]interface{}, vars map[string]interface{}) string {
	resolved := command
	for _, ref := range refs {
		var val interface{}
		switch {
		case ref.Kind == ir.RefInput && len(ref.Path) > 0:
			val = input[ref.Path[0]]
		case ref.Kind == ir.RefVars && len(ref.Path) > 0:
			val = vars[ref.Path[0]]
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
func buildRouterSchema(node *ir.RouterNode, candidates []string) *ir.Schema {
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
func (e *ClawExecutor) executeLLMRouterUnified(ctx context.Context, node *ir.RouterNode, input map[string]interface{}) (map[string]interface{}, error) {
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
	userText := e.buildUserMessage(node.UserPrompt, cleanInput)

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
		ReasoningEffort: resolveReasoningEffort(node.ReasoningEffort, input),
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
func resolveReasoningEffort(nodeEffort string, input map[string]interface{}) string {
	if v, ok := input["_reasoning_effort"]; ok {
		if s, ok := v.(string); ok && ir.ValidReasoningEfforts[s] {
			return s
		}
	}
	return nodeEffort
}

// providerOptsForNode builds the ProviderOptions map from the resolved
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
// userPrompt is the prompt reference name from the node (empty if not set).
func (e *ClawExecutor) buildUserMessage(userPrompt string, input map[string]interface{}) string {
	// If the node has a user prompt template, resolve it.
	if userPrompt != "" {
		if p, ok := e.prompts[userPrompt]; ok {
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
func (e *ClawExecutor) resolveTemplate(body string, input map[string]interface{}) string {
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
			e.logger.Warn("template expansion exceeded %d bytes, truncating", maxTemplateExpansionSize)
			break
		}
	}

	return b.String()
}

// resolveTemplateRef resolves a single "namespace.path" reference.
// Returns the resolved value and true, or ("", false) if unresolvable.
func (e *ClawExecutor) resolveTemplateRef(ref string, input map[string]interface{}) (string, bool) {
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

// nodeActiveMCPServers delegates to ir.NodeActiveMCPServers.
var nodeActiveMCPServers = ir.NodeActiveMCPServers

// resolveToolsForNode resolves a list of tool names to delegate.ToolDef
// instances for a specific node, ensuring that only tools from the node's
// active MCP servers are exposed. Wildcard entries like "mcp.<server>.*"
// are expanded to all tools discovered from that server.
func (e *ClawExecutor) resolveToolsForNode(ctx context.Context, node ir.Node, names []string) ([]delegate.ToolDef, error) {
	// Expand wildcards (e.g. mcp.claude_code.*) into concrete tool names.
	expanded, err := e.expandWildcards(ctx, node, names)
	if err != nil {
		return nil, err
	}

	if err := e.ensureMCPServers(ctx, node, expanded); err != nil {
		return nil, err
	}

	var tools []delegate.ToolDef
	for _, name := range expanded {
		t, ok, err := e.resolveSingleToolForNode(ctx, node, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if e.toolPolicy != nil {
			t = e.guardTool(t, node)
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// expandWildcards replaces wildcard entries ("mcp.<server>.*") with the
// concrete tool names discovered from that MCP server.
func (e *ClawExecutor) expandWildcards(ctx context.Context, node ir.Node, names []string) ([]string, error) {
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
			e.logger.Warn("wildcard %q matched no tools (server %q may not be started or has no tools)", name, server)
		}
		for _, td := range serverTools {
			expanded = append(expanded, td.QualifiedName)
		}
	}
	return expanded, nil
}

// resolveSingleToolForNode resolves one tool name in the context of a node.
func (e *ClawExecutor) resolveSingleToolForNode(ctx context.Context, node ir.Node, name string) (delegate.ToolDef, bool, error) {
	if err := e.ensureMCPServers(ctx, node, []string{name}); err != nil {
		return delegate.ToolDef{}, false, err
	}

	if e.toolRegistry == nil {
		return delegate.ToolDef{}, false, fmt.Errorf("no tool registry configured")
	}

	td, err := e.toolRegistry.Resolve(name)
	if err != nil {
		return delegate.ToolDef{}, false, err
	}
	if err := e.checkNodeToolAccess(node, td.QualifiedName); err != nil {
		return delegate.ToolDef{}, false, err
	}
	return td.ToDelegateDef(), true, nil
}

func (e *ClawExecutor) ensureMCPServers(ctx context.Context, node ir.Node, names []string) error {
	if e.mcpManager == nil || e.toolRegistry == nil {
		return nil
	}
	servers := activeMCPServersForNames(node, names)
	if len(servers) == 0 {
		return nil
	}
	return e.mcpManager.EnsureServers(ctx, e.toolRegistry, servers)
}

func activeMCPServersForNames(node ir.Node, names []string) []string {
	mcpServers := nodeActiveMCPServers(node)
	if node == nil || len(mcpServers) == 0 {
		return nil
	}
	active := make(map[string]struct{}, len(mcpServers))
	for _, server := range mcpServers {
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

func (e *ClawExecutor) checkNodeToolAccess(node ir.Node, qualified string) error {
	server, _, err := tool.ParseMCPName(qualified)
	if err != nil {
		return nil
	}
	if node == nil {
		return fmt.Errorf("model: MCP tool %q requires a node context", qualified)
	}
	mcpServers := nodeActiveMCPServers(node)
	if len(mcpServers) == 0 {
		return nil
	}
	for _, active := range mcpServers {
		if active == server {
			return nil
		}
	}
	return fmt.Errorf("model: node %q cannot access MCP tool %q because server %q is not active", node.NodeID(), qualified, server)
}

// ---------------------------------------------------------------------------
// Policy guard
// ---------------------------------------------------------------------------

// guardTool wraps a tool's Execute function with a policy check.
// If the tool is denied, Execute returns an ErrToolDenied error without
// invoking the underlying implementation.
func (e *ClawExecutor) guardTool(t delegate.ToolDef, node ir.Node) delegate.ToolDef {
	original := t.Execute
	name := t.Name
	policy := e.toolPolicy
	nodeID := node.NodeID()
	nodeKind := node.NodeKind().String()
	vars := e.vars
	t.Execute = func(ctx context.Context, input json.RawMessage) (string, error) {
		pctx := tool.PolicyContext{
			Ctx:      ctx,
			NodeID:   nodeID,
			NodeKind: nodeKind,
			ToolName: name,
			Input:    input,
			Vars:     vars,
		}
		if err := policy.CheckContext(pctx); err != nil {
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
