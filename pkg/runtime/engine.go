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
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/backend/recipe"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
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

// ErrRunInterrupted marks a run cancelled by INFRASTRUCTURE — a cloud
// runner draining on deploy (lame-duck) or a lost lease heartbeat — rather
// than by operator intent. A caller requests it by cancelling the run
// context with this as the cause (context.CancelCause); the engine then
// writes failed_resumable (not cancelled) and emits a resumable run_failed
// event (not run_cancelled), so the run auto-resumes on a healthy pod
// instead of being dropped like a deliberate cancel. This single-sources
// the resumable-vs-terminal decision in the engine, next to the existing
// Canceled-vs-DeadlineExceeded split in handleContextDoneWithCheckpoint.
var ErrRunInterrupted = errors.New("runtime: run interrupted (resumable)")

// ErrRunPausedOperator is returned when execution is suspended in
// response to a POST /api/runs/{id}/pause request — the operator
// asked for a soft pause (no cancellation) that resumes via the
// same checkpoint machinery as cancelled runs.
var ErrRunPausedOperator = errors.New("runtime: run paused by operator")

// ErrServerDraining is returned by the runview Service when Launch or
// Resume is called after the server has begun graceful shutdown. The
// HTTP layer translates this to 503 Service Unavailable.
var ErrServerDraining = errors.New("runtime: server draining")

// ErrUsageCapped is returned by the runview Service when the operator's
// subscription usage cap (pkg/usagecap) leaves no headroom to start new
// work. The HTTP layer translates it to 429 Too Many Requests — the run was
// refused by a quota, and the caller may retry once the window reopens
// (the error text says when). A run already claimed by a cloud runner takes
// the other road instead: it parks with a durable retry.
var ErrUsageCapped = errors.New("runtime: usage cap reached")

// NodeExecutor is the abstraction called by the engine to actually run a
// node (LLM call, tool invocation, etc.). The runtime itself is agnostic
// to the concrete implementation — tests supply stubs, production code
// plugs in real providers.
type NodeExecutor interface {
	// Execute runs the given node with the provided input and returns its
	// output. For terminal nodes (done/fail) this is never called.
	Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error)
}

// The following minimal interfaces are optional extensions to NodeExecutor:
// the engine type-asserts the configured executor against each and, on a
// match, pushes the corresponding launch-time state in (workDir, repoRoot,
// resolved vars). Hoisted to package scope so the workspace-persist /
// runInitState / restoreRunEnv / pushExecutorVars paths share one
// declaration instead of redeclaring the same anonymous interface inline.

type workDirSetter interface{ SetWorkDir(string) }

type repoRootSetter interface{ SetRepoRoot(string) }

type varsSetter interface{ SetVars(map[string]any) }

