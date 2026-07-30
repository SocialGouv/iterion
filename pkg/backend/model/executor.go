package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/rewrite"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/backend/tooldisplay"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

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
	cursors         map[string]*ir.CursorDef
	imageAttachs    map[string]bool // names of image-typed attachments declared in the workflow
	vars            map[string]any
	presetPrompt    string   // selected preset's "## Focus" bias, {{vars}}-templated per node
	presetSkills    []string // selected preset's relevant-skill hint names
	hooks           EventHooks
	retry           RetryPolicy
	logger          *iterlog.Logger
	workDir         string // working directory for backend subprocesses
	repoRoot        string // source-of-truth repo path (project-rooted memory uses this)
	defaultBackend  string // workflow-level default backend (empty = use "claw")
	// modelOverrides are launch-time per-node/-group backend+model+provider
	// overrides (studio Launch dropdowns, CLI --model/--backend). They sit at
	// the TOP of the resolution chain — above the node's DSL backend:/model: —
	// so a run can re-target the bot without editing the .bot. Empty applies
	// nothing (backward-compatible). See model_override.go.
	modelOverrides ModelOverrides
	wfCompaction   *ir.Compaction
	wfCapabilities []string // workflow-level default host capabilities (nil = none)
	wfSkills       []string // workflow-level default skill-library references (nil = none)
	// skillHints maps a skill-library name to its description for every skill
	// referenced by the workflow that RESOLVED in the library at run start
	// (set by SetSkillHints from the runtime mirror). Per-node, the executor
	// renders the "## Skills" section for the subset this node references.
	skillHints map[string]string
	// mirroredSkills are the skill directories iterion wrote into the
	// workspace this run (SetMirroredSkills), as opposed to whatever the
	// target repo ships at the same path.
	mirroredSkills []string
	botID          string // stable bot/workflow id used for bot-scoped memory
	storeDir       string // dispatcher store root (empty = backend default)
	// artifactFilesDir is the run's tool-output scratch area
	// (runs/<id>/artifact_files), exported to HOST tool-node subprocesses as
	// ITERION_ARTIFACT_FILES_DIR. Sandboxed runs already get the variable from
	// the container env (pkg/runtime/sandbox.go bind-mounts the same dir);
	// without this the variable only existed in-sandbox, so a bot writing its
	// outputs there worked sandboxed but broke on a plain local run.
	artifactFilesDir string
	lifecycleHooks   *hooks.Runner

	// Command-output compression (the rewriter plugin chain). wfCompress is
	// the workflow-level `compress:` DSL value; compressOverride is the
	// run-level override (CLI --compress / studio Launch); compressEnvDefault
	// is ITERION_COMPRESS, read once at construction instead of per node. All
	// three feed rewrite.Resolve to compute each node's effective mode
	// (precedence: override > node DSL > workflow DSL > env). chain is the
	// ordered set of enabled rewriters (rtk by default), wired from the plugin
	// registry; nil when no rewriter plugin is enabled (compression off).
	wfCompress         string
	compressOverride   string
	compressEnvDefault string
	chain              *rewrite.Chain

	// Tool-permission gate (anti-prompt-injection boundary). wfPermission
	// + wfPerm{Allow,Ask,Deny} are the workflow-level `permission:` DSL
	// values; permOverride / permRuleOverride are the run-level CLI/studio
	// override (--permission, --permission-allow/ask/deny); permEnvDefault
	// is ITERION_PERMISSION read once at construction. Mode precedence:
	// override > node DSL > workflow DSL > env > off. Rule lists are
	// additive (workflow + override). See resolvePermissionPolicy.
	wfPermission   string
	wfPermAllow    []string
	wfPermAsk      []string
	wfPermDeny     []string
	permOverride   string
	permAllowRules []string
	permAskRules   []string
	permDenyRules  []string
	permEnvDefault string

	// sandbox is the live [sandbox.Run] for the current iterion run,
	// or nil when the workflow doesn't activate a sandbox. The engine
	// calls SetSandbox after the run starts; backends and tool nodes
	// route their subprocess invocations through it when set.
	sandbox sandbox.Run

	// runExtraEnv is a list of KEY=value process-environment additions
	// applied to every HOST-spawned command of the run — tool nodes,
	// delegate CLI spawns (via Task.ExtraEnv), and the claw bash
	// builtin. The engine pushes it via SetRunExtraEnv on runs without
	// a sandbox (host devbox provisioning: the profile bin dirs
	// prepended to PATH). Nil on sandboxed runs — the container env is
	// settled at container creation.
	runExtraEnv []string

	// sessions holds per-(runID, nodeID) accumulated message lists
	// so the recovery dispatcher's CompactAndRetry path has
	// something to actually compact. The claw backend reads this
	// store via ctx values plumbed by executeBackend.
	sessions *nodeSessionStore

	// boardRegister mints a per-node board MCP run token for the given
	// capabilities and owning ticket (returns the token registered with
	// the server's BoardMCPTokenRegistry). Set via WithBoardRegister on
	// the server path; nil on CLI runs (sandboxed board-emit then stays
	// disabled).
	boardRegister func(caps []string, sourceIssueID string) string
	// boardEndpoint is the per-run gateway-reachable board MCP URL pushed
	// in by the engine after the sandbox starts (SetBoardEndpoint). Empty
	// disables sandboxed board-emit wiring (C082).
	boardEndpoint string
	// askUserEndpoint / askUserToken are the per-run gateway-reachable
	// ask-user MCP HTTP endpoint + bearer token pushed in by the engine
	// after the sandbox starts (SetAskUserEndpoint) — ADR-082 Phase 3.
	// Empty leaves the sandboxed ask-user HTTP transport unwired; the
	// claude_code delegate then disables the native ask_user tools for
	// sandboxed interactive nodes with a loud warning.
	askUserEndpoint string
	askUserToken    string

	// sourceIssueID is the ticket that owns this run (if any). Plumbed
	// into board MCP / claw board tools so create_issue can auto-stamp
	// parent_id on spawned children.
	sourceIssueID string

	// detector lazily probes host credentials (claude_code OAuth,
	// codex OAuth, ANTHROPIC_API_KEY, …) so resolveBackendName can
	// auto-select a backend when neither node nor workflow specifies one.
	detector *detect.CachedDetector

	// inbox is the operator-chatbox binder. When set, every Task built
	// by this executor gets an InboxDrain closure so CLI-based backends
	// (claude_code) can drain queued operator messages from inside their
	// PostToolUse hook. The claw backend reads its own copy via the
	// backend-level WithInbox option (set in runview/executor.go).
	inbox InboxBinder

	// asyncAsk backs the ask_user_async / await_answers tools of
	// interaction: async nodes (ADR-081). nil = async asks unavailable
	// (the tools then error explicitly). Set via WithExecutorAsyncAsk.
	asyncAsk AsyncAskBinder

	// secretGuard is the per-run secrets scrubber (Layer 0/1/2). It is
	// shared with the event hooks (Layer 0 sink redaction); the executor
	// holds its own reference so it can (a) satisfy runtime.SecretScrubber
	// for node_finished output redaction, (b) materialise ${secret.X}
	// placeholders at tool/shell exec (Layer 1). Nil disables it.
	secretGuard *secretguard.Guard

	// extraClosers are auxiliary resources opened during executor
	// construction (e.g. the native board store, whose fsnotify watcher runs
	// a goroutine + holds an inotify instance) that must be released on
	// Close(). Without this each BuildExecutor call leaks one watcher — acute
	// under parallel subbot fan-out, which builds one executor per child and
	// can march toward fs.inotify.max_user_instances.
	extraClosers []io.Closer

	// hostSecretOnce fires ensureHostSecretFiles exactly once per executor:
	// on the first Execute call of a HOST (non-sandbox) run it materialises
	// every `as: file` workflow secret to a per-run tempdir so
	// {{secrets.X.path}} resolves to a real host file. Sandbox runs are a
	// no-op (the sandbox driver bind-mounts the same files via
	// [sandbox.Spec.SecretFiles]). hostSecretCleanup deletes the dir on
	// Close(); hostSecretErr sticks after a first-call failure so every
	// subsequent Execute surfaces the same error rather than silently
	// proceeding with unresolved secret paths.
	hostSecretOnce    sync.Once
	hostSecretCleanup func()
	hostSecretErr     error
}

