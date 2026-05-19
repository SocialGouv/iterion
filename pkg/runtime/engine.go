// Package runtime implements the workflow execution engine.
// It walks the compiled IR graph node by node, persists outputs and
// artifacts via the store, evaluates edge conditions and loop counters,
// and emits lifecycle events. It supports both sequential execution and
// parallel fan-out/join patterns via a bounded branch scheduler.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tracerName is the OTel instrumentation name for runtime spans. The
// global tracer is a no-op until cmd/iterion configures a provider, so
// instrumentation here costs nothing in local mode and unit tests.
const tracerName = "github.com/SocialGouv/iterion/pkg/runtime"

// ErrRunPaused is returned by Run or Resume when execution is suspended
// at a human node. This is not a failure — the run can be resumed via
// Engine.Resume.
var ErrRunPaused = errors.New("runtime: run paused waiting for human input")

// ErrRunCancelled is returned when a run is interrupted by context
// cancellation (e.g. SIGINT). Distinguished from failures so callers
// can handle cancellation gracefully.
var ErrRunCancelled = errors.New("runtime: run cancelled")

// ErrServerDraining is returned by the runview Service when Launch or
// Resume is called after the server has begun graceful shutdown. The
// HTTP layer translates this to 503 Service Unavailable.
var ErrServerDraining = errors.New("runtime: server draining")

// NodeExecutor is the abstraction called by the engine to actually run a
// node (LLM call, tool invocation, etc.). The runtime itself is agnostic
// to the concrete implementation — tests supply stubs, production code
// plugs in real providers.
type NodeExecutor interface {
	// Execute runs the given node with the provided input and returns its
	// output. For terminal nodes (done/fail) this is never called.
	Execute(ctx context.Context, node ir.Node, input map[string]interface{}) (map[string]interface{}, error)
}

// Engine executes workflows. It supports sequential execution and
// parallel fan-out via bounded branch scheduling.
type Engine struct {
	workflow                 *ir.Workflow
	store                    store.RunStore
	executor                 NodeExecutor
	logger                   *iterlog.Logger
	onNodeFinished           func(nodeID string, output map[string]interface{})
	onEvent                  func(evt store.Event) // optional observer fired after every successful append
	recoveryDispatch         RecoveryDispatch      // optional; consulted on node execution failure
	workflowHash             string                // SHA-256 of the .iter source, set via WithWorkflowHash
	filePath                 string                // absolute .iter source path, set via WithFilePath
	preset                   string                // in-source preset name selected at launch, set via WithPreset
	runName                  string                // deterministic human-friendly run label, set via WithRunName
	mergeInto                string                // worktree finalization: FF target ("" = current branch, "none" = skip, or branch name); set via WithMergeInto
	branchName               string                // worktree finalization: storage branch override ("" = iterion/run/<runName>); set via WithBranchName
	mergeStrategy            string                // worktree finalization: "squash" (default) or "merge" (FF); set via WithMergeStrategy
	autoMerge                bool                  // worktree finalization: when true, apply mergeStrategy at end of run; otherwise leave merge_status=pending for UI; set via WithAutoMerge
	validateOutputs          bool                  // when true, validate node outputs against declared schemas
	forceResume              bool                  // when true, skip workflow hash check on resume
	workDir                  string                // working directory for subprocesses + PROJECT_DIR expansion; defaults to os.Getwd() at Run() time
	containerWorkspace       string                // when sandbox is active, the in-container path the host workDir is bind-mounted to (e.g. "/workspace"); used to remap ${PROJECT_DIR} so prompts and tool nodes see paths the in-container processes can actually open
	sandboxOverride          string                // CLI/Launch-level sandbox mode override; "" means "no override" (workflow + global default win); set via WithSandboxOverride
	sandboxDefault           string                // global ITERION_SANDBOX_DEFAULT value snapshot; set via WithSandboxDefault
	sandboxDefaultImage      string                // image ref used as fallback when sandbox: auto and no .devcontainer/devcontainer.json is found; "" lets the runtime pick the built-in pinned to the iterion version; set via WithSandboxDefaultImage
	sandboxHostStateOverride string                // CLI/Launch-level override for sandbox.host_state ("auto"|"none"|""); set via WithSandboxHostStateOverride
	sandboxHostStateDefault  string                // global ITERION_SANDBOX_HOST_STATE snapshot; set via WithSandboxHostStateDefault
	attachmentPromote        AttachmentPromoteFunc // optional: invoked after CreateRun to materialise attachments
	bundle                   *bundle.Bundle        // optional: bundle backing this run; nil for plain .iter/.bot runs
}

// AttachmentPromoteFunc is invoked once at the start of a run, right
// after the run is created in the store but before the engine walks
// the graph. It is expected to populate Run.Attachments by calling
// store.WriteAttachment for each attachment declared in the
// workflow's `attachments:` block.
type AttachmentPromoteFunc func(ctx context.Context, runID string) error

// EngineOption configures an Engine.
type EngineOption func(*Engine)

// WithLogger sets a leveled logger for console output during execution.
func WithLogger(l *iterlog.Logger) EngineOption {
	return func(e *Engine) { e.logger = l }
}

// WithSandboxOverride sets the CLI / Launch-modal level sandbox mode.
// Highest precedence in the resolution chain (CLI > workflow > global
// default). The value is one of "", "none", or "auto". An empty
// string means "no override".
func WithSandboxOverride(mode string) EngineOption {
	return func(e *Engine) { e.sandboxOverride = mode }
}

// WithSandboxDefault sets the global default sandbox mode (the
// snapshot of ITERION_SANDBOX_DEFAULT or the project config). Lowest
// precedence in the resolution chain — workflow and CLI override it.
func WithSandboxDefault(mode string) EngineOption {
	return func(e *Engine) { e.sandboxDefault = mode }
}

// WithSandboxDefaultImage sets the image ref used as fallback when
// `sandbox: auto` is active but no .devcontainer/devcontainer.json is
// found. Empty string lets the runtime pick the built-in default
// (`ghcr.io/socialgouv/iterion-sandbox-slim:<iterion-version>`).
func WithSandboxDefaultImage(ref string) EngineOption {
	return func(e *Engine) { e.sandboxDefaultImage = ref }
}

// WithSandboxHostStateOverride sets the CLI / Launch-modal level
// override for sandbox.host_state. Highest precedence. Value is one
// of "", "auto", or "none". An empty string means "no override".
func WithSandboxHostStateOverride(mode string) EngineOption {
	return func(e *Engine) { e.sandboxHostStateOverride = mode }
}

// WithSandboxHostStateDefault sets the global default for
// sandbox.host_state (the snapshot of ITERION_SANDBOX_HOST_STATE).
// Lowest precedence — workflow and CLI override it.
func WithSandboxHostStateDefault(mode string) EngineOption {
	return func(e *Engine) { e.sandboxHostStateDefault = mode }
}

// WithAttachmentPromote registers a callback invoked right after
// CreateRun to materialise attachments declared in the workflow's
// `attachments:` block. The callback is responsible for writing the
// bytes (typically via store.WriteAttachment) so that
// Run.Attachments is populated before the first node runs.
func WithAttachmentPromote(fn AttachmentPromoteFunc) EngineOption {
	return func(e *Engine) { e.attachmentPromote = fn }
}

// WithOnNodeFinished registers a callback invoked after each node finishes
// with the node's ID and output. The callback must be safe for concurrent use.
func WithOnNodeFinished(fn func(nodeID string, output map[string]interface{})) EngineOption {
	return func(e *Engine) { e.onNodeFinished = fn }
}

// WithEventObserver registers a callback invoked after every successful
// event append (including branch_started/finished). It must be safe for
// concurrent use; the callback runs in the goroutine that emitted the
// event. Use it to fan out events to non-store observers (Prometheus,
// custom metrics) without changing the persistence layer.
func WithEventObserver(fn func(evt store.Event)) EngineOption {
	return func(e *Engine) { e.onEvent = fn }
}