// Engine executes workflows. It supports sequential execution and
// parallel fan-out via bounded branch scheduling.
type Engine struct {
	workflow                 *ir.Workflow
	store                    store.RunStore
	executor                 NodeExecutor
	logger                   *iterlog.Logger
	onNodeFinished           func(runID, nodeID string, output map[string]any)
	onEvent                  func(evt store.Event)    // optional observer fired after every successful append
	recoveryDispatch         RecoveryDispatch         // optional; consulted on node execution failure
	workflowHash             string                   // SHA-256 of the .bot source, set via WithWorkflowHash
	workflowSource           string                   // .bot text at launch, set via WithWorkflowSource (else read from filePath)
	workspaceTracker         workspacetrack.Tracker   // iterion-owned workspace versioning; nil = disabled (see WithWorkspaceTracker)
	filePath                 string                   // absolute .bot source path, set via WithFilePath
	parentRunID              string                   // immediate parent run, set via WithParentRunID for nested executions
	parentNodeID             string                   // IR node id of the parent's subbot node that spawned this run, set via WithParentNodeID
	preset                   string                   // in-source preset name selected at launch, set via WithPreset
	extraSkills              []string                 // operator-added skill-library skills (--skill / ITERION_SKILLS), unioned with the workflow's own; set via WithExtraSkills
	extraSkillsOrigin        string                   // "flag" | "env" — where extraSkills came from, reported on the skills_injected event
	runName                  string                   // deterministic human-friendly run label, set via WithRunName
	source                   *store.RunSource         // originating action metadata (dispatcher → issue ref), set via WithSource
	mergeInto                string                   // worktree finalization: FF target ("" = current branch, "none" = skip, or branch name); set via WithMergeInto
	branchName               string                   // worktree finalization: storage branch override ("" = iterion/run/<runName>); set via WithBranchName
	mergeStrategy            string                   // worktree finalization: "squash" (default) or "merge" (FF); set via WithMergeStrategy
	autoMerge                bool                     // worktree finalization: when true, apply mergeStrategy at end of run; otherwise leave merge_status=pending for UI; set via WithAutoMerge
	modelOverrides           []store.RunModelOverride // launch-time per-node/-group model/backend pins, persisted display-only on the run so the studio Overview shows what it launched with; set via WithModelOverrides
	permissionOverride       string                   // launch-time tool-permission gate override, persisted so every resume keeps the operator's choice; set via WithPermissionOverride
	validateOutputs          bool                     // when true, validate node outputs against declared schemas
	forceResume              bool                     // when true, skip workflow hash check on resume
	workDir                  string                   // working directory for subprocesses + PROJECT_DIR expansion; defaults to os.Getwd() at Run() time
	workDirDelegated         bool                     // true when workDir was handed to the engine explicitly (WithWorkDir) — the gate for adopting a linked-worktree workspace as a managed baseline; a defaulted CWD never grants finalization authority
	repoRoot                 string                   // source-of-truth repo root (project_root memory + ${PROJECT_MEMORY_DIR} expansion); empty until runRun resolves it
	containerWorkspace       string                   // when sandbox is active, the in-container path the host workDir is bind-mounted to (e.g. "/workspace"); used to remap ${PROJECT_DIR} so prompts and tool nodes see paths the in-container processes can actually open
	workspaceIntegrity       WorkspaceIntegrity       // sandbox-side HEAD captured at teardown for export-based drivers (zero when not applicable); read via SandboxWorkspaceIntegrity after Run/Resume returns
	attachmentsContainerDir  string                   // in-container path the run's attachments dir is bind-mounted at; empty when nodes must read them from the host (no sandbox, degraded sandbox, or a driver that drops host binds). Authoritative only once sandboxSettled is true
	sandboxSettled           bool                     // true once startSandbox has run: attachmentsContainerDir is then a FACT, not a forecast. Before that, attachmentPath falls back to a pre-flight prediction (the resume path resolves file answers before the bootstrap)
	sandboxOverride          string                   // CLI/Launch-level sandbox mode override; "" means "no override" (workflow + global default win); set via WithSandboxOverride
	sandboxDefault           string                   // global ITERION_SANDBOX_DEFAULT value snapshot; set via WithSandboxDefault
	sandboxDefaultImage      string                   // image ref used as fallback when sandbox: auto and no .devcontainer/devcontainer.json is found; "" lets the runtime pick the built-in pinned to the iterion version; set via WithSandboxDefaultImage
	sandboxHostStateOverride string                   // CLI/Launch-level override for sandbox.host_state ("auto"|"none"|""); set via WithSandboxHostStateOverride
	sandboxHostStateDefault  string                   // global ITERION_SANDBOX_HOST_STATE snapshot; set via WithSandboxHostStateDefault
	loopBudgetGuardOverride  string                   // CLI/Launch-level loop_budget_guard override ("on"|"off"|""); highest precedence, above the workflow block; set via WithLoopBudgetGuard
	repoDevboxOverride       string                   // CLI/Launch-level repo_devbox override ("on"|"off"|""); highest precedence, above the workflow block; set via WithRepoDevbox
	attachmentPromote        AttachmentPromoteFunc    // optional: invoked after CreateRun to materialise attachments
	bundle                   *bundle.Bundle           // optional: bundle backing this run; nil for plain .bot runs
	contributions            *Contributions           // optional: pre-resolved plugin/library skills (cloud runner pods have no iterion home); nil = resolve locally. Set via WithContributions
	pauseSignal              <-chan struct{}          // optional: closed by Service.Pause to request a soft pause at the next safe boundary; nil disables operator pause
	overrideCh               <-chan *OverrideMsg      // optional: live-steering commands drained at the same safe boundary (see override.go); nil disables steering
	dailyCap                 *DailyCapGuard           // optional: per-(store, UTC-day) spend cap; nil disables it. Set via WithDailyCap
	callbackURL              string                   // optional: run-completion webhook target persisted on the run; set via WithCallback
	callbackToken            string                   // optional: opaque correlation token echoed in the completion payload; set via WithCallback
	callbackAnswerNode       string                   // optional: node whose latest artifact holds the run's final answer; set via WithCallback
	boardMCPHandler          http.Handler             // optional: serves the board MCP routes; when set + a sandbox is active, a per-run gateway-reachable listener is started so sandboxed board-cap nodes can write the operator's board (C082). Set via WithBoardMCP; nil disables sandboxed board-emit (CLI runs with no server).
	subbotRunner             SubbotRunner             // optional: host-supplied closure that compiles + runs a child .bot for a `subbot` node. nil → subbot nodes hard-error (the runtime can't compile a child itself — import cycle with runview). Set via WithSubbotRunner.
	sandboxRunObserver       func(sandbox.Run)        // optional: invoked with the live sandbox Run right after it starts, so the host (cloud runner) can drive mid-run file-secret refresh against the driver's SecretFileRefresher. nil disables it. Set via WithSandboxRunObserver.
	answersBell              answersDoorbell          // in-process fast-path waking await_answers nodes when an async interaction is answered (ADR-081); rung via NotifyInteractionAnswered

	// activeBudget points at the SharedBudget of the run currently
	// executing in this engine, published atomically by newRunState so an
	// out-of-band observer (the runview/runner active-duration stamping
	// callback) can read the run's monotonic active elapsed without
	// locking the run loop. nil until a run starts, and whenever the
	// workflow declares no budget (then ActiveElapsed returns 0 and the
	// studio falls back to the wall-clock display for that run). One run
	// per engine, so a single slot suffices.
	activeBudget atomic.Pointer[SharedBudget]
}