// SetRunExtraEnv installs run-level process-environment additions
// (KEY=value entries) applied to every host-spawned command of the run.
// The engine calls this once per run, before the first node executes,
// from host devbox provisioning (pkg/runtime/devbox_host.go); the same
// happens-before as SetSandbox makes a mutex unnecessary.
func (e *ClawExecutor) SetRunExtraEnv(env []string) {
	e.runExtraEnv = env
}

// SetSandbox installs the live sandbox handle on the executor. The
// engine calls this once per run, after [resolveAndStartSandbox]
// returns. Subsequent tool node and backend invocations consult the
// handle to route through the sandbox transparently.
//
// Passing nil clears the previous handle (used between runs).
func (e *ClawExecutor) SetSandbox(run sandbox.Run) {
	e.sandbox = run
}

// SetBoardEndpoint installs the per-run gateway-reachable board MCP URL
// (started with the sandbox) so the executor can wire
// Task.BoardHTTPEndpoint/BoardRunToken for sandboxed board-cap nodes
// (C082). Called by the engine after the sandbox starts; no-op endpoint
// "" leaves sandboxed board-emit disabled.
func (e *ClawExecutor) SetBoardEndpoint(endpoint string) {
	e.boardEndpoint = endpoint
}

// SetAskUserEndpoint installs the per-run gateway-reachable ask-user
// MCP URL + token (started with the sandbox) so the executor can wire
// Task.AskUserHTTPEndpoint/AskUserRunToken for sandboxed interactive
// nodes (ADR-082 Phase 3). Called by the engine after the sandbox
// starts; empty values leave the sandboxed ask-user transport unwired.
func (e *ClawExecutor) SetAskUserEndpoint(endpoint, token string) {
	e.askUserEndpoint = endpoint
	e.askUserToken = token
}