// WithRecoveryDispatch installs the dispatcher consulted when a node's
// executor returns an error. The dispatcher decides between retry,
// compact-and-retry, pause for human, and terminal failure. When unset,
// every error falls straight through to failed_resumable (legacy
// behaviour).
//
// Build the dispatcher with recovery.Dispatch(recovery.DefaultRecipes()).
func WithRecoveryDispatch(d RecoveryDispatch) EngineOption {
	return func(e *Engine) { e.recoveryDispatch = d }
}

// WithWorkflowHash sets a hash of the .iter source so that Resume can
// detect if the workflow changed since the run was started.
func WithWorkflowHash(hash string) EngineOption {
	return func(e *Engine) { e.workflowHash = hash }
}

// WithFilePath records the absolute .iter source path on the run
// metadata so that resume (and the run console) can re-locate the
// workflow without the caller having to thread it back through the
// API. Optional — empty string is ignored.
func WithFilePath(path string) EngineOption {
	return func(e *Engine) { e.filePath = path }
}

// WithRunName records a deterministic, human-friendly label on the
// run metadata at creation. Display-only — the canonical identifier
// remains the run ID. Optional — empty string is ignored.
func WithRunName(name string) EngineOption {
	return func(e *Engine) { e.runName = name }
}

// WithPreset records the in-source preset name selected at launch
// (`--preset <name>`) on the run metadata so resume re-applies the
// same parameter set without re-typing it. Empty when no preset was
// selected.
func WithPreset(name string) EngineOption {
	return func(e *Engine) { e.preset = name }
}

// WithMergeInto controls the worktree-finalization fast-forward target
// for `worktree: auto` runs. Values:
//   - "" or "current" → fast-forward the user's currently-checked-out
//     branch (default behaviour)
//   - "none"          → skip the fast-forward; the storage branch
//     remains the only landing
//   - <branch-name>   → fast-forward this named branch (only honoured
//     when it matches the currently-checked-out branch; otherwise a
//     warning is logged and the FF is skipped)
//
// No effect on runs without `worktree: auto`.
func WithMergeInto(target string) EngineOption {
	return func(e *Engine) { e.mergeInto = target }
}

// WithBranchName overrides the storage branch name for the worktree
// finalization. The default `iterion/run/<runName>` is used when this
// is empty. The branch is always created (it is the GC guard for the
// run's commits); on collision the engine appends a numeric suffix.
//
// No effect on runs without `worktree: auto`.
func WithBranchName(name string) EngineOption {
	return func(e *Engine) { e.branchName = name }
}

// WithMergeStrategy selects how the run's commits are landed on the
// merge target when AutoMerge is on (or when triggered later via the
// UI). Accepted values:
//
//   - "squash" (default) — collapse all run commits into one new commit
//     on top of the target branch, with an aggregated message.
//   - "merge"            — fast-forward the target onto the run's HEAD,
//     preserving the per-iteration commit history (legacy behaviour).
//
// Empty string falls back to "squash".
func WithMergeStrategy(strategy string) EngineOption {
	return func(e *Engine) { e.mergeStrategy = strategy }
}

// WithAutoMerge controls whether the engine applies the merge strategy
// synchronously at the end of the run (true) or stops after creating
// the storage branch (false, default), leaving merge_status="pending"
// so the studio can offer a deferred GitHub-style merge action.
func WithAutoMerge(auto bool) EngineOption {
	return func(e *Engine) { e.autoMerge = auto }
}

// WithForceResume allows resuming a run even when the workflow source has
// changed since the run was started. The hash mismatch is logged as a warning
// instead of causing an error.
func WithForceResume(force bool) EngineOption {
	return func(e *Engine) { e.forceResume = force }
}

// WithWorkDir sets the working directory used for backend subprocesses and
// for resolving the `${PROJECT_DIR}` placeholder in workflow var defaults.
// When unset, defaults to os.Getwd() at Run() time. With worktree: auto on
// the workflow, the engine overrides this with the per-run worktree path.
func WithWorkDir(dir string) EngineOption {
	return func(e *Engine) { e.workDir = dir }
}

// WithBundle attaches a resolved `.botz` bundle to the engine. The
// runtime mirrors the bundle's `skills/` directory into the workDir's
// `.claude/skills/` so claude_code and the claw skill registry pick
// them up transparently. Pass nil (the default) for plain .iter/.bot
// runs without a backing bundle.
func WithBundle(b *bundle.Bundle) EngineOption {
	return func(e *Engine) { e.bundle = b }
}

// WithOutputValidation enables post-execution validation of node outputs
// against their declared output schemas. When enabled, a node whose output
// does not conform to its schema will cause the run to fail immediately.
func WithOutputValidation(enabled bool) EngineOption {
	return func(e *Engine) { e.validateOutputs = enabled }
}