// ActiveElapsed returns the monotonic active time consumed by the run
// currently executing in this engine, or 0 when no run is active or the
// workflow declares no budget. The value comes from the run's
// SharedBudget (CLOCK_MONOTONIC via startedAt): OS-suspend time is
// EXCLUDED (the monotonic clock freezes while the machine sleeps), long
// LLM thinking IS counted, and prior active time is preserved across
// resume (Restore shifts startedAt back). This is the engine-
// authoritative active-duration source the studio surfaces via
// Event.ActiveMs, replacing the wall-clock event-window derivation that
// miscounted suspend. Safe for concurrent use.
func (e *Engine) ActiveElapsed() time.Duration {
	b := e.activeBudget.Load()
	if b == nil {
		return 0
	}
	_, _, _, elapsed, _, _ := b.Snapshot()
	return elapsed
}

// SubbotRequest is the payload a SubbotNode hands to the host-supplied
// SubbotRunner: the child .bot source, the resolved input vars, and the
// parent linkage so the runner can record a child run tied to the parent.
type SubbotRequest struct {
	Source      string         // child .bot path/ref (relative to the parent workdir)
	Vars        map[string]any // resolved `with:` mappings + `_lease_<resource>` instance ids
	ParentRunID string
	NodeID      string
	// ReattachKey uniquely identifies THIS execution of the subbot node
	// (node id + loop-iteration path + fan-out branch id) so the runner can
	// persist the child run id under it on the parent and, on a resumed
	// re-execution, re-attach to that in-flight/finished child instead of
	// spawning a fresh one. Stable across resume (branch ids are
	// deterministic), unique per concurrent execution (branch id
	// disambiguates fan-out) and per loop iteration (iteration path
	// disambiguates loops). Mongo-field-safe (no '.'/'$'). Empty disables
	// re-attach (the runner always spawns fresh).
	ReattachKey string
	// WorkDir is the parent engine's EFFECTIVE working directory at the moment
	// the subbot node runs — not the one it was launched with. Under the
	// default `worktree: auto` the engine swaps its workDir for a fresh per-run
	// worktree (engine_run.go), so a runner that reused the launch-time path
	// would point the child at a checkout holding none of the parent's work,
	// including anything the parent committed. Advisory: empty means "the host
	// decides".
	WorkDir string
}

