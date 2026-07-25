package runtime

import (
	"context"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// WithSubbotRunner wires the closure invoked by `subbot` nodes.
func WithSubbotRunner(r SubbotRunner) EngineOption {
	return func(e *Engine) { e.subbotRunner = r }
}

// WithSandboxRunObserver registers a callback invoked with the live
// sandbox [sandbox.Run] immediately after it starts. The cloud runner
// uses it to drive mid-run file-secret refresh against a driver that
// implements [sandbox.SecretFileRefresher] (a long sandboxed run must
// not push/comment with a token that expired since launch). The callback
// must not block — spawn any long-lived work on a goroutine keyed to the
// run's context. nil (the default) disables the hook.
func WithSandboxRunObserver(fn func(sandbox.Run)) EngineOption {
	return func(e *Engine) { e.sandboxRunObserver = fn }
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

// WithBoardMCP wires the board MCP HTTP handler used to serve a per-run
// gateway-reachable board listener when a sandbox is active, so sandboxed
// board-capability nodes (claude_code) can write the operator's board
// (C082). The handler must serve ONLY the board MCP routes (it is exposed
// gateway-reachable, token-gated) — never the full server mux. Nil (CLI
// runs with no server) leaves sandboxed board-emit disabled.
func WithBoardMCP(h http.Handler) EngineOption {
	return func(e *Engine) { e.boardMCPHandler = h }
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
// with the run ID, the node's ID, and the raw (unsanitized) output map.
// The callback must be safe for concurrent use (parallel branches finish
// concurrently). The runID lets a single registered callback serve every
// run the engine drives, so convention-specific logic (e.g. stamping
// Run.WatchedIssueIDs from a dispatch node's `dispatched_ids` output)
// can live in the wiring layer instead of the generic engine.
func WithOnNodeFinished(fn func(runID, nodeID string, output map[string]any)) EngineOption {
	return func(e *Engine) { e.onNodeFinished = fn }
}

// WithEventObserver registers a callback invoked after every successful
// event append (including branch_started/finished). It must be safe for
// concurrent use; the callback runs in the goroutine that emitted the
// event. Use it to fan out events to non-store observers (Prometheus,
// custom metrics) without changing the persistence layer.
//
// Observers CHAIN: multiple WithEventObserver options all fire (in
// registration order), so registering e.g. both the studio WS broker and
// the run-health alert manager delivers events to both. (Previously the
// last registration silently overwrote the earlier ones — enabling
// alerting would have killed the live console feed.)
func WithEventObserver(fn func(evt store.Event)) EngineOption {
	return func(e *Engine) {
		if fn == nil {
			return
		}
		prev := e.onEvent
		if prev == nil {
			e.onEvent = fn
			return
		}
		e.onEvent = func(evt store.Event) {
			prev(evt)
			fn(evt)
		}
	}
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

// WithWorkflowHash sets a hash of the .bot source so that Resume can
// detect if the workflow changed since the run was started.
func WithWorkflowHash(hash string) EngineOption {
	return func(e *Engine) { e.workflowHash = hash }
}

// WithFilePath records the absolute .bot source path on the run
// metadata so that resume (and the run console) can re-locate the
// workflow without the caller having to thread it back through the
// API. Optional — empty string is ignored.
func WithFilePath(path string) EngineOption {
	return func(e *Engine) { e.filePath = path }
}

// WithParentRunID records the immediate parent of a nested run. Empty values
// are ignored so pickup/resume paths that do not re-supply the option preserve
// any lineage already stored on the run document.
func WithParentRunID(parentRunID string) EngineOption {
	return func(e *Engine) { e.parentRunID = parentRunID }
}

// WithParentNodeID records the IR node id of the subbot node in the parent
// workflow that spawned this child run. Empty values are ignored, mirroring
// WithParentRunID.
func WithParentNodeID(parentNodeID string) EngineOption {
	return func(e *Engine) { e.parentNodeID = parentNodeID }
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

// WithSource records the originating action that produced this run.
// The dispatcher passes a non-nil *store.RunSource carrying the
// issue back-reference so the studio's RunHeader can link back to
// the kanban. nil for CLI / studio / fork-spawned runs.
func WithSource(src *store.RunSource) EngineOption {
	return func(e *Engine) { e.source = src }
}

// WithCallback records the run-completion webhook parameters on the run
// metadata at creation. url is the http/https endpoint the completion
// notifier POSTs to when the run reaches a terminal state; token is an
// opaque value echoed back to the receiver for correlation; answerNode
// optionally names the node whose latest artifact holds the run's
// user-facing answer (read from the "final_answer" field). All optional
// — an empty url leaves the run without a callback (the common case).
func WithCallback(url, token, answerNode string) EngineOption {
	return func(e *Engine) {
		e.callbackURL = url
		e.callbackToken = token
		e.callbackAnswerNode = answerNode
	}
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

// WithModelOverrides records the launch-time per-node/-group model/
// backend pins so the engine persists them (display-only) on the run
// record. The overrides are applied to the executor separately at
// launch; this option exists solely so the studio Overview can show
// what a run was launched with. Empty on resume (not re-supplied), so
// the persisted value is left untouched — see engine_run.go.
func WithModelOverrides(o []store.RunModelOverride) EngineOption {
	return func(e *Engine) { e.modelOverrides = o }
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
//
// Passing the workspace explicitly also marks it as DELEGATED to the
// engine (dispatcher-seeded per-issue worktrees, studio-bound dirs):
// runPersistWorkspace may then adopt a linked-worktree workspace as a
// managed-worktree baseline (Worktree=true → finalization authority).
// A defaulted CWD stays the operator's own place — the engine never
// claims lifecycle authority over it (a `worktree: none` run launched
// from inside a foreign linked worktree, e.g. a Claude Code session
// worktree, must not get branched/FF'd/cleaned on close).
func WithWorkDir(dir string) EngineOption {
	return func(e *Engine) {
		e.workDir = dir
		e.workDirDelegated = dir != ""
	}
}

// WithWorktreeAuthoritySince supplies the trusted lower bound used by managed
// worktree cleanup's process census. Launchers that construct or health-check
// run-scoped subprocesses before Engine.Run must capture this timestamp before
// doing so; otherwise an inaccessible early child could look unrelated merely
// because it predates `git worktree add`. The timestamp is persisted outside
// the checkout. Callers that do not start anything early may omit this option;
// setupWorktree then captures the boundary immediately before creation.
func WithWorktreeAuthoritySince(since time.Time) EngineOption {
	return func(e *Engine) { e.worktreeAuthoritySince = since.UTC() }
}

// WithBundle attaches a resolved `.botz` bundle to the engine. The
// runtime mirrors the bundle's `skills/` directory into the workDir's
// `.claude/skills/` so claude_code and the claw skill registry pick
// them up transparently. Pass nil (the default) for plain .bot
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

// WithPauseSignal wires an external pause request channel into the
// engine. When a caller closes the channel (or sends a non-blocking
// send-and-don't-care signal), the engine pauses at the next safe
// boundary (top of execLoop, between LLM turns inside an agent, etc.)
// and returns ErrRunPausedOperator after saving a checkpoint.
//
// This is the engine-side hook the studio's "Pause now" button + the
// POST /api/runs/{id}/pause endpoint depend on (Phase 1). Distinct
// from ctx cancellation: a paused run is resumable like a
// failed_resumable run; a cancelled run is terminal.
//
// Pass nil (the default) to opt out — engine pause is then disabled
// and the only way to interrupt a run is ctx cancellation (i.e.
// Cancel).
func WithPauseSignal(ch <-chan struct{}) EngineOption {
	return func(e *Engine) { e.pauseSignal = ch }
}

// WithDailyCap wires a per-(store, UTC-day) LLM spend cap into the
// engine. Before each node executes, the engine checks the shared daily
// ledger; if the cap is over (and not overridden for the day) it pauses
// the run (resumable, status paused_operator) with a run_paused event
// tagged reason=cost_cap_daily. After each node it records the run's
// cumulative cost back into the ledger. Because the ledger is shared
// across all runs in a store, one run tripping the cap causes every
// other running run to pause at its next node boundary, and the
// dispatcher to stop launching new work.
//
// Pass nil (the default, or a disabled guard from NewDailyCapGuard) to
// opt out — the cap is then inert.
func WithDailyCap(g *DailyCapGuard) EngineOption {
	return func(e *Engine) { e.dailyCap = g }
}