// New creates a new Engine for a raw workflow.
func New(wf *ir.Workflow, s store.RunStore, exec NodeExecutor, opts ...EngineOption) *Engine {
	e := &Engine{workflow: wf, store: s, executor: exec}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// NewFromRecipe creates a new Engine by applying a recipe's presets onto
// the given workflow. The recipe merges preset variables, prompt overrides,
// and budget limits, producing a self-contained execution unit.
func NewFromRecipe(r *recipe.RecipeSpec, wf *ir.Workflow, s store.RunStore, exec NodeExecutor, opts ...EngineOption) (*Engine, error) {
	applied, err := r.Apply(wf)
	if err != nil {
		return nil, fmt.Errorf("runtime: apply recipe %q: %w", r.Name, err)
	}
	e := &Engine{workflow: applied, store: s, executor: exec}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// runState holds the mutable runtime state passed through the execution loop.
type runState struct {
	// ctx is the per-run context. Stored on runState (despite the
	// usual "no context in struct" rule) because helpers.go threads
	// `rs *runState` deeply through emit/failRun*/checkpoint paths
	// where adding ctx to every signature would 80+ call sites with
	// no semantic gain — the lifetime of rs IS the lifetime of ctx.
	// Set in Run() before execLoop().
	ctx          context.Context
	runID        string
	runInputs    map[string]interface{}
	vars         map[string]interface{}
	outputs      map[string]map[string]interface{}
	artifacts    map[string]map[string]interface{} // publish name → output
	loopCounters map[string]int
	// loopPreviousOutput holds the snapshot of the source node output from
	// the PREVIOUS traversal of a given loop's edge — i.e., one iteration
	// behind the current one. Workflows reference it as
	// {{loop.<name>.previous_output[.field]}}; in the very first iteration
	// of a loop the value is nil. The snapshot is rotated through
	// loopCurrentOutput on each traversal to preserve the one-iteration lag.
	loopPreviousOutput map[string]map[string]interface{}
	loopCurrentOutput  map[string]map[string]interface{} // staging slot for the next iteration's "previous"
	roundRobinCounters map[string]int
	artifactVersions   map[string]int
	budget             *SharedBudget // shared across branches, nil if no budget

	// nodeAttempts counts prior failed attempts per (nodeID, ErrorCode)
	// so the recovery dispatcher can apply per-class retry budgets and
	// reset them after a successful execution. Keys are created lazily.
	nodeAttempts map[string]map[ErrorCode]int

	// attachments holds the resolved per-attachment view exposed to
	// templates as {{attachments.X[.path|.url|.mime|.size|.sha256]}}.
	// Populated once at run start from Run.Attachments and the
	// store's PresignAttachment helper.
	attachments map[string]model.AttachmentInfo
}

// markFailedBestEffort transitions the run to status=failed with a
// formatted message describing the origin error. Used on pre-execLoop
// setup failures where the engine is about to return the same error to
// the caller — a store-side failure of the status flip itself can't be
// propagated, so it's logged at warn level instead of being silently
// dropped (without this, an op error during startup leaves the run
// stuck in `running` in the UI).
func (e *Engine) markFailedBestEffort(ctx context.Context, runID, phase string, cause error) {
	msg := fmt.Sprintf("%s: %v", phase, cause)
	if err := e.store.UpdateRunStatus(ctx, runID, store.RunStatusFailed, msg); err != nil && e.logger != nil {
		e.logger.Warn("runtime: failed to record run %s as failed during %s: %v (original cause: %v)", runID, phase, err, cause)
	}
}

// newRunState builds a runState with all maps allocated. Resume paths
// then overwrite specific fields (outputs, loop counters, vars, etc.)
// from the persisted checkpoint.
func (e *Engine) newRunState(runID string, inputs map[string]interface{}) *runState {
	return &runState{
		runID:              runID,
		runInputs:          inputs,
		outputs:            make(map[string]map[string]interface{}),
		artifacts:          make(map[string]map[string]interface{}),
		loopCounters:       make(map[string]int),
		loopPreviousOutput: make(map[string]map[string]interface{}),
		loopCurrentOutput:  make(map[string]map[string]interface{}),
		roundRobinCounters: make(map[string]int),
		artifactVersions:   make(map[string]int),
		nodeAttempts:       make(map[string]map[ErrorCode]int),
		budget:             newSharedBudget(e.workflow.Budget, e.logger),
	}
}

// Run executes the workflow. It creates a run, walks the graph from the
// entry node, and returns when a terminal node is reached, a human pause
// is hit (ErrRunPaused), or an error occurs.
//
// Two entry shapes are accepted:
//
//   - **Direct** (CLI / single-process): no doc exists yet, CreateRun
//     inserts a fresh row.
//   - **Cloud pickup** (runner pool): the cloudpublisher already
//     persisted the run with status=queued before publishing on
//     JetStream. The runner calls Run with the same runID, expecting
//     us to claim the existing row and transition it to running. A
//     plain CreateRun would error with "already exists".
//
// LoadRun + transition is attempted first; if no doc exists we fall
// back to CreateRun. Any other status (running, finished, …) is a
// programming error — refuse to clobber state.
func (e *Engine) Run(ctx context.Context, runID string, inputs map[string]interface{}) (err error) {
	var run *store.Run
	if existing, loadErr := e.store.LoadRun(ctx, runID); loadErr == nil {
		// Pickup path: the doc already exists.
		//   - queued: cloudpublisher pre-created the row before
		//     publishing on JetStream; transition to running here.
		//   - running: either FilesystemRunStore.CreateRun (which
		//     creates with running) was invoked first by a CLI or
		//     test setup, or a sibling runner is already executing
		//     this run. The contention case is supposed to be guarded
		//     at the queue level (NATS distributed lock), not here.
		// Other statuses (finished/failed/cancelled/paused/failed_resumable)
		// are terminal or resume-only and must not be silently restarted.
		switch existing.Status {
		case store.RunStatusQueued:
			if err := e.store.UpdateRunStatus(ctx, runID, store.RunStatusRunning, ""); err != nil {
				return fmt.Errorf("runtime: pickup transition: %w", err)
			}
			existing.Status = store.RunStatusRunning
		case store.RunStatusRunning:
			// Already running — assume legitimate claim.
		default:
			return fmt.Errorf("runtime: run %s already in status %s, refusing to restart", runID, existing.Status)
		}
		if len(inputs) > 0 {
			existing.Inputs = inputs
		}
		run = existing
	} else {
		// Direct path: no doc yet, create one. CreateRun is strict
		// (InsertOne) so a parallel pickup would lose this race —
		// acceptable: the only callers here are the CLI and tests, both
		// single-writer.
		var err error
		run, err = e.store.CreateRun(ctx, runID, e.workflow.Name, inputs)
		if err != nil {
			return fmt.Errorf("runtime: create run: %w", err)
		}
	}
	if e.workflowHash != "" || e.filePath != "" || e.runName != "" || e.mergeStrategy != "" || e.autoMerge || e.preset != "" || e.bundle != nil {
		if e.workflowHash != "" {
			run.WorkflowHash = e.workflowHash
		}
		if e.filePath != "" {
			run.FilePath = e.filePath
		}
		if e.runName != "" {
			run.Name = e.runName
		}
		if e.mergeStrategy != "" {
			run.MergeStrategy = store.MergeStrategy(e.mergeStrategy)
		}
		run.AutoMerge = e.autoMerge
		if e.preset != "" {
			run.Preset = e.preset
		}
		if e.bundle != nil {
			run.BundleHash = e.bundle.Hash
			run.BundlePath = e.bundle.SourcePath
		}
		if err := e.store.SaveRun(ctx, run); err != nil {
			return fmt.Errorf("runtime: save run metadata: %w", err)
		}
	}

	// Bundle attachment defaults land first so any runtime upload
	// processed by attachmentPromote below overwrites them.
	if err := promoteBundleAttachmentDefaults(ctx, e.store, runID, e.workflow, e.bundle, e.logger); err != nil {
		e.markFailedBestEffort(ctx, runID, "bundle attachment defaults", err)
		return fmt.Errorf("runtime: bundle attachment defaults: %w", err)
	}

	// Reload `run` after the promote callback so the next SaveRun
	// below doesn't clobber Run.Attachments (the promote writes
	// directly to the store; our local `run` is now stale).
	if e.attachmentPromote != nil {
		if err := e.attachmentPromote(ctx, runID); err != nil {
			e.markFailedBestEffort(ctx, runID, "attachment promote", err)
			return fmt.Errorf("runtime: promote attachments: %w", err)
		}
	}
	if reloaded, err := e.store.LoadRun(ctx, runID); err == nil {
		run = reloaded
	}

	// Default workDir to process cwd if not set explicitly.
	if e.workDir == "" {
		if cwd, cwdErr := os.Getwd(); cwdErr == nil {
			e.workDir = cwd
		}
	}

	// Worktree setup: when the workflow opts in with `worktree: auto`,
	// create a fresh git worktree for this run so all node executions
	// happen in an isolated checkout. The user's main working tree
	// (with WIP, build artefacts, etc.) is invisible by construction.
	var worktreeCleanup func()
	var wtCtx worktreeContext
	worktreeActive := false
	if e.workflow.Worktree == "auto" {
		wtc, cleanup, wtErr := setupWorktree(e.store.Root(), runID, e.workDir, e.logger)
		if wtErr != nil {
			e.markFailedBestEffort(ctx, runID, "worktree setup", wtErr)
			return fmt.Errorf("runtime: worktree setup: %w", wtErr)
		}
		e.workDir = wtc.wtPath
		worktreeCleanup = cleanup
		wtCtx = wtc
		worktreeActive = true
		// Cover every exit path from this point on. Without this defer,
		// failures in mirrorBundleSkills / startSandbox / emit /
		// SaveRun(workDir) would return without finalize and leak the
		// worktree dir + orphan any commits the partial run produced.
		// finalizeOnExit short-circuits on error (preserves worktree for
		// inspection, skips FF) so a single defer covers both happy and
		// error paths uniformly.
		defer func() {
			e.finalizeOnExit(ctx, runID, &wtCtx, worktreeCleanup, err)
		}()
	}

	// Persist the resolved working directory so studio surfaces (modified-
	// files panel) can locate it without re-deriving it. Done after worktree
	// setup so e.workDir reflects the per-run worktree path when applicable.
	// For worktree-active runs we also stash the baseline (RepoRoot +
	// BaseCommit) so the modified-files panel can render a historical diff
	// against FinalCommit after the worktree directory has been torn down.
	if e.workDir != "" {
		run.WorkDir = e.workDir
		run.Worktree = e.workflow.Worktree == "auto"
		if worktreeActive {
			run.RepoRoot = wtCtx.repoRoot
			run.BaseCommit = wtCtx.originalTip
		}
		if err := e.store.SaveRun(ctx, run); err != nil {
			e.markFailedBestEffort(ctx, runID, "save work dir", err)
			return fmt.Errorf("runtime: save work dir: %w", err)
		}
	}

	// Push workDir into the executor so backend subprocesses (claude_code,
	// codex) and tool nodes see it. Type-assert because NodeExecutor is a
	// minimal interface; only ClawExecutor implements SetWorkDir.
	type workDirSetter interface{ SetWorkDir(string) }
	if s, ok := e.executor.(workDirSetter); ok {
		s.SetWorkDir(e.workDir)
	}

	// Bundle skill mirroring: when a .botz backs this run, copy the
	// bundle's skills/ entries into <workDir>/.claude/skills/ so both
	// claude_code's native skill lookup and the claw `skill` tool
	// discover them transparently. Workspace files always win on
	// collision (see runtime/bundle.go for the rule).
	if err := mirrorBundleSkills(e.workDir, e.bundle, e.logger); err != nil {
		e.markFailedBestEffort(ctx, runID, "bundle skills", err)
		return fmt.Errorf("runtime: bundle skills: %w", err)
	}

	// Sandbox lifecycle: when the workflow opts in, start a long-lived
	// container that hosts every delegate invocation for this run.
	// startSandbox handles the workflow/global/CLI resolution, executor
	// wiring, and containerWorkspace stash so resumes can share the
	// exact same path.
	repoRoot := wtCtx.repoRoot
	if repoRoot == "" {
		repoRoot = engineRepoRoot(e.workDir)
	}
	sandboxCleanup, sbErr := e.startSandbox(ctx, runID, repoRoot)
	if sbErr != nil {
		e.markFailedBestEffort(ctx, runID, "sandbox start", sbErr)
		return fmt.Errorf("runtime: sandbox: %w", sbErr)
	}
	defer sandboxCleanup()

	// Emit run_started.
	if err := e.emit(ctx, runID, store.EventRunStarted, "", nil); err != nil {
		e.markFailedBestEffort(ctx, runID, "emit run_started", err)
		return fmt.Errorf("runtime: emit run_started: %w", err)
	}

	rs := e.newRunState(runID, inputs)
	rs.ctx = ctx
	rs.vars = e.resolveVars(inputs)
	rs.attachments = e.loadAttachmentInfos(ctx, runID)

	// Refresh executor vars: PROJECT_DIR-aware expansion may have changed
	// values from what the CLI/server originally seeded.
	type varsSetter interface{ SetVars(map[string]interface{}) }
	if sv, ok := e.executor.(varsSetter); ok {
		sv.SetVars(rs.vars)
	}

	loopErr := e.execLoop(ctx, rs, e.workflow.Entry)
	e.evictRunSessions(runID, loopErr)

	// Worktree finalization runs via the defer installed right after
	// setupWorktree above — covers both this happy-path return and any
	// setup-phase early returns.
	return loopErr
}

// finalizeOnExit applies the worktree-finalization step at the end of a
// run. Called from Run() (which captures wtCtx during setupWorktree)
// and from both resume paths (which reconstruct wtCtx from the
// persisted run record). Persistence + cleanup are best-effort: a save
// failure logs but never fails the run, since the work has completed.
//
// Without this on the resume paths, a `worktree: auto` run that paused
// and resumed via CLI ended with no final_branch / final_commit
// persisted, the worktree dir leaked, and the run's commits were
// reachable only via reflog (eligible for `git gc` after ~30 days) —
// see F-RT-1 in docs/reviews/codebase-2026-05-17.md.
func (e *Engine) finalizeOnExit(ctx context.Context, runID string, wtCtx *worktreeContext, cleanup func(), loopErr error) {
	if wtCtx == nil {
		return
	}
	if loopErr != nil {
		if e.logger != nil {
			e.logger.Info("runtime: worktree preserved for inspection: %s", e.workDir)
		}
		return
	}
	finRes := finalizeWorktree(*wtCtx, finalizeOptions{
		runName:       e.runName,
		runID:         runID,
		branchName:    e.branchName,
		mergeInto:     e.mergeInto,
		mergeStrategy: e.mergeStrategy,
		autoMerge:     e.autoMerge,
	}, e.logger)
	if finRes.FinalCommit != "" || finRes.FinalBranch != "" || finRes.MergedInto != "" || finRes.MergeStatus != "" || finRes.FinalBranchError != "" {
		if r2, err := e.store.LoadRun(ctx, runID); err == nil {
			r2.FinalCommit = finRes.FinalCommit
			r2.FinalBranch = finRes.FinalBranch
			r2.FinalBranchError = finRes.FinalBranchError
			r2.MergedInto = finRes.MergedInto
			r2.MergeStatus = store.MergeStatus(finRes.MergeStatus)
			r2.MergedCommit = finRes.MergedCommit
			if e.mergeStrategy != "" {
				r2.MergeStrategy = store.MergeStrategy(e.mergeStrategy)
			}
			r2.AutoMerge = e.autoMerge
			if saveErr := e.store.SaveRun(ctx, r2); saveErr != nil && e.logger != nil {
				e.logger.Warn("runtime: persist finalization metadata: %v", saveErr)
			}
		}
	}
	if finRes.FinalBranchError != "" {
		if err := e.emit(ctx, runID, store.EventWorktreeBranchFailed, "", map[string]interface{}{
			"sha":    finRes.FinalCommit,
			"reason": finRes.FinalBranchError,
		}); err != nil && e.logger != nil {
			e.logger.Warn("runtime: emit worktree_branch_failed event for %s: %v", runID, err)
		}
	}
	if cleanup != nil {
		cleanup()
	}
}

// reconstructWorktreeContext rebuilds a worktreeContext from a persisted
// run record on the resume path. The original setupWorktree-time
// `originalBranch` is not in `r.*` — re-read it from the live repo so
// finalizeWorktree can attempt the FF when the operator hasn't switched
// branches since launch. Returns nil when the run isn't a worktree run
// or when the persisted paths are empty.
func (e *Engine) reconstructWorktreeContext(r *store.Run) *worktreeContext {
	if r == nil || !r.Worktree || r.WorkDir == "" || r.RepoRoot == "" {
		return nil
	}
	// Skip if the worktree directory is gone (already finalized, or
	// operator removed it manually). finalizeWorktree handles a missing
	// dir gracefully, but skipping here avoids an unnecessary git call.
	if _, err := os.Stat(r.WorkDir); err != nil {
		return nil
	}
	originalBranch := ""
	if out, brErr := gitCmd("-C", r.RepoRoot, "symbolic-ref", "--quiet", "--short", "HEAD").Output(); brErr == nil {
		originalBranch = strings.TrimSpace(string(out))
	}
	return &worktreeContext{
		repoRoot:       r.RepoRoot,
		wtPath:         r.WorkDir,
		originalBranch: originalBranch,
		originalTip:    r.BaseCommit,
	}
}

// evictRunSessions clears any per-node session state still held by
// the executor for runID, except when the run is paused (human input
// awaited) — in which case Resume will pick up the same sessions.
func (e *Engine) evictRunSessions(runID string, loopErr error) {
	if errors.Is(loopErr, ErrRunPaused) {
		return
	}
	if ev, ok := e.executor.(interface{ EvictRun(string) }); ok {
		ev.EvictRun(runID)
	}
}

// execLoop is the shared execution loop used by both Run and Resume.
// It walks the graph from startNodeID until a terminal node, human pause,
// or error.
func (e *Engine) execLoop(ctx context.Context, rs *runState, startNodeID string) error {
	currentNodeID := startNodeID

	for {
		select {
		case <-ctx.Done():
			return e.handleContextDoneWithCheckpoint(rs, currentNodeID, ctx.Err())
		default:
		}

		node, ok := e.workflow.Nodes[currentNodeID]
		if !ok {
			return e.failRunWithCheckpoint(rs, currentNodeID,
				fmt.Sprintf("node %q not found", currentNodeID))
		}

		// --- Terminal nodes ---
		switch node.(type) {
		case *ir.DoneNode:
			if err := e.emitTerminalNodeEvents(rs, currentNodeID); err != nil {
				return err
			}
			// Best-effort status flip — the run logically succeeded the
			// moment we reached DoneNode, so a transient store-side
			// failure on the final status write must not flip a
			// successful run to "failed" (which would also skip
			// worktree finalize and orphan any commits the run
			// produced). Log and continue; run_finished still fires
			// below so observers see the terminal event.
			if err := e.store.UpdateRunStatus(rs.ctx, rs.runID, store.RunStatusFinished, ""); err != nil && e.logger != nil {
				e.logger.Warn("runtime: failed to persist run %s as finished: %v (run reached DoneNode — treating as success)", rs.runID, err)
			}
			return e.emit(rs.ctx, rs.runID, store.EventRunFinished, "", nil)
		case *ir.FailNode:
			if err := e.emitTerminalNodeEvents(rs, currentNodeID); err != nil {
				return err
			}
			return e.failRun(rs.ctx, rs.runID, currentNodeID, "workflow reached fail node")
		default:
			// non-terminal — continue below
		}

		// --- Human node ---
		if hn, ok := node.(*ir.HumanNode); ok {
			switch hn.Interaction {
			case ir.InteractionLLM:
				// Intentional fall-through: LLM interaction human nodes are
				// executed via the standard node path below (emit started →
				// budget check → executor.Execute → store output → emit
				// finished → select edge). The executor dispatches to
				// executeHumanLLM which handles model resolution and schema.
			case ir.InteractionLLMOrHuman:
				paused, err := e.execAutoOrPauseHuman(ctx, rs, currentNodeID, node)
				if err != nil {
					return err
				}
				if paused {
					return ErrRunPaused
				}
				// LLM decided no human needed — continue to edge selection.
				nextNodeID, err := e.selectEdgeRS(rs, currentNodeID, rs.outputs[currentNodeID])
				if err != nil {
					return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			default:
				// InteractionHuman (default) and InteractionNone both pause.
				return e.pauseAtHuman(rs, currentNodeID, node)
			}
		}

		// --- Router nodes ---
		if rn, ok := node.(*ir.RouterNode); ok {
			switch rn.RouterMode {
			case ir.RouterFanOutAll:
				nextNodeID, err := e.execFanOut(ctx, rs, currentNodeID)
				if err != nil {
					return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			case ir.RouterRoundRobin:
				nextNodeID, err := e.execRoundRobin(ctx, rs, currentNodeID)
				if err != nil {
					return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			case ir.RouterLLM:
				nextNodeID, err := e.execLLMRouter(ctx, rs, currentNodeID)
				if err != nil {
					return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
				}
				currentNodeID = nextNodeID
				continue
			}
			// RouterCondition falls through to normal execution path.
		}

		// --- Compute nodes execute deterministically inside the engine ---
		// (no LLM, no shell-out). Keep this path before emitting node_started
		// so it shares the same envelope as other nodes.
		if cn, ok := node.(*ir.ComputeNode); ok {
			nextNodeID, err := e.execCompute(rs, currentNodeID, cn)
			if err != nil {
				return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
			}
			currentNodeID = nextNodeID
			continue
		}

		// --- Emit node_started ---
		// Compute the loop iteration once so the event payload and the
		// executor's Task.Iteration agree. The frontend uses
		// data.iteration as the source of truth for the pip-strip UI,
		// but the reducer keys exec_id on data.iteration_path because a
		// single int collapses nested-loop executions onto the same id
		// (observed live: solo body nodes were stuck on the family_loop
		// counter so every package's validate_upgrade collided on
		// iter=5 → canvas showed nothing as running across 5+ pkgs).
		iter := e.currentLoopIteration(currentNodeID, rs.loopCounters)
		iterPath := e.currentLoopIterationPath(currentNodeID, rs.loopCounters)
		payload := map[string]interface{}{
			"kind":      node.NodeKind().String(),
			"iteration": iter,
		}
		if iterPath != "" {
			payload["iteration_path"] = iterPath
		}
		if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, currentNodeID, payload); err != nil {
			return err
		}

		// --- Check budget before execution ---
		if err := e.checkBudgetBeforeExec(rs, currentNodeID); err != nil {
			return err
		}

		// --- Build node input from edge mappings ---
		nodeInput := e.buildNodeInputRS(currentNodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)

		// --- Execute node ---
		// Thread the run ID into ctx so the executor can locate
		// per-node session state (used by Compactor implementations
		// to find the right messages list to compact + retry).
		execCtx := model.WithRunID(ctx, rs.runID)
		// Attach a template-data snapshot so the executor can resolve
		// `outputs.*`, `loop.*`, `artifacts.*`, and `run.*` refs in
		// prompt bodies. These namespaces complement the `input.*`
		// fields populated by edge `with`-mappings and the workflow-
		// level `vars.*`.
		execCtx = model.WithTemplateData(execCtx, e.buildTemplateData(rs))
		// Per-node span: inherits the runner-side or server-side root
		// span via ctx (W3C trace propagated through NATS in cloud
		// mode). Attributes mirror the node_started event payload so
		// trace + event log stay aligned.
		spanCtx, span := otel.Tracer(tracerName).Start(execCtx, "iterion.node.execute",
			trace.WithAttributes(
				attribute.String("iterion.run_id", rs.runID),
				attribute.String("iterion.node_id", currentNodeID),
				attribute.String("iterion.node_kind", node.NodeKind().String()),
			),
		)
		spanCtx = model.WithLoopIteration(spanCtx, iter)
		output, err := e.executor.Execute(spanCtx, node, nodeInput)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
		if err != nil {
			// Check if the delegate needs user interaction.
			var needsInput *model.ErrNeedsInteraction
			if errors.As(err, &needsInput) {
				return e.handleNeedsInteraction(ctx, rs, currentNodeID, node, needsInput, 0)
			}
			// Recovery dispatch (when wired via WithRecoveryDispatch):
			// classify the error, look up a recipe, and either retry,
			// pause, or fail terminally. Without a dispatcher, every
			// failure produces failed_resumable as before. The
			// run-ID-enriched ctx is passed so Compact() can locate
			// the per-node session.
			retry, recoveryErr := e.handleNodeFailure(execCtx, rs, currentNodeID, err)
			if recoveryErr != nil {
				return recoveryErr
			}
			if retry {
				continue
			}
			return e.failRunWithCheckpoint(rs, currentNodeID, fmt.Sprintf("node %q execution failed: %v", currentNodeID, err))
		}

		// Reset per-node retry counters on success so a future failure
		// starts fresh.
		delete(rs.nodeAttempts, currentNodeID)

		// Store output.
		rs.outputs[currentNodeID] = output

		// Validate output against declared schema (optional).
		if err := e.validateNodeOutput(currentNodeID, node, output); err != nil {
			return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
		}

		// Record budget usage and check limits.
		if err := e.recordAndCheckBudget(rs, currentNodeID, output); err != nil {
			return err
		}

		// Persist artifact if node has publish.
		if pub := nodePublish(node); pub != "" {
			version := rs.artifactVersions[currentNodeID]
			artifact := &store.Artifact{
				RunID:   rs.runID,
				NodeID:  currentNodeID,
				Version: version,
				Data:    output,
			}
			if err := e.store.WriteArtifact(ctx, artifact); err != nil {
				return fmt.Errorf("runtime: write artifact: %w", err)
			}
			rs.artifactVersions[currentNodeID] = version + 1
			rs.artifacts[pub] = output

			if err := e.emit(rs.ctx, rs.runID, store.EventArtifactWritten, currentNodeID, map[string]interface{}{
				"publish": pub,
				"version": version,
			}); err != nil {
				return fmt.Errorf("runtime: artifact written but event emission failed (state inconsistency): %w", err)
			}
		}

		// --- Emit node_finished with usage data ---
		nodeFinishedData := buildNodeFinishedData(sanitizeOutputForEvent(node, output))
		if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, currentNodeID, nodeFinishedData); err != nil {
			return err
		}
		if e.onNodeFinished != nil {
			e.onNodeFinished(currentNodeID, output)
		}

		// Best-effort checkpoint for resume-from-failed.
		if err := e.store.SaveCheckpoint(rs.ctx, rs.runID, buildCheckpoint(rs, currentNodeID)); err != nil {
			e.logger.Error("failed to save checkpoint after node %q: %v", currentNodeID, err)
		}

		// --- Select outgoing edge ---
		nextNodeID, err := e.selectEdgeRS(rs, currentNodeID, output)
		if err != nil {
			return e.failRunErrWithCheckpoint(rs, currentNodeID, err)
		}

		currentNodeID = nextNodeID
	}
}

// ---------------------------------------------------------------------------
// Compute nodes
// ---------------------------------------------------------------------------

// execCompute evaluates a ComputeNode's expressions deterministically and
// stores the result as the node's output. It mirrors the standard execution
// envelope (node_started → output → node_finished → checkpoint → edge select)
// without invoking the executor backend.
func (e *Engine) execCompute(rs *runState, nodeID string, cn *ir.ComputeNode) (string, error) {
	startedPayload := map[string]interface{}{
		"kind":      "compute",
		"iteration": e.currentLoopIteration(nodeID, rs.loopCounters),
	}
	if p := e.currentLoopIterationPath(nodeID, rs.loopCounters); p != "" {
		startedPayload["iteration_path"] = p
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, nodeID, startedPayload); err != nil {
		return "", err
	}

	nodeInput := e.buildNodeInputRS(nodeID, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)
	output := make(map[string]interface{}, len(cn.Exprs))

	exprCtx := e.exprContext(rs, nodeInput)
	for _, ce := range cn.Exprs {
		v, err := evalComputeExpr(ce.AST, exprCtx)
		if err != nil {
			return "", &RuntimeError{
				Code:    ErrCodeExecutionFailed,
				Message: fmt.Sprintf("compute %q: field %q expression %q: %v", nodeID, ce.Key, ce.Raw, err),
				NodeID:  nodeID,
				Hint:    "check the compute node's expressions for type mismatches or unknown references",
			}
		}
		output[ce.Key] = v
	}

	rs.outputs[nodeID] = output
	delete(rs.nodeAttempts, nodeID)

	if err := e.validateNodeOutput(nodeID, cn, output); err != nil {
		return "", err
	}

	if err := e.emit(rs.ctx, rs.runID, store.EventNodeFinished, nodeID, buildNodeFinishedData(sanitizeOutputForEvent(cn, output))); err != nil {
		return "", err
	}
	if e.onNodeFinished != nil {
		e.onNodeFinished(nodeID, output)
	}

	if err := e.store.SaveCheckpoint(rs.ctx, rs.runID, buildCheckpoint(rs, nodeID)); err != nil {
		e.logger.Error("failed to save checkpoint after compute %q: %v", nodeID, err)
	}

	return e.selectEdgeRS(rs, nodeID, output)
}

// ---------------------------------------------------------------------------
// Edge selection
// ---------------------------------------------------------------------------

// selectEdgeRS picks the next node by evaluating outgoing edges from the
// current node, threading the runState so expression-form `when` clauses
// can resolve `{{loop.*}}` / `{{run.*}}` namespaces and so loop edges
// snapshot the source node's output as `loop.<name>.previous_output` for
// the next iteration. Conditional edges are checked first; the first
// matching unconditional edge serves as fallback. When a loop's counter is
// exhausted that edge is skipped — enabling graceful exit patterns like
// `fix_loop -> outer_loop` or `loop_edge -> done`.
func (e *Engine) selectEdgeRS(rs *runState, fromNodeID string, output map[string]interface{}) (string, error) {
	selected := e.evaluateEdgesWithLoopsRS(fromNodeID, "main", output, rs)
	if selected == nil {
		return "", &RuntimeError{
			Code:    ErrCodeNoOutgoingEdge,
			Message: fmt.Sprintf("no outgoing edge from node %q", fromNodeID),
			NodeID:  fromNodeID,
			Hint:    "ensure the node's output matches at least one edge condition, or add an unconditional fallback edge",
		}
	}

	// Reset loop counters when we re-enter the loop at its TOP — i.e.
	// when a non-loop edge targets one of the loop's entry nodes
	// (target of a loop-bearing back-edge) from a source that isn't
	// part of the loop body. That signals a fresh outer iteration
	// driving a fresh loop instance (e.g. package_loop pushes into
	// Phase 1, which lands on validate_upgrade — the fix_loop entry —
	// from outside the body via align_code).
	//
	// Earlier this fired on ANY body-node target, which over-reset
	// when computeLoopBodies couldn't intersect forward+reverse BFS
	// past intermediate loop edges and yielded a minimal-endpoints
	// body. Concrete case: recovery_loop's body was just {alt_review,
	// review_commit_auto} because the cycle goes through review_loop's
	// back-edge; the non-loop edge fix_X → review_commit_auto then
	// reset the counter every cycle and review_commit_auto's
	// iteration_path stuck at recovery_loop=0, collapsing every
	// invocation into one snapshot row. Scoping the reset to the
	// loop's entries fixes the false positive while still resetting
	// when a parent iteration legitimately re-enters.
	if selected.LoopName == "" {
		for loopName, loop := range e.workflow.Loops {
			if loop == nil || len(loop.Entries) == 0 {
				continue
			}
			if !loop.Entries[selected.To] || loop.Body[selected.From] {
				continue
			}
			if prior, ok := rs.loopCounters[loopName]; ok && prior > 0 {
				e.logger.Debug("loop %q: re-entered via edge %s→%s — counter reset from %d", loopName, selected.From, selected.To, prior)
				rs.loopCounters[loopName] = 0
				if rs.loopPreviousOutput != nil {
					delete(rs.loopPreviousOutput, loopName)
					delete(rs.loopCurrentOutput, loopName)
				}
			}
		}
	}

	if selected.LoopName != "" {
		rs.loopCounters[selected.LoopName] = rs.loopCounters[selected.LoopName] + 1
		// Rotate snapshots so {{loop.<name>.previous_output}} reads the
		// snapshot from the PRIOR traversal (one iteration behind), not the
		// current one. The current iteration's source output is staged in
		// loopCurrentOutput and only promoted to loopPreviousOutput at the
		// NEXT loop-edge crossing for the same loop name.
		if rs.loopPreviousOutput != nil {
			if staged, ok := rs.loopCurrentOutput[selected.LoopName]; ok {
				rs.loopPreviousOutput[selected.LoopName] = staged
			}
			snap := make(map[string]interface{}, len(output))
			for k, v := range output {
				snap[k] = v
			}
			rs.loopCurrentOutput[selected.LoopName] = snap
		}
	}

	data := map[string]interface{}{
		"from": selected.From,
		"to":   selected.To,
	}
	if selected.Condition != "" {
		data["condition"] = selected.Condition
		data["negated"] = selected.Negated
	}
	if selected.ExpressionSrc != "" {
		data["expression"] = selected.ExpressionSrc
	}
	if selected.LoopName != "" {
		data["loop"] = selected.LoopName
		data["iteration"] = rs.loopCounters[selected.LoopName]
	}
	if err := e.emit(rs.ctx, rs.runID, store.EventEdgeSelected, "", data); err != nil {
		e.logger.Warn("failed to emit edge_selected: %v", err)
	}

	return selected.To, nil
}

// ---------------------------------------------------------------------------
// Input resolution
// ---------------------------------------------------------------------------

// buildNodeInputRS constructs the input map for a node by looking at the
// edge `with` mappings that target this node. For convergence points,
// mappings from ALL resolved incoming edges are merged. If no mappings
// exist, the run-level inputs are used for the entry node. The runState
// is required so that `{{loop.*}}` / `{{run.*}}` references resolve
// against the run's iteration state. Pass nil for rs only in tests that
// don't exercise those namespaces.
func (e *Engine) buildNodeInputRS(nodeID string, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}, artifacts map[string]map[string]interface{}, rs *runState) map[string]interface{} {
	result := make(map[string]interface{})

	// Merge with-mappings from ALL edges targeting this node whose source
	// has already produced output.
	for _, edge := range e.workflow.Edges {
		if edge.To != nodeID || len(edge.With) == 0 {
			continue
		}
		// Only use mappings from an edge whose source has already produced output.
		if _, ok := outputs[edge.From]; !ok && edge.From != "" {
			continue
		}

		// Build effective input context: {{input.X}} in with-mappings should
		// resolve from the edge source's output (e.g. a router's pass-through
		// input) with run-level inputs as fallback.
		effectiveInputs := runInputs
		if sourceOut := outputs[edge.From]; sourceOut != nil {
			effectiveInputs = make(map[string]interface{}, len(runInputs)+len(sourceOut))
			for k, v := range runInputs {
				effectiveInputs[k] = v
			}
			for k, v := range sourceOut {
				effectiveInputs[k] = v
			}
		}

		for _, dm := range edge.With {
			val := e.resolveMapping(dm, vars, outputs, effectiveInputs, artifacts, rs)
			// Include nil values too: a ref that resolves to nil
			// (e.g. `{{outputs.fixer.pushback}}` before the fixer
			// has run, `{{loop.X.previous_output}}` on iteration 1)
			// is still a *valid* mapping — the field exists, its
			// value is just empty. Dropping it would leave
			// `{{input.<key>}}` placeholders unresolved in
			// downstream prompts, surfacing template syntax to the
			// LLM instead of an empty string.
			result[dm.Key] = val
		}
	}

	if len(result) > 0 {
		return result
	}

	// Fallback: for the entry node merge workflow var defaults with run-level
	// inputs so that {{input.X}} references resolve to the var default when
	// --var X=... was not provided on the CLI. Without this, vars declared
	// with a default like `scope_notes: string = ""` are missing from the
	// entry node's input map and the placeholder is left literal in prompts.
	// CLI inputs override defaults.
	if nodeID == e.workflow.Entry {
		for name, v := range e.workflow.Vars {
			if v.HasDefault {
				result[name] = v.Default
			}
		}
		for k, v := range runInputs {
			result[k] = v
		}
	}

	return result
}

// resolveMapping resolves a DataMapping's references to concrete values.
// For simplicity in the minimal runtime, if there is exactly one ref we
// return the resolved value directly; otherwise we return the raw template.
func (e *Engine) resolveMapping(dm *ir.DataMapping, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}, artifacts map[string]map[string]interface{}, rs *runState) interface{} {
	if len(dm.Refs) == 1 {
		return e.resolveRef(dm.Refs[0], vars, outputs, runInputs, artifacts, rs)
	}
	return dm.Raw
}

// resolveRef resolves a single Ref to a concrete value. The runState is
// required for `loop` and `run` namespace resolution; pass nil to skip
// those (they'll resolve to nil).
func (e *Engine) resolveRef(ref *ir.Ref, vars map[string]interface{}, outputs map[string]map[string]interface{}, runInputs map[string]interface{}, artifacts map[string]map[string]interface{}, rs *runState) interface{} {
	switch ref.Kind {
	case ir.RefVars:
		if len(ref.Path) > 0 {
			return vars[ref.Path[0]]
		}
	case ir.RefInput:
		if len(ref.Path) > 0 {
			return runInputs[ref.Path[0]]
		}
	case ir.RefOutputs:
		if len(ref.Path) == 0 {
			return nil
		}
		nodeOut := outputs[ref.Path[0]]
		if nodeOut == nil {
			return nil
		}
		if len(ref.Path) == 1 {
			return nodeOut
		}
		return nodeOut[ref.Path[1]]
	case ir.RefArtifacts:
		if len(ref.Path) > 0 {
			return artifacts[ref.Path[0]]
		}
	case ir.RefLoop:
		if rs == nil || len(ref.Path) < 2 {
			return nil
		}
		return e.resolveLoopPath(ref.Path, rs)
	case ir.RefRun:
		if rs == nil || len(ref.Path) == 0 {
			return nil
		}
		switch ref.Path[0] {
		case "id":
			return rs.runID
		}
	}
	return nil
}

// resolveLoopPath resolves a {{loop.<name>.<field>[.subfield…]}} reference.
// Recognized fields:
//
//	iteration       — current loop counter (int64)
//	max             — effective cap (int64): the literal int for plain
//	                   caps, the resolved template value for templated caps
//	previous_output — snapshot of the source node output at the previous
//	                   traversal of this loop's edge; sub-fields drill in.
func (e *Engine) resolveLoopPath(path []string, rs *runState) interface{} {
	loopName := path[0]
	switch path[1] {
	case "iteration":
		return int64(rs.loopCounters[loopName])
	case "max":
		if l, ok := e.workflow.Loops[loopName]; ok {
			return int64(e.resolveLoopMax(l, rs))
		}
		return nil
	case "previous_output":
		return drillPath(rs.loopPreviousOutput[loopName], path[2:])
	}
	return nil
}

// resolveLoopMax returns the effective cap for a loop. Literal-int
// declarations (`as fix_loop(3)`) yield MaxIterations directly.
// Template declarations (`as fix_loop("{{outputs.X.cap}}")`) resolve
// the refs against the runState and coerce the result to int. The
// fallback when resolution / coercion fails is loop.MaxIterations
// (typically 0 for the template form) — that surfaces as a "loop
// exhausted on iteration 0" log line at the edge check, which is the
// loudest visible failure mode we can offer without aborting the run.
func (e *Engine) resolveLoopMax(loop *ir.Loop, rs *runState) int {
	if loop.MaxIterationsExpr == "" || len(loop.MaxIterationsExprRefs) == 0 {
		return loop.MaxIterations
	}
	var resolved interface{}
	for _, ref := range loop.MaxIterationsExprRefs {
		v := e.resolveRef(ref, rs.vars, rs.outputs, rs.runInputs, rs.artifacts, rs)
		if v != nil {
			resolved = v
		}
	}
	if resolved == nil {
		return loop.MaxIterations
	}
	if n, ok := coerceToInt(resolved); ok {
		return n
	}
	return loop.MaxIterations
}

// coerceToInt accepts the common shapes that an output/var ref can
// carry for a numeric value: native ints, float64 (the JSON decoder
// default), json.Number, and decimal-string scalars (some JS nodes
// emit numbers as strings). Returns false for anything else.
func coerceToInt(v interface{}) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case int32:
		return int(x), true
	case float64:
		return int(x), true
	case float32:
		return int(x), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n), true
		}
		if f, err := x.Float64(); err == nil {
			return int(f), true
		}
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n, true
		}
	}
	return 0, false
}