// SubbotRunner compiles and runs a child .bot as a nested run and returns its
// terminal output (mapped to outputs.<subbot>.<field>). Wired by the CLI /
// runview layer where compiling + running a child engine is possible.
type SubbotRunner func(ctx context.Context, req SubbotRequest) (map[string]any, error)

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
//
// CONCURRENCY CONTRACT (no mutex by design — ownership, not exclusion):
// the main execution-loop goroutine is the SINGLE WRITER of every
// unsynchronized field (outputs, artifacts, loopCounters, vars,
// costUSDTotal, ...). Parallel fan-out branches never touch them:
//   - branches receive deep COPIES of outputs/artifacts
//     (fanOutPlan.parentOutputs via copyOutputs) and write only into
//     their own branchResult; the merge back into rs happens
//     mono-thread in processConvergence after collection;
//   - the explicitly synchronized exceptions are branchLedgerSeq
//     (atomic), budget (SharedBudget, internal mutex), events
//     (runEvents, internal mutex) and resourceSemaphores (channels).
//
// Two rules keep this sound — breaking either introduces a silent data
// race the compiler cannot catch:
//  1. never write an unsynchronized rs field from a branch goroutine;
//  2. fields branches READ through the resolution scope (loopCounters,
//     vars, runInputs) must not be mutated by the main loop while a
//     fan-out is in flight — INCLUDING an abandoned branch that
//     outlives its fan-out (collectBranches' grace-period escape), so
//     the constraint extends until the run ends, not just until the
//     collector returns. TestFanOutAbandonedBranchDoesNotRaceRunState
//     exercises that window under -race.
type runState struct {
	// ctx is the per-run context. Stored on runState (despite the
	// usual "no context in struct" rule) because helpers.go threads
	// `rs *runState` deeply through emit/failRun*/checkpoint paths
	// where adding ctx to every signature would 80+ call sites with
	// no semantic gain — the lifetime of rs IS the lifetime of ctx.
	// Set in Run() before execLoop().
	ctx          context.Context
	runID        string
	runInputs    map[string]any
	vars         map[string]any
	outputs      map[string]map[string]any
	artifacts    map[string]map[string]any // publish name → output
	loopCounters map[string]int
	// loopOverrides holds the live-steering iteration grants (bump_loop):
	// loop name → extra iterations added to the loop's resolved max.
	// Written only by the execution-loop goroutine (applyOverride) and
	// re-seeded at resume from Run.LoopOverrides. nil until first bump.
	loopOverrides map[string]int
	// permissionGrants accumulates the `allow always` rules earned at a
	// permission-gate pause, keyed by the node that earned them and
	// ordered by grant. Every pause is resumed by a fresh engine with a
	// fresh runState, so a grant that lived only in the node input
	// reached exactly ONE re-invocation and then vanished — the operator
	// re-authorized the same tool at every pause while the runtime told
	// them it had been added "for the rest of this run". Re-seeded at
	// resume from Run.PermissionGrants. Per node, never run-wide: an
	// allow rule outranks the mode default, so a shared set would let an
	// approval on a permissive node unlock one that declared
	// `permission: deny`.
	permissionGrants map[string][]string
	// loopPreviousOutput holds the snapshot of the source node output from
	// the PREVIOUS traversal of a given loop's edge — i.e., one iteration
	// behind the current one. Workflows reference it as
	// {{loop.<name>.previous_output[.field]}}; in the very first iteration
	// of a loop the value is nil. The snapshot is rotated through
	// loopCurrentOutput on each traversal to preserve the one-iteration lag.
	loopPreviousOutput map[string]map[string]any
	loopCurrentOutput  map[string]map[string]any // staging slot for the next iteration's "previous"
	// loopProgressSig / loopStaleness drive the unbounded-loop liveness monitor:
	// the last-seen progress signature (a hash of the source output) per loop,
	// and how many consecutive crossings it has been unchanged. Reset when the
	// signal changes or the loop is re-entered. Not persisted across resume —
	// a resumed run simply starts its stall window fresh.
	loopProgressSig map[string]string
	loopStaleness   map[string]int
	// loopBudgetMarks prices one iteration of each loop: what the run had
	// consumed, per enforced budget dimension, when that loop was entered
	// or last crossed its back-edge. Persisted on the checkpoint, so a
	// resumed run keeps measuring across the pause.
	loopBudgetMarks    map[string]loopBudgetMark
	roundRobinCounters map[string]int
	// selectedIncoming records, per destination node, the incoming edges
	// routing actually selected for the current visit of that node. Fan-out
	// branches keep a private copy on branchResult so concurrent writers
	// cannot race this map; the trunk copies the join union at
	// processConvergence. Re-seeded at resume from Checkpoint.SelectedIncoming.
	selectedIncoming map[string][]store.IncomingEdge
	// events is the run-scoped reliable event registry backing the emit/wait
	// node primitives (ADR-051). Sticky: a wait that arrives after the emit
	// still observes it. Distinct from the lossy cross-run pkg/eventbus.
	events           *runEvents
	artifactVersions map[string]int
	nodeSessions     map[string]store.NodeSessionSlot
	pauseSessionRef  string // in-flight CLI ask_user pack (ADR-089)
	// lastGraceNode/lastGraceDim dedupe the budget_exit_grace event: the
	// pre-exec check and the post-resource-wait duration gate can route
	// the SAME overrun through graceOrFailBudget at one node boundary,
	// and the audit trail must record one deliberate grace per
	// (node, dimension), not one per checkpoint that happened to look.
	lastGraceNode string
	lastGraceDim  string
	// gateAnchors memoises the review-gate anchor of each (node, iter), so
	// the companion and the human are handed the SAME range: the companion
	// runs before the pause, and re-capturing at pause time would move the
	// head under it.
	gateAnchors map[string]int
	// lastWorkspaceSnapshot is the workspacetrack snapshot id capturing
	// the workspace as the previous node boundary left it. Same role as
	// lastSnapshotCommit, for the tracker-backed path.
	lastWorkspaceSnapshot string
	// lastSnapshotCommit is the commit capturing the workspace as the
	// previous node boundary left it, or "" meaning UNKNOWN (run start,
	// resume, or just out of a special dispatch that may have mutated the
	// tree). When it is known, the next node's pre-boundary marker aliases
	// it, so recording "state before node N" costs one update-ref instead
	// of a second index walk. Written and read only on the main execution
	// path (fan-out branches never snapshot — workspace safety admits a
	// single mutating branch), so it needs no lock.
	lastSnapshotCommit string
	// preMarked deduplicates markPreNodeBoundary per (node, loopIter):
	// several dispatch paths bracket the same node — execLoop brackets
	// every isSpecialDispatch kind, and execSpecialNode / the human-node
	// path bracket it again. A second call is pure duplicated work (the
	// same tree, hence the same ref target), but when lastSnapshotCommit
	// is UNKNOWN each one runs a full `git add -A` index walk and leaves
	// an orphan commit object. Same-key entries are dropped; a later loop
	// iteration carries a different key and re-marks.
	preMarked map[string]bool
	// stopCaptured deduplicates captureStopBoundary per (phase, node,
	// loopIter), for the same reason preMarked exists one field up: a
	// single failure travels through several checkpoint-aware handlers
	// (a node error surfaces at execLoopAfterExec, is returned, and the
	// caller hands the same *RuntimeError to failRunErrWithCheckpoint
	// again), and each pass would otherwise re-walk the workspace on the
	// failure path — the one place a run is least able to afford it.
	// Same tree, hence the same snapshot; only the walk is duplicated.
	stopCaptured map[string]bool
	// resumed marks a runState rebuilt by a RESUME, and is cleared by the
	// first workspace boundary that consumes it. That boundary is the one
	// closing the interval in which the run was stopped — the only
	// interval a scoped rewind can prove is not the run's own work.
	resumed bool
	budget  *SharedBudget // shared across branches, nil if no budget

	// resourceSemaphores holds one buffered channel per declared workflow
	// resource, pre-seeded with its tokens and shared by reference across all
	// branches so contention is global. A node that declares `needs: <resource>`
	// pops a token before running and pushes it back after (even on failure) —
	// bounding e.g. concurrent Godot sessions WITHOUT a global parallelism cap.
	// Token values: the empty string for the counting form (`godot: 5` → 5
	// anonymous tokens), or the member ids for the lease form (`godot: [s1,s2]`
	// → each acquire leases one distinct id, surfaced to the node as `_lease`).
	// nil when no resources declared.
	resourceSemaphores map[string]chan string

	// costUSDTotal is the run's cumulative LLM spend (sum of per-node
	// _cost_usd), tracked independently of `budget` so the daily spend
	// cap works even when the workflow declares no per-run budget. Read
	// and incremented on the post-exec path (recordAndCheckBudget);
	// recorded into the shared daily ledger via Engine.dailyCap.
	costUSDTotal float64

	// branchLedgerSeq hands each execBranch invocation a unique suffix for
	// its daily-cap ledger key. Without it, a fan-out INSIDE a loop reuses
	// the same "<runID>#<branchID>" key every iteration (branchID encodes
	// router+index, not the iteration), and the ledger's monotonic-max would
	// keep only the single costliest iteration instead of summing them.
	// Atomic: incremented concurrently from parallel branch goroutines.
	branchLedgerSeq atomic.Uint64

	// nodeAttempts counts prior failed attempts per (nodeID, ErrorCode)
	// so the recovery dispatcher can apply per-class retry budgets and
	// reset them after a successful execution. Keys are created lazily.
	nodeAttempts map[string]map[ErrorCode]int

	// attachments holds the resolved per-attachment view exposed to
	// templates as {{attachments.X[.path|.url|.mime|.size|.sha256]}}.
	// Populated once at run start from Run.Attachments and the
	// store's PresignAttachment helper.
	attachments map[string]model.AttachmentInfo

	// resumeBackend carries the persisted backend rehydration payload
	// from the checkpoint at resume time, injected into the input map
	// of the FIRST execution of cp.NodeID so the backend picks up
	// where the parent left off. Cleared after the first injection so
	// downstream nodes do not see it.
	resumeBackend resumeBackendState

	// isWorktree mirrors Run.Worktree, set once at run start by
	// setupWorktree / on resume restoration. Read by the per-node
	// snapshot hook to avoid one LoadRun per node finish.
	isWorktree bool
}