// ClawExecutorOption configures a ClawExecutor.
type ClawExecutorOption func(*ClawExecutor)

// WithBoardRegister installs the per-node board MCP token minter (server
// path). The closure registers a token for the node's board capabilities
// with the server's BoardMCPTokenRegistry and returns it; the executor
// pairs it with the board endpoint on each sandboxed board-cap node.
// sourceIssueID is the ticket owning the run, so the HTTP transport can
// auto-stamp parent_id on board.create like the other two transports.
func WithBoardRegister(fn func(caps []string, sourceIssueID string) string) ClawExecutorOption {
	return func(e *ClawExecutor) { e.boardRegister = fn }
}

// WithSourceIssueID stamps the ticket that owns this run so board.create
// can auto-link spawned children (parent_id / spawned_from).
func WithSourceIssueID(issueID string) ClawExecutorOption {
	return func(e *ClawExecutor) { e.sourceIssueID = strings.TrimSpace(issueID) }
}

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

// WithExtraClosers registers auxiliary resources (nil entries ignored) to be
// released when the executor is closed — e.g. the native board store opened for
// board.* MCP tools, whose fsnotify watcher would otherwise leak a goroutine +
// inotify fd per BuildExecutor call. Close() aggregates their errors.
func WithExtraClosers(closers ...io.Closer) ClawExecutorOption {
	return func(e *ClawExecutor) {
		for _, c := range closers {
			if c != nil {
				e.extraClosers = append(e.extraClosers, c)
			}
		}
	}
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

// WithModelOverrides sets the launch-time per-node/-group backend+model+
// provider overrides (studio Launch dropdowns / CLI --model/--backend). They
// take precedence over the node's DSL backend:/model:. Empty is a no-op.
func WithModelOverrides(o ModelOverrides) ClawExecutorOption {
	return func(e *ClawExecutor) { e.modelOverrides = o }
}

// WithCompressOverride sets the run-level compression override (CLI --compress
// / studio Launch toggle): on|ultra|off, or "" for "unset, defer to DSL/env".
// It is the highest-priority input to rewrite.Resolve.
func WithCompressOverride(mode string) ClawExecutorOption {
	return func(e *ClawExecutor) { e.compressOverride = mode }
}

// WithRewriteChain sets the active rewriter chain (the enabled rewriter
// plugins, rtk by default), wired from the plugin registry at startup. Nil
// disables compression regardless of mode.
func WithRewriteChain(chain *rewrite.Chain) ClawExecutorOption {
	return func(e *ClawExecutor) { e.chain = chain }
}

// WithPermissionOverride sets the run-level permission-gate override (CLI
// --permission / studio Launch): off|ask|deny, or "" to defer to DSL/env.
// Highest-priority input to the mode precedence (override > node > workflow
// > env > off).
func WithPermissionOverride(mode string) ClawExecutorOption {
	return func(e *ClawExecutor) { e.permOverride = mode }
}

// WithPermissionRules adds run-level permission rules (CLI
// --permission-allow / --permission-ask / --permission-deny). They are
// additive on top of the workflow-level rule lists.
func WithPermissionRules(allow, ask, deny []string) ClawExecutorOption {
	return func(e *ClawExecutor) {
		e.permAllowRules = append(e.permAllowRules, allow...)
		e.permAskRules = append(e.permAskRules, ask...)
		e.permDenyRules = append(e.permDenyRules, deny...)
	}
}

// WithBotID sets the stable bot identity used to qualify structured
// visibility=bot memory spaces. When unset, NewClawExecutor falls back
// to Workflow.Name.
func WithBotID(botID string) ClawExecutorOption {
	return func(e *ClawExecutor) { e.botID = botID }
}

// WithStoreDir sets the dispatcher store root forwarded to capability-gated
// backend tools (currently the board MCP server). Backends translate this to
// the ITERION_STORE_DIR env var on spawned MCP children.
// WithArtifactFilesDir sets the run's artifact_files scratch dir, exported to
// host tool-node subprocesses as ITERION_ARTIFACT_FILES_DIR (sandboxed runs
// receive it from the container env instead).
func WithArtifactFilesDir(dir string) ClawExecutorOption {
	return func(e *ClawExecutor) { e.artifactFilesDir = dir }
}

func WithStoreDir(dir string) ClawExecutorOption {
	return func(e *ClawExecutor) { e.storeDir = dir }
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

// WithSecretGuard installs the per-run secrets scrubber. Shared with the
// event hooks; the executor uses it to redact node_finished output
// (runtime.SecretScrubber) and to materialise ${secret.X} placeholders
// at tool/shell exec. A nil guard disables secrets protection.
func WithSecretGuard(g *secretguard.Guard) ClawExecutorOption {
	return func(e *ClawExecutor) { e.secretGuard = g }
}

// ScrubOutput satisfies runtime.SecretScrubber: it returns a redacted
// deep copy of a node's output for the (observational) node_finished
// event stream, never mutating the live output (which feeds downstream
// nodes and the resume checkpoint). Nil-safe via the guard.
func (e *ClawExecutor) ScrubOutput(output map[string]any) map[string]any {
	return e.secretGuard.RedactMap(output)
}

// secretMaterializer returns the placeholder→value substitution used to
// populate Task.MaterializeSecrets. Returns nil when no known secrets
// are registered so backends skip the work entirely.
func (e *ClawExecutor) secretMaterializer() func(string) string {
	if e.secretGuard == nil || !e.secretGuard.HasKnownSecrets() {
		return nil
	}
	return e.secretGuard.Materialize
}

func (e *ClawExecutor) secretFileHints() []delegate.SecretFileHint {
	if e.secretGuard == nil {
		return nil
	}
	hints := e.secretGuard.SecretFileHints()
	if len(hints) == 0 {
		return nil
	}
	out := make([]delegate.SecretFileHint, 0, len(hints))
	for _, h := range hints {
		out = append(out, delegate.SecretFileHint{Name: h.Name, Path: h.Path, Env: h.Env})
	}
	return out
}

// MaterializeForHost / ExfiltratesTo / SecretsInspectActive let the
// engine use the executor's guard as the egress rewriter for the
// sandbox proxy's TLS-inspection mode (Layer 2), via a structural
// interface — so the runtime needn't import pkg/backend/secretguard.
func (e *ClawExecutor) MaterializeForHost(s, host string) string {
	return e.secretGuard.MaterializeForHost(s, host)
}

func (e *ClawExecutor) ExfiltratesTo(s, host string) bool {
	return e.secretGuard.ExfiltratesTo(s, host)
}

// SecretsInspectActive reports whether the run has known secrets worth
// inspecting egress for. Egress TLS inspection only pays its cost (CA
// minting + trust injection) when there is something to substitute or
// protect.
func (e *ClawExecutor) SecretsInspectActive() bool {
	return e.secretGuard != nil && e.secretGuard.HasKnownSecrets()
}

// WithExecutorInbox installs the operator-chatbox binder on the
// executor. Every Task built by executeBackend / executeLLMRouterUnified
// then carries an InboxDrain closure so CLI-based backends
// (claude_code) can drain queued messages from inside their
// PostToolUse / Stop hooks. The claw backend's own copy is wired
// separately via WithInbox on the backend (set together in
// runview/executor.go); both share the same StoreInboxBinder so the
// run's queue is the single source of truth.
func WithExecutorInbox(b InboxBinder) ClawExecutorOption {
	return func(e *ClawExecutor) { e.inbox = b }
}

// LifecycleHooks returns the runner installed via WithLifecycleHooks
// (nil if none). It is intended for backends that need to forward the
// runner into their own generation paths.
func (e *ClawExecutor) LifecycleHooks() *hooks.Runner {
	return e.lifecycleHooks
}

// bindInboxDrain resolves the per-task inbox drain closure. Returns nil
// when the executor has no binder, the run ID isn't on the context, or
// the binder returns no hook for this run. Backends that can fire
// hooks at tool / session boundaries (claude_code) consume this; claw
// uses its own opts.Inbox wiring instead.
func (e *ClawExecutor) bindInboxDrain(ctx context.Context) func() []string {
	if e.inbox == nil {
		return nil
	}
	runID := RunIDFromContext(ctx)
	if runID == "" {
		return nil
	}
	hook := e.inbox.Bind(ctx, runID)
	if hook == nil {
		return nil
	}
	return func() []string { return hook.Drain(ctx) }
}

// bindAsyncAsk threads the per-(run,node) async-question closures onto
// the Task (ADR-081). Only called for interaction: async nodes; when
// binding is impossible (no binder wired, no run ID on ctx) the
// closures stay nil — backends then reject the async tools with an
// explicit error — and a warning names the real culprit so the tool
// error isn't misread as a DSL mistake.
func (e *ClawExecutor) bindAsyncAsk(ctx context.Context, nodeID string, task *delegate.Task) {
	var hook AsyncAskHook
	if e.asyncAsk != nil {
		hook = e.asyncAsk.BindAsyncAsk(ctx, RunIDFromContext(ctx), nodeID)
	}
	if hook == nil {
		if e.logger != nil {
			e.logger.Warn("node %q declares interaction: async but no async-ask binder is available for this run (embedder missing WithExecutorAsyncAsk, or no run ID on context) — ask_user_async/await_answers will error", nodeID)
		}
		return
	}
	task.PostAsyncQuestion = func(q delegate.AsyncQuestion) (string, error) {
		return hook.Post(ctx, q)
	}
	task.PendingAsyncQuestions = func() ([]delegate.PendingAsync, error) {
		return hook.Pending(ctx)
	}
	task.CollectAsyncAnswers = func() (string, error) {
		return hook.CollectAnswers(ctx)
	}
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
		iter := LoopIterationFromContext(ctx)
		e.logger.Info("[%s#%d/claw] recovery: compacted session (%d messages dropped)", nodeID, iter, removed)
	}
	return nil
}

// NewClawExecutor creates a ClawExecutor for a given workflow.
func NewClawExecutor(registry *Registry, wf *ir.Workflow, opts ...ClawExecutorOption) *ClawExecutor {
	// Seed vars with workflow-declared defaults from the .bot `vars:`
	// block so prompt templates referencing {{vars.X}} resolve even
	// when the var is not overridden via CLI --var or resume inputs.
	// Without this, an unoverridden var with a default rendered as the
	// literal "{{vars.X}}" string in the LLM prompt — a silent prompt
	// corruption observed in whole_improve_loop where scope_notes
	// (default "") leaked the placeholder into every reviewer call.
	var seed map[string]any
	if len(wf.Vars) > 0 {
		seed = make(map[string]any, len(wf.Vars))
		for name, vr := range wf.Vars {
			if vr.HasDefault {
				seed[name] = vr.Default
			}
		}
	}
	imageAttachs := map[string]bool{}
	for name, a := range wf.Attachments {
		if a != nil && a.Type == ir.AttachmentImage {
			imageAttachs[name] = true
		}
	}
	e := &ClawExecutor{
		registry:           registry,
		prompts:            wf.Prompts,
		schemas:            wf.Schemas,
		cursors:            wf.Cursors,
		imageAttachs:       imageAttachs,
		defaultBackend:     wf.DefaultBackend,
		wfCompress:         wf.Compress,
		compressEnvDefault: os.Getenv(rewrite.ModeEnv),
		wfPermission:       wf.Permission,
		wfPermAllow:        wf.PermissionAllow,
		wfPermAsk:          wf.PermissionAsk,
		wfPermDeny:         wf.PermissionDeny,
		permEnvDefault:     os.Getenv("ITERION_PERMISSION"),
		wfCompaction:       wf.Compaction,
		wfCapabilities:     wf.Capabilities,
		wfSkills:           wf.Skills,
		botID:              wf.Name,
		sessions:           newNodeSessionStore(),
		vars:               seed,
		detector:           detect.NewCachedDetector(5 * time.Minute),
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
// connections and the per-run host secret-file tempdir (host runs only —
// sandbox runs never populate it).
func (e *ClawExecutor) Close() error {
	var errs []error
	if e.mcpManager != nil {
		errs = append(errs, e.mcpManager.Close())
	}
	for _, c := range e.extraClosers {
		errs = append(errs, c.Close())
	}
	if e.hostSecretCleanup != nil {
		e.hostSecretCleanup()
	}
	return errors.Join(errs...)
}

// ensureHostSecretFiles materialises `as: file` workflow secrets to a
// per-run host tempdir on the first Execute call of a HOST (non-sandbox)
// run. Sandbox runs are a no-op — the sandbox driver already bind-mounts
// each file at its declared mount_path via [sandbox.Spec.SecretFiles], so
// this path stays strictly gated on `e.sandbox == nil` to keep sandbox
// behaviour byte-for-byte unchanged.
//
// Idempotent via sync.Once; the resulting rewrite of the guard's
// filePathByName / fileHints happens strictly before any concurrent
// Execute reads them, so ResolveSecretRef sees the host path.
func (e *ClawExecutor) ensureHostSecretFiles() error {
	if e.sandbox != nil {
		return nil
	}
	if e.secretGuard == nil || len(e.secretGuard.SecretFileHints()) == 0 {
		return nil
	}
	e.hostSecretOnce.Do(func() {
		dir, err := os.MkdirTemp("", "iterion-secrets-*")
		if err != nil {
			e.hostSecretErr = fmt.Errorf("model: create host secrets dir: %w", err)
			return
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			_ = os.RemoveAll(dir)
			e.hostSecretErr = fmt.Errorf("model: chmod host secrets dir: %w", err)
			return
		}
		fileCleanup, err := e.secretGuard.MaterializeHostFiles(dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			e.hostSecretErr = err
			return
		}
		e.hostSecretCleanup = func() {
			if fileCleanup != nil {
				fileCleanup()
			}
			_ = os.RemoveAll(dir)
		}
	})
	return e.hostSecretErr
}

// SetVars merges run-level workflow variables into the executor's
// vars map. Keys present in vars override the matching default seeded
// from wf.Vars at construction time; keys absent from vars retain
// their default. Must be called before Execute.
func (e *ClawExecutor) SetVars(vars map[string]any) {
	if e.vars == nil {
		e.vars = make(map[string]any, len(vars))
	}
	for k, v := range vars {
		e.vars[k] = v
	}
}

// SetPresetFocus records the selected launch-time preset's prompt bias and
// relevant-skill hints. The engine calls this at run start (and on resume)
// for the `--preset <name>` selection; every LLM node then renders a
// "## Focus" section in its system prompt (see executeBackend). The prompt
// is stored raw and {{vars.X}}-resolved per node. Empty args clear the
// focus. Must be called before Execute; not safe to call concurrently.
func (e *ClawExecutor) SetPresetFocus(prompt string, skills []string) {
	e.presetPrompt = prompt
	e.presetSkills = skills
}

// SetSkillHints records the name→description map of skill-library skills that
// resolved in the library at run start (from the runtime mirror). Every LLM
// node then renders a "## Skills" section for the subset it references
// (node `skills:` ∪ workflow default). Nil/empty clears the hints. Must be
// called before Execute; not safe to call concurrently.
func (e *ClawExecutor) SetSkillHints(hints map[string]string) {
	e.skillHints = hints
}

// SetMirroredSkills records the skill directories iterion OWNS in this
// workspace — what the mirror wrote or refreshed, excluding anything the
// workspace shadowed.
//
// A backend that hands skills to an agent needs to tell iterion's own from
// whatever the TARGET repository ships under the same .claude/skills/ path, and
// the workspace is an untrusted checkout: reading it back cannot establish that,
// because a repo can write any file, including one shaped like iterion's own
// bookkeeping. This is the engine saying what it wrote. Must be called before
// Execute; not safe to call concurrently.
func (e *ClawExecutor) SetMirroredSkills(paths []string) {
	e.mirroredSkills = paths
}

// SetWorkDir updates the working directory for backend subprocesses
// (claude_code, codex) and tool node shell exec. The engine calls this
// at run start when `worktree: auto` produces a per-run worktree path
// that wasn't known at executor construction time. Safe to call before
// Execute; not safe to call concurrently with an in-flight Execute.
func (e *ClawExecutor) SetWorkDir(dir string) {
	e.workDir = dir
}

// SetRepoRoot updates the source-of-truth repository root. The engine
// calls this once per run, alongside SetWorkDir, so memory specs that
// opt into `project_root: true` resolve their scope under the operator's
// main repo even when the run executes from a worktree or dispatcher
// workspace. Empty string means "no project root captured" — memory
// specs that require it will fall back to WorkDir's encoded key.
func (e *ClawExecutor) SetRepoRoot(dir string) {
	e.repoRoot = dir
}

// clawToolHint extracts a short human-readable hint from a tool's
// raw JSON input for the per-tool-call log line. Defensive against
// arbitrary tool schemas — returns "" when nothing fits.
func clawToolHint(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "query", "url", "title", "id"} {
		if v, ok := obj[k].(string); ok && v != "" {
			if len(v) > 60 {
				v = v[:57] + "…"
			}
			return v
		}
	}
	return ""
}

// delegateHooksFor builds the TaskHooks block passed to a delegate
// backend. It bridges the executor's own EventHooks into the simpler
// callback surface backends consume. Returns a zero-value TaskHooks
// when neither hook is wired, which backends handle as "no observers".
//
// When backendName == claw, the tool hooks ALSO write a tagged line
// to e.logger so per-tool-call activity surfaces in run.log (F-NEW-13).
// claude_code + codex emit their own `[%s#%d/<backend>]` lines from
// the subprocess stderr capture path, so we don't double-log them here.
func (e *ClawExecutor) delegateHooksFor(nodeID string, backendName string, iteration int) delegate.TaskHooks {
	var h delegate.TaskHooks
	logForClaw := backendName == delegate.BackendClaw && e.logger != nil
	if e.hooks.OnToolStarted != nil || logForClaw {
		fn := e.hooks.OnToolStarted
		h.OnToolStarted = func(toolName string, toolUseID string, input json.RawMessage) {
			if logForClaw {
				// LogBlock (not Info) so a long/multi-line input folds under the
				// truncated header as an expandable "▸" block — parity with
				// claude_code. clawToolHint keeps the one-line header; BlockBody
				// supplies the full body when it had to clip.
				e.logger.LogBlock(iterlog.LevelInfo, "ℹ️ ",
					fmt.Sprintf("[%s#%d/claw] 🔧 %s %s", nodeID, iteration, toolName, clawToolHint(input)),
					tooldisplay.BlockBody(toolName, input))
			}
			if fn != nil {
				fn(nodeID, LLMToolStartedInfo{
					ToolName:  toolName,
					ToolUseID: toolUseID,
					InputSize: len(input),
					Input:     input,
					Iteration: iteration,
				})
			}
		}
	}
	if e.hooks.OnToolCall != nil || logForClaw {
		fn := e.hooks.OnToolCall
		h.OnToolCalled = func(toolName string, toolUseID string, isError bool, output string) {
			if logForClaw {
				// Log the tool RESULT as an expandable block (📤 success, ❌
				// error): truncated preview in the header, full (bounded) output
				// folded underneath — symmetric with the input above and
				// identical to claude_code via tooldisplay.ResultDisplay.
				glyph := "📤"
				if isError {
					glyph = "❌"
				}
				header, body := tooldisplay.ResultDisplay(output)
				if header == "" {
					header = fmt.Sprintf("(%d bytes)", len(output))
				}
				e.logger.LogBlock(iterlog.LevelInfo, "ℹ️ ",
					fmt.Sprintf("[%s#%d/claw] %s %s %s", nodeID, iteration, glyph, toolName, header),
					body)
			}
			if fn != nil {
				info := LLMToolCallInfo{
					ToolName:  toolName,
					ToolUseID: toolUseID,
					Output:    output,
				}
				if isError {
					// The tool_result content IS the error message (e.g. the
					// CLI's StructuredOutput schema-validation detail). Losing
					// it to a generic "tool error" cost a real debugging hour:
					// 2.1.128's stringified-bool emissions surfaced as five
					// opaque "tool error (0ms)" lines. Keep it, truncated.
					msg := strings.TrimSpace(output)
					if msg == "" {
						msg = "tool error"
					}
					info.Error = errors.New(iterlog.Truncate(msg, 500))
				}
				fn(nodeID, info)
			}
		}
	}
	// Wire claude_code's per-delegate-call OnTurnFinished hook into a
	// LLMTurnCaptureInfo emission so the same store-backed event hook
	// that writes TurnCheckpoints for claw also writes them for
	// claude_code. The conversation payload is empty (the CLI owns its
	// own session jsonl at ~/.claude/projects/...); SessionID carries
	// the anchor the Fork API needs to launch `claude --resume <id>
	// --fork-session`.
	// Narration bridge: claude_code streams assistant text blocks; the
	// store hook filters structured-JSON payloads and persists the rest
	// as assistant_text events (claw's equivalent is derived from
	// tool-bearing steps inside onLLMStepFinish).
	if e.hooks.OnAssistantText != nil {
		fn := e.hooks.OnAssistantText
		h.OnAssistantText = func(text string) {
			fn(nodeID, AssistantTextInfo{Text: text, Iteration: iteration})
		}
	}
	if e.hooks.OnLLMTurnCapture != nil {
		fn := e.hooks.OnLLMTurnCapture
		h.OnTurnFinished = func(info delegate.TurnFinishedInfo) {
			// One TurnCheckpoint per delegate call; the CLI's session
			// jsonl at ~/.claude/projects/<key>/<uuid>.jsonl is the
			// source of truth for the conversation, and SessionID is
			// the anchor the Fork API uses to relaunch claude with
			// --resume + --fork-session.
			fn(nodeID, LLMTurnCaptureInfo{
				Step:         1,
				Text:         info.Text,
				FinishReason: info.FinishReason,
				InputTokens:  info.InputTokens,
				OutputTokens: info.OutputTokens,
				SessionID:    info.SessionID,
				Backend:      delegate.BackendClaudeCode,
				Iteration:    iteration,
			})
		}
	}
	return h
}

// Execute implements runtime.NodeExecutor.
func (e *ClawExecutor) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	// Host runs (no sandbox) materialise `as: file` workflow secrets to a
	// tempdir on the first call so {{secrets.X.path}} resolves to a real
	// host file for tool nodes AND for agent/judge prompts. Cheap
	// sync.Once check after the first hit; a no-op when no file secrets
	// are declared or when a sandbox owns the mounts.
	if err := e.ensureHostSecretFiles(); err != nil {
		return nil, err
	}

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

func (e *ClawExecutor) executeNode(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	switch n := node.(type) {
	case *ir.AgentNode:
		return e.executeBackend(ctx, n, input)
	case *ir.JudgeNode:
		return e.executeBackend(ctx, n, input)
	case *ir.HumanNode:
		return e.executeHumanLLM(ctx, n, input, nil)
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