// buildTemplateData assembles a model.TemplateData snapshot from the
// current run state. It is attached to ctx before each node execution
// so the executor can resolve `outputs.*`, `loop.*`, `artifacts.*`,
// and `run.*` refs in prompt bodies. Maps are passed by reference —
// the executor must treat them as read-only.
func (e *Engine) buildTemplateData(rs *runState) *model.TemplateData {
	loopMax := make(map[string]int, len(e.workflow.Loops))
	for name, l := range e.workflow.Loops {
		if l != nil {
			loopMax[name] = e.resolveLoopMax(l, rs)
		}
	}
	return &model.TemplateData{
		Outputs:            rs.outputs,
		LoopCounters:       rs.loopCounters,
		LoopMaxIterations:  loopMax,
		LoopPreviousOutput: rs.loopPreviousOutput,
		Artifacts:          rs.artifacts,
		RunID:              rs.runID,
		Attachments:        rs.attachments,
	}
}

// loadAttachmentInfos populates the per-run attachment view consumed
// by template references. Called once after CreateRun (and any
// promote callback) so Run.Attachments is authoritative.
//
// Path computation:
//   - Filesystem stores: <root>/runs/<id>/attachments/<name>/<filename>
//     pre-resolved into the AttachmentRecord.StorageRef.
//   - Cloud / non-FS stores: Path is left empty; nodes that need bytes
//     access them via the URL accessor (presigned).
func (e *Engine) loadAttachmentInfos(ctx context.Context, runID string) map[string]model.AttachmentInfo {
	if e.store == nil {
		return nil
	}
	list, err := e.store.ListAttachments(ctx, runID)
	if err != nil || len(list) == 0 {
		return nil
	}
	storeRoot := e.store.Root()
	out := make(map[string]model.AttachmentInfo, len(list))
	for _, rec := range list {
		info := model.AttachmentInfo{
			Name:             rec.Name,
			OriginalFilename: rec.OriginalFilename,
			MIME:             rec.MIME,
			Size:             rec.Size,
			SHA256:           rec.SHA256,
		}
		// FS stores keep StorageRef as a path relative to Root(); join
		// it back into an absolute host path so prompts/tools can open
		// the file directly.
		if storeRoot != "" && rec.StorageRef != "" {
			info.Path = filepath.Join(storeRoot, filepath.FromSlash(rec.StorageRef))
		}
		// Lazy presign — capture the loop var by value so each closure
		// targets its own attachment.
		recCopy := rec
		runIDCopy := runID
		store := e.store
		info.PresignURL = func() (string, error) {
			return store.PresignAttachment(ctx, runIDCopy, recCopy.Name, 10*time.Minute)
		}
		out[rec.Name] = info
	}
	return out
}