// resumeBackendState bundles the three fields the engine uses to
// rehydrate a backend at the resume entry node: the persisted claw
// conversation, the claude_code session id, and the anchor node these
// payloads target. Bundling them keeps callers from partially
// updating the group.
type resumeBackendState struct {
	nodeID string
	// conversation is raw JSON; typed json.RawMessage so the value
	// injected under delegate.ResumeConversationKey satisfies the
	// consumer's json.RawMessage assertion in applyResumeContinuity
	// (a plain []byte silently failed it and dropped the rehydration).
	conversation json.RawMessage
	sessionID    string
}

// markFailedBestEffort transitions the run to status=failed with a
// formatted message describing the origin error. Used on pre-execLoop
// setup failures where the engine is about to return the same error to
// the caller — a store-side failure of the status flip itself can't be
// propagated, so it's retried then logged instead of being silently
// dropped (without this, an op error during startup leaves the run
// stuck in `running` in the UI).
//
// The retry exists because this write shares its failure domain with
// whatever just failed (the store): a transient blip (Mongo hiccup, a
// ctx already torn down) that broke setup often clears within seconds,
// and a lost flip costs an operator round trip — or waits for the
// periodic orphan reconcile. Attempts run on a detached ctx: the run
// ctx being dead is one of the very triggers that lands here.
// The status it writes is not always `failed`. Setup runs on the same
// ctx the node loop does, so the same two interruptions reach it — and
// they mean the same thing here as there: a runner drained mid-rollout,
// or an operator cancelling, has not produced a workflow failure. Marked
// terminal, such a run cannot be resumed, and for a review that means the
// merge gate it owed stays absent for good. The node loop already
// classifies both (handleContextDoneWithCheckpoint); this is that
// classification, minus the checkpoint, which no node has yet earned —
// resume then restarts from the entry, which it is built to do.
func (e *Engine) markFailedBestEffort(ctx context.Context, runID, phase string, cause error) {
	writeCtx := context.WithoutCancel(ctx)
	status, msg := setupFailureStatus(ctx, phase, cause)
	var err error
	for attempt, delay := 0, 500*time.Millisecond; attempt < 3; attempt, delay = attempt+1, delay*4 {
		if attempt > 0 {
			time.Sleep(delay)
		}
		if err = e.store.UpdateRunStatus(writeCtx, runID, status, msg); err == nil {
			return
		}
	}
	if e.logger != nil {
		e.logger.Warn("runtime: failed to record run %s as %s during %s after 3 attempts: %v (original cause: %v — the run stays running until the orphan reconcile catches it)", runID, status, phase, err, cause)
	}
}