// drillPath walks a nested map[string]interface{} structure by the given
// path, returning the final value (or nil if any segment is missing or
// non-map). Used by every reference resolver that needs to descend into
// node outputs / artifacts / loop snapshots.
func drillPath(root interface{}, path []string) interface{} {
	cur := root
	for _, key := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

// resolveVars builds the vars map from workflow variable defaults,
// coercing user-provided override strings to the declared type.
//
// Coercion is necessary because the CLI's --var flag and the HTTP
// /api/runs endpoint both deliver vars as raw strings. Without
// coercion, an explicit "--var loop_count=3" stores the var as the
// string "3", which then fails downstream comparisons against the
// typed defaults (e.g. "input.count >= vars.loop_count" tries to
// compare a number against a string and aborts the run with an
// opaque "cannot compare X >= string" error). Defaults from the
// .iter source are already typed by the IR compiler — we coerce
// only on overrides.
func (e *Engine) resolveVars(inputs map[string]interface{}) map[string]interface{} {
	vars := make(map[string]interface{})
	// expandFn lets var values reference ${PROJECT_DIR} (resolved to the
	// engine's workDir, possibly a worktree path) and any other env var.
	// Applied to both string defaults AND string user-provided overrides:
	// the studio's LaunchView pre-fills its form with the literal default
	// (e.g. "${PROJECT_DIR}") so an unmodified submit re-sends it as an
	// override, which would otherwise reach tool nodes verbatim and break
	// `git -C '${PROJECT_DIR}'`. Expanding overrides in the same pass
	// keeps `vars.workspace_dir` resolved to a real path regardless of
	// whether it came from the workflow default or the form input.
	expandFn := func(key string) string {
		if key == "PROJECT_DIR" {
			// In sandbox mode, ${PROJECT_DIR} must resolve to the
			// in-container bind-mount target (e.g. /workspace), not
			// the host worktree path. Tool nodes and prompts using
			// this var are consumed by processes RUNNING inside the
			// container — they cannot open /home/<host-user>/...
			// paths because they're not mounted there. The container
			// workspace IS the host worktree, just at a different
			// pathname.
			if e.containerWorkspace != "" {
				return e.containerWorkspace
			}
			return e.workDir
		}
		return os.Getenv(key)
	}
	for name, v := range e.workflow.Vars {
		if v.HasDefault {
			if s, ok := v.Default.(string); ok {
				vars[name] = os.Expand(s, expandFn)
			} else {
				vars[name] = v.Default
			}
		}
	}
	for k, v := range inputs {
		decl, isVar := e.workflow.Vars[k]
		if !isVar {
			continue
		}
		coerced, err := coerceVarValue(v, decl.Type)
		if err != nil {
			// Fall back to whatever the caller passed; the engine's
			// downstream type checks will surface a clear error if
			// the value really is incompatible. The alternative —
			// failing the run here — would be more aggressive than
			// the previous behaviour.
			e.logger.Warn("runtime: var %q: coerce to %s failed: %v (using raw value)", k, decl.Type, err)
			vars[k] = v
			continue
		}
		if s, ok := coerced.(string); ok {
			coerced = os.Expand(s, expandFn)
		}
		vars[k] = coerced
	}
	return vars
}

// coerceVarValue narrows a user-provided override (typically a
// string from --var or POST /api/runs) to the type declared in the
// IR for that var. Already-typed values pass through.
func coerceVarValue(v interface{}, vt ir.VarType) (interface{}, error) {
	s, isStr := v.(string)
	if !isStr {
		return v, nil
	}
	switch vt {
	case ir.VarString:
		return s, nil
	case ir.VarBool:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no", "":
			return false, nil
		default:
			return nil, fmt.Errorf("invalid bool %q", s)
		}
	case ir.VarInt:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid int %q: %w", s, err)
		}
		return n, nil
	case ir.VarFloat:
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid float %q: %w", s, err)
		}
		return n, nil
	case ir.VarJSON:
		// Parse JSON; if the user gave us non-JSON text, leave it
		// as a string — JSON expressions accept either.
		var out interface{}
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return s, nil
		}
		return out, nil
	case ir.VarStringArray:
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			return []interface{}{}, nil
		}
		// Accept either JSON array form (["a","b"]) or
		// comma-separated (a,b).
		if strings.HasPrefix(trimmed, "[") {
			var arr []interface{}
			if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
				return arr, nil
			}
		}
		parts := strings.Split(trimmed, ",")
		out := make([]interface{}, len(parts))
		for i, p := range parts {
			out[i] = strings.TrimSpace(p)
		}
		return out, nil
	default:
		return s, nil
	}
}

// emitTerminalNodeEvents emits the NodeStarted+NodeFinished pair for a
// terminal node (DoneNode, FailNode). Both events fire so the run
// console renders the terminal step like any other; the iteration tag
// matches the loop-counter at the moment the node was reached. Bails
// on the first emit error.
func (e *Engine) emitTerminalNodeEvents(rs *runState, nodeID string) error {
	iter := map[string]interface{}{"iteration": e.currentLoopIteration(nodeID, rs.loopCounters)}
	if err := e.emit(rs.ctx, rs.runID, store.EventNodeStarted, nodeID, iter); err != nil {
		return err
	}
	return e.emit(rs.ctx, rs.runID, store.EventNodeFinished, nodeID, nil)
}