// setupFailureStatus classifies a pre-execLoop failure, mirroring what
// handleContextDoneWithCheckpoint decides inside a node.
//
//   - the run ctx cancelled with ErrRunInterrupted (runner drain, lost
//     heartbeat) — infrastructure took the run away: failed_resumable, so
//     the ordinary retry puts it on a healthy pod;
//   - the run ctx cancelled by an operator: cancelled, which is resumable
//     too and says who stopped it;
//   - anything else — a real setup error: failed, as before.
//
// It reads the CTX, not just the error: a drain kills the work by
// cancelling, so what surfaces is whatever the interrupted step returned
// (a killed `kubectl exec`, a half-written worktree), never the cause.
func setupFailureStatus(ctx context.Context, phase string, cause error) (store.RunStatus, string) {
	if ctx == nil || ctx.Err() == nil {
		return store.RunStatusFailed, fmt.Sprintf("%s: %v", phase, cause)
	}
	if errors.Is(context.Cause(ctx), ErrRunInterrupted) {
		return store.RunStatusFailedResumable, fmt.Sprintf("%s interrupted before the first node (resumable): %v", phase, cause)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return store.RunStatusCancelled, fmt.Sprintf("%s cancelled before the first node: %v", phase, cause)
	}
	return store.RunStatusFailed, fmt.Sprintf("%s: %v", phase, cause)
}

// setupErr decorates the error a setup phase returns so an interruption
// stays recognisable to the runner: it NAKs on ErrRunInterrupted (the run
// is redelivered to a healthy pod) and ACKs on anything else. Without it
// the status written above would say resumable while the queue had
// already dropped the run — the two halves have to agree.
func (e *Engine) setupErr(ctx context.Context, err error) error {
	if ctx != nil && ctx.Err() != nil && errors.Is(context.Cause(ctx), ErrRunInterrupted) {
		return fmt.Errorf("%w: %v", ErrRunInterrupted, err)
	}
	return err
}

// newRunState builds a runState with all maps allocated. Resume paths
// then overwrite specific fields (outputs, loop counters, vars, etc.)
// from the persisted checkpoint.
func (e *Engine) newRunState(runID string, inputs map[string]any) *runState {
	rs := &runState{
		runID:              runID,
		runInputs:          inputs,
		outputs:            make(map[string]map[string]any),
		artifacts:          make(map[string]map[string]any),
		loopCounters:       make(map[string]int),
		loopPreviousOutput: make(map[string]map[string]any),
		loopCurrentOutput:  make(map[string]map[string]any),
		loopProgressSig:    make(map[string]string),
		loopStaleness:      make(map[string]int),
		loopBudgetMarks:    make(map[string]loopBudgetMark),
		roundRobinCounters: make(map[string]int),
		selectedIncoming:   make(map[string][]store.IncomingEdge),
		artifactVersions:   make(map[string]int),
		nodeSessions:       make(map[string]store.NodeSessionSlot),
		preMarked:          make(map[string]bool),
		nodeAttempts:       make(map[string]map[ErrorCode]int),
		budget:             newSharedBudget(e.workflow.Budget, e.logger),
		resourceSemaphores: buildResourceSemaphores(e.workflow.Resources, e.workflow.ResourceMembers),
		events:             newRunEvents(),
	}
	// Publish the run's budget so the active-duration stamping callback
	// (runview Service / runner) can read its monotonic elapsed. nil when
	// the workflow declares no budget — ActiveElapsed then returns 0.
	e.activeBudget.Store(rs.budget)
	return rs
}

// loopBoundsPayload builds the run_started event payload carrying each
// named loop's iteration bound (MaxIterations), so the runview snapshot
// can render a run-level loop indicator (current/max). Returns nil when
// the workflow has no declared loops (payload stays absent). Literal
// caps only — expression / unbounded caps report 0 (max unknown), which
// the studio renders as a bare current count.
func loopBoundsPayload(wf *ir.Workflow) map[string]any {
	if wf == nil || len(wf.Loops) == 0 {
		return nil
	}
	bounds := make(map[string]any, len(wf.Loops))
	for name, loop := range wf.Loops {
		if loop == nil {
			continue
		}
		bounds[name] = loop.MaxIterations
	}
	if len(bounds) == 0 {
		return nil
	}
	return map[string]any{"loops": bounds}
}

// leaseInputKey is the node-input key under which a node's acquired
// instance leases are surfaced (resource name → leased member id), e.g.
// input["_lease"]["godot"] == "godot-s3". Only present for lease-form
// resources; consumed by bots to bind the leased instance (cwd/MCP).
const leaseInputKey = "_lease"

// buildResourceSemaphores creates one buffered channel per declared resource,
// PRE-SEEDED with its tokens: the buffer holds the available slots, so a
// receive acquires (blocks when empty) and a send returns the token. For the
// counting form the tokens are empty strings (capacity anonymous slots); for
// the lease form they are the distinct member ids, so an acquire pops a
// specific instance to lease. Returns nil when the workflow declares no
// resources, so acquireResources is a no-op.
func buildResourceSemaphores(resources map[string]int, members map[string][]string) map[string]chan string {
	if len(resources) == 0 {
		return nil
	}
	sem := make(map[string]chan string, len(resources))
	for name, capacity := range resources {
		if capacity <= 0 {
			continue // defensive; validate rejects ≤ 0
		}
		ch := make(chan string, capacity)
		if pool := members[name]; len(pool) > 0 {
			// Lease form: seed with the distinct instance ids.
			for _, id := range pool {
				ch <- id
			}
		} else {
			// Counting form: seed with `capacity` anonymous tokens.
			for i := 0; i < capacity; i++ {
				ch <- ""
			}
		}
		sem[name] = ch
	}
	return sem
}

// acquireResources blocks until a token of every resource in `needs` is free,
// then returns a release func that frees them and the leases it took. Resources
// are acquired in a stable (sorted, de-duplicated) order so two nodes that need
// the same pair can never deadlock via inconsistent lock ordering. A cancelled
// ctx aborts the wait, releasing any tokens already taken and returning
// ctx.Err(). The caller must `defer release()` so the tokens free even when the
// node fails. `leases` maps a resource name to the distinct instance id it
// acquired, but ONLY for lease-form resources (counting-form tokens are empty
// and omitted); nil when nothing was leased.
func (e *Engine) acquireResources(ctx context.Context, rs *runState, needs []string) (func(), map[string]string, error) {
	if len(needs) == 0 || rs.resourceSemaphores == nil {
		return func() {}, nil, nil
	}
	// De-duplicate + sort for a consistent global acquisition order.
	seen := make(map[string]bool, len(needs))
	ordered := make([]string, 0, len(needs))
	for _, r := range needs {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		ordered = append(ordered, r)
	}
	sort.Strings(ordered)

	type held struct{ name, tok string }
	acquired := make([]held, 0, len(ordered))
	release := func() {
		for _, h := range acquired {
			rs.resourceSemaphores[h.name] <- h.tok // return the exact token/id
		}
	}
	var leases map[string]string
	for _, r := range ordered {
		ch := rs.resourceSemaphores[r]
		if ch == nil {
			continue // undeclared resource (validate flags it); skip defensively
		}
		select {
		case tok := <-ch:
			acquired = append(acquired, held{r, tok})
			if tok != "" { // lease form → record the leased instance id
				if leases == nil {
					leases = make(map[string]string, len(ordered))
				}
				leases[r] = tok
			}
		case <-ctx.Done():
			release()
			return nil, nil, ctx.Err()
		}
	}
	return release, leases, nil
}
