package store

import (
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// ErrRunNotFound is the sentinel every RunStore.LoadRun implementation
// wraps when the requested run genuinely does not exist — as opposed to
// a transient store failure (outage, decode error, context deadline).
// Callers that must distinguish "the run is gone" from "the store is
// momentarily unreachable" test with errors.Is(err, ErrRunNotFound); a
// bare (unwrapped) error is treated as unknown/transient and must NOT be
// read as absence. The k8s sandbox reaper relies on this: it may only
// force-delete a run's sandbox + credential Secret when the run is
// provably gone, never on a store blip. The filesystem store additionally
// wraps os.ErrNotExist, so errors.Is(err, os.ErrNotExist) also holds there.
var ErrRunNotFound = errors.New("store: run not found")

// ErrRunDeleted marks a run that was DELIBERATELY deleted (DeleteRun
// left a durable tombstone). Distinct from ErrRunNotFound so late
// writers — a detached engine goroutine, a stale runner, a replayed
// message — get a hard, typed refusal instead of silently resurrecting
// the run by re-creating its directory / upserting its document. The
// HTTP layer maps it to 410 Gone.
var ErrRunDeleted = errors.New("store: run was deleted")

// ---------------------------------------------------------------------------
// RunStatus — lifecycle state of a run
// ---------------------------------------------------------------------------

// RunStatus represents the current state of a run.
type RunStatus string

const (
	RunStatusRunning            RunStatus = "running"
	RunStatusPausedWaitingHuman RunStatus = "paused_waiting_human"
	// RunStatusPausedOperator is set when an operator requested a soft
	// pause via the studio Pause button or POST /api/runs/{id}/pause.
	// Distinct from paused_waiting_human (no pending Interaction record),
	// resumes via the same checkpoint machinery as cancelled runs.
	RunStatusPausedOperator  RunStatus = "paused_operator"
	RunStatusFinished        RunStatus = "finished"
	RunStatusFailed          RunStatus = "failed"
	RunStatusFailedResumable RunStatus = "failed_resumable"
	RunStatusCancelled       RunStatus = "cancelled"
	// RunStatusQueued is set on cloud-mode runs that have been submitted
	// to the NATS queue but not yet picked up by a runner pod. Local
	// runs never observe this state — they transition straight to
	// running on Engine.Run(). See cloud-ready plan §A, §F (T-03).
	RunStatusQueued RunStatus = "queued"
)

// IsTerminal returns true when the run has reached a state it cannot
// leave without operator action. Polling consumers (cloud-mode shard
// dispatchers, the studio's run list, etc.) stop refreshing once
// IsTerminal is true. `failed_resumable` is terminal in this sense
// even though the run can be resumed — the resume produces a new
// observable status transition.
func (s RunStatus) IsTerminal() bool {
	switch s {
	case RunStatusFinished, RunStatusFailed, RunStatusFailedResumable, RunStatusCancelled:
		return true
	default:
		return false
	}
}

// IsPaused returns true for the dormant-but-resumable statuses —
// parked on a human gate or an operator pause. The run is neither
// running nor terminal; a resume continues it in place.
func (s RunStatus) IsPaused() bool {
	return s == RunStatusPausedWaitingHuman || s == RunStatusPausedOperator
}

// ---------------------------------------------------------------------------
// Run — top-level run metadata persisted in run.json
// ---------------------------------------------------------------------------

// RunFormatVersion is the current version of the persisted run.json format.
// Bump this when making breaking changes to the Run struct.
const RunFormatVersion = 1

// Run is the top-level metadata for a single workflow invocation.
//
// bson tags mirror the json tags exactly (same snake_case names) so a
// future Mongo backend can encode/decode against the same struct
// without a parallel BSON struct. The cloud-only fields documented in
// plan §D.1 (queued_at, started_at, runner_id, repo_url, repo_sha,
// queue_msg_id, version) live on Run when they appear; pure
// filesystem callers leave them zero and they round-trip as `null`
// in BSON without breaking the JSON shape.
// RunModelOverride is one persisted launch-time model/backend override
// rule: the node selector (id / glob / kind / "*") plus whatever it
// pins. Mirror of the launch request's override entries, kept on the
// run so the studio can show "launched with model X on backend Y"
// after the fact — the overrides otherwise ride only the transient
// launch request, not the persisted record.
type RunModelOverride struct {
	Selector string `json:"selector" bson:"selector"`
	Backend  string `json:"backend,omitempty" bson:"backend,omitempty"`
	Model    string `json:"model,omitempty" bson:"model,omitempty"`
	Provider string `json:"provider,omitempty" bson:"provider,omitempty"`
	Effort   string `json:"effort,omitempty" bson:"effort,omitempty"`
}

// NodeServed is the (backend, model) that actually served one LLM node.
// Last write wins per node_id — a loop's last pass is what a finished
// run.json reports; the event stream is the full history.
//
// Model is the provider-reported effective model, distinct from
// DeclaredModel (the workflow `model:` / launch override the node
// asked for). They diverge on a fallback chain, an env override
// (ANTHROPIC_MODEL), or a third-party proxy. Empty Model means the
// backend did not report one — DeclaredModel is then the only hint,
// not a substitute for what ran.
type NodeServed struct {
	Backend         string `json:"backend" bson:"backend"`
	Model           string `json:"model,omitempty" bson:"model,omitempty"`
	DeclaredModel   string `json:"declared_model,omitempty" bson:"declared_model,omitempty"`
	ContextWindow   int    `json:"context_window,omitempty" bson:"context_window,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty" bson:"max_output_tokens,omitempty"`
}

// RunBudget is the EFFECTIVE budget cap set captured at launch — the
// workflow's `budget:` block after recipe/preset/CLI overrides AND (in
// cloud) the platform ceiling clamp, snapshotted onto the run so the
// studio Overview can draw budget meters with a stable denominator
// instead of re-deriving caps from budget_warning events. A zero field
// means "no cap on this dimension" (per the runtime's SharedBudget
// convention); nil (the whole pointer) means the workflow declared no
// budget and no overrides were applied. Display-only, like
// RunModelOverride — the live enforcement reads the runtime's
// SharedBudget, never this record. MaxDuration is kept as a Go duration
// string ("30m") — the source of truth — so the frontend parses it and
// no lossy ms conversion happens at the wire boundary.
type RunBudget struct {
	MaxCostUSD          float64 `json:"max_cost_usd,omitempty" bson:"max_cost_usd,omitempty"`
	MaxTokens           int     `json:"max_tokens,omitempty" bson:"max_tokens,omitempty"`
	WarnTokens          int     `json:"warn_tokens,omitempty" bson:"warn_tokens,omitempty"`
	MaxIterations       int     `json:"max_iterations,omitempty" bson:"max_iterations,omitempty"`
	MaxDuration         string  `json:"max_duration,omitempty" bson:"max_duration,omitempty"`
	MaxParallelBranches int     `json:"max_parallel_branches,omitempty" bson:"max_parallel_branches,omitempty"`
}

// RunBudgetRaises is the live-steering (raise_budget) counterpart of
// RunBudget: the ABSOLUTE caps granted mid-run, persisted so resume
// re-applies them (raise-only) over the workflow's declared budget.
// Zero fields were never raised.
type RunBudgetRaises struct {
	MaxCostUSD    float64 `json:"max_cost_usd,omitempty" bson:"max_cost_usd,omitempty"`
	MaxTokens     int     `json:"max_tokens,omitempty" bson:"max_tokens,omitempty"`
	MaxIterations int     `json:"max_iterations,omitempty" bson:"max_iterations,omitempty"`
	MaxDuration   string  `json:"max_duration,omitempty" bson:"max_duration,omitempty"`
}

// RunRetryPolicy is the launch-time snapshot of the effective retry
// contract (pkg/retrypolicy), plus where each field came from. The
// provenance map is not decoration: a four-layer chain is undebuggable
// without it, and "why is this run waiting 8 days" must be answerable
// without replaying the launch.
type RunRetryPolicy struct {
	UsageWindow string `json:"usage_window,omitempty" bson:"usage_window,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty" bson:"max_attempts,omitempty"`
	MaxWait     string `json:"max_wait,omitempty" bson:"max_wait,omitempty"`
	Jitter      string `json:"jitter,omitempty" bson:"jitter,omitempty"`
	// Sources maps each field name to the layer that won it
	// ("run_override", "schedule", "bot", "env", "default",
	// "platform_ceiling").
	Sources map[string]string `json:"sources,omitempty" bson:"sources,omitempty"`
}

// RunRetryState is the live retry bookkeeping for a run whose failure is
// waiting on a provider quota window to reopen.
//
// Attempts is MONOTONIC over the run's whole lifetime and is never reset:
// it is the anti-loop bound, so a run that trips the window on three
// separate resumes has spent three of its attempts. RetryAfter is unset
// (nil) once a sweeper claims the retry, which is what makes the claim a
// compare-and-swap: two replicas cannot both win it.
type RunRetryState struct {
	// RetryAfter is when the run becomes eligible for an automatic
	// resume. Nil = nothing armed (never armed, already claimed, or
	// deliberately abandoned).
	RetryAfter *time.Time `json:"retry_after,omitempty" bson:"retry_after,omitempty"`
	// Reason names the failure class that armed this retry
	// ("usage_window").
	Reason string `json:"reason,omitempty" bson:"reason,omitempty"`
	// Code is the RuntimeError code that armed it
	// ("USAGE_LIMIT_BLOCKED").
	Code string `json:"code,omitempty" bson:"code,omitempty"`
	// Attempts counts retries armed for this run, ever.
	Attempts int `json:"attempts,omitempty" bson:"attempts,omitempty"`
	// LastError records why the most recent automatic resume did not
	// happen (admission denied, unresolvable source, a Resume error), so
	// a run that stops retrying says why instead of going quiet.
	LastError string `json:"last_error,omitempty" bson:"last_error,omitempty"`
	// ClaimedAt stamps when a sweeper last took the retry.
	ClaimedAt *time.Time `json:"claimed_at,omitempty" bson:"claimed_at,omitempty"`
}

type Run struct {
	FormatVersion int    `json:"format_version" bson:"format_version"`
	ID            string `json:"id" bson:"_id"`
	// Name is a deterministic, human-friendly label derived from
	// (file_path + run_id) at run creation. Display-only — the
	// canonical identifier remains ID. Empty for runs persisted
	// before this field existed; surfaces should fall back to
	// WorkflowName in that case.
	Name string `json:"name,omitempty" bson:"name,omitempty"`
	// ParentRunID, ShardIndex, ShardCount, ShardLabel are set on child
	// runs spawned by a parent workflow via `iterion __scan-shards`
	// (see docs/security-bots-distributed.md). Empty / zero for root
	// runs. The studio surfaces parent/child relationships via these
	// fields; aggregation is by polling children's terminal status.
	ParentRunID string `json:"parent_run_id,omitempty" bson:"parent_run_id,omitempty"`
	// ParentNodeID is the IR node id of the subbot node in the parent
	// workflow that spawned this child run; empty for root runs and
	// non-subbot children.
	ParentNodeID string `json:"parent_node_id,omitempty" bson:"parent_node_id,omitempty"`
	ShardIndex   int    `json:"shard_index,omitempty" bson:"shard_index,omitempty"`
	ShardCount   int    `json:"shard_count,omitempty" bson:"shard_count,omitempty"`
	ShardLabel   string `json:"shard_label,omitempty" bson:"shard_label,omitempty"`
	// SubbotChildren maps a subbot-node execution key (node id +
	// loop-iteration path + fan-out branch id) to the child run id that
	// execution spawned. Written when the child is launched and cleared
	// when its terminal output is consumed, so a PARENT resumed after a
	// process restart RE-ATTACHES to the in-flight child (paused on its
	// human gate, or since finished) instead of spawning a fresh one and
	// losing the answered child's work. Empty for parents with no subbot
	// nodes and for runs that predate this field. Set only on the parent.
	SubbotChildren map[string]string `json:"subbot_children,omitempty" bson:"subbot_children,omitempty"`
	WorkflowName   string            `json:"workflow_name" bson:"workflow_name"`
	WorkflowHash   string            `json:"workflow_hash,omitempty" bson:"workflow_hash,omitempty"` // SHA-256 of the .bot source at run start
	FilePath       string            `json:"file_path,omitempty" bson:"file_path,omitempty"`         // absolute .bot source path captured at launch (resume without re-supplying file)
	// WorkflowSource is the .bot text as it was AT LAUNCH. WorkflowHash
	// answers "did the source change since?"; this answers "which node
	// changed", which is what `iterion rewind --auto` needs to target the
	// edit. FilePath cannot serve: by the time you rewind, that file holds
	// the NEW version — the whole reason you are rewinding.
	//
	// Best-effort and size-capped (see runtime.maxPersistedWorkflowSource):
	// an unreadable or oversized source simply disables auto-targeting,
	// leaving `--node` to work as before.
	WorkflowSource string `json:"workflow_source,omitempty" bson:"workflow_source,omitempty"`
	// Preset is the in-source preset name selected at launch via
	// `--preset <name>` (or the studio Launch modal). Persisted so
	// `iterion resume` re-applies the same parameter set without the
	// caller having to re-supply it. Empty when no preset was selected
	// or the workflow declares none.
	Preset string `json:"preset,omitempty" bson:"preset,omitempty"`
	// ExtraSkills are skill-library skills the OPERATOR added to this run
	// (`--skill <name>`, repeatable, or the ITERION_SKILLS machine default),
	// on top of whatever the workflow declares. ADDITIVE by construction: a
	// run-level list never removes a skill the bot's author declared.
	//
	// Persisted for the same reason as Preset, and it matters more here: a
	// conversational bot resumes on every turn, so a launch-only list would
	// silently vanish after the first reply. `iterion resume` re-applies
	// this without the caller re-typing it.
	ExtraSkills []string `json:"extra_skills,omitempty" bson:"extra_skills,omitempty"`
	// PermissionMode is the workflow-declared tool-permission gate mode
	// ("" | "off" | "ask" | "deny") captured at launch, surfaced in the
	// studio RunHeader so a gated run reads at a glance. See
	// docs/permissions.md.
	PermissionMode string `json:"permission_mode,omitempty" bson:"permission_mode,omitempty"`
	// ModelOverrides captures launch-time per-node/-group backend+model+
	// provider pins (studio dropdowns / CLI --model/--backend / HTTP
	// model_overrides) so the run's Overview can show what it was
	// launched with. Display-only: the overrides are applied to the
	// executor at launch, never re-read from here. Empty when none, and
	// left untouched on resume (resume doesn't re-supply them).
	ModelOverrides []RunModelOverride `json:"model_overrides,omitempty" bson:"model_overrides,omitempty"`
	// NodesServed maps IR node id → last (backend, model) that served
	// it. Display-only / post-hoc: makes a finished run self-describing
	// without replaying events.jsonl. Empty for legacy runs and for
	// nodes that never delegated (tool/compute/human). Written by
	// RecordNodeServed from the delegate_finished / delegate_error
	// hooks; last write wins per node.
	NodesServed map[string]NodeServed `json:"nodes_served,omitempty" bson:"nodes_served,omitempty"`
	// Budget is the effective budget cap set captured at launch (after
	// overrides + cloud ceiling clamp), so the studio Overview draws
	// budget meters with a denominator. Nil when the workflow declares
	// no budget: block and no overrides applied. See RunBudget.
	Budget *RunBudget `json:"budget,omitempty" bson:"budget,omitempty"`
	// LoopOverrides is the accumulated per-loop iteration grant applied
	// by live steering (bump_loop): loop name → extra iterations on top
	// of the loop's declared/expr-resolved max. AUTHORITATIVE for
	// enforcement across resume (the engine re-seeds from it); written
	// by the engine goroutine via PatchRunSteering. Empty for runs
	// never bumped.
	LoopOverrides map[string]int `json:"loop_overrides,omitempty" bson:"loop_overrides,omitempty"`
	// BudgetRaises captures the ABSOLUTE budget caps applied by live
	// steering (raise_budget) — not deltas, so a resume on another
	// machine re-applies the exact ceilings. AUTHORITATIVE for
	// enforcement across resume (re-applied before the budget is
	// rebuilt); raise-only vs the workflow's declared budget. Nil for
	// runs never raised.
	BudgetRaises *RunBudgetRaises `json:"budget_raises,omitempty" bson:"budget_raises,omitempty"`
	// PermissionGrants accumulates the `allow always` rules the operator
	// earned at permission-gate pauses, KEYED BY THE NODE that earned
	// them. Each pause is resumed by a fresh engine with a fresh
	// runState, so a grant can only outlive the pause it was answered on
	// if it outlives the run record too. AUTHORITATIVE across resume (the
	// engine re-seeds from it). Order-preserving and deduplicated per
	// node. Empty for runs that never hit an approved gate.
	//
	// Keyed per node, not run-wide, because Evaluate ranks allow rules
	// above the mode default: a run-wide rule would beat a node whose own
	// `permission: deny` made the gate a hard block, so an approval given
	// on a permissive node would silently unlock a strict one.
	PermissionGrants map[string][]string `json:"permission_grants,omitempty" bson:"permission_grants,omitempty"`
	// RetryPolicy is the retry contract RESOLVED AT LAUNCH from every
	// layer that can own one (per-run override → launch surface → bot
	// manifest → machine default, then clamped by the platform ceiling).
	// It is resolved server-side because only the launch site can see all
	// the layers, and snapshotted here because the run doc is the one
	// carrier every consumer already loads — the runner reads it from the
	// doc it fetches anyway, so no queue-schema bump is needed and it
	// never has to query schedules. Nil for runs launched before this
	// existed; treat nil as the package defaults.
	RetryPolicy *RunRetryPolicy `json:"retry_policy,omitempty" bson:"retry_policy,omitempty"`
	// RetryState is the live retry bookkeeping for this run (cloud only).
	// Nil until a retryable failure arms one. See RunRetryState.
	RetryState *RunRetryState `json:"retry_state,omitempty" bson:"retry_state,omitempty"`
	// DeletedAt is the Mongo-side durable tombstone (the filesystem
	// twin is the .deleted marker file): DeleteRun strips the run's
	// data and leaves a skeleton doc carrying this stamp, so a late
	// writer's upsert/update matches the tombstone and is refused
	// (ErrRunDeleted) instead of resurrecting the run. Reaped by
	// `iterion runs prune`.
	DeletedAt *time.Time `json:"deleted_at,omitempty" bson:"deleted_at,omitempty"`
	// BundleHash is the SHA-256 of the logical content (sorted
	// (relative-path, file-bytes) sequence) of the `.botz` archive
	// backing this run. Format-independent, so it is stable whether the
	// archive is a ZIP or a legacy tar.gz. Used by resume to re-locate
	// the same cache slot (and detect when the archive has changed
	// out from under the run). Empty when the run was launched from a
	// plain .bot file or a directory bundle (no archive).
	BundleHash string `json:"bundle_hash,omitempty" bson:"bundle_hash,omitempty"`
	// BundlePath is the absolute path of the source `.botz` archive
	// or directory bundle captured at launch. Used by resume to
	// re-extract the archive when the cache has been GC'd between
	// runs. Empty for plain .bot runs.
	BundlePath string `json:"bundle_path,omitempty" bson:"bundle_path,omitempty"`
	// BundleName is the bundle's `manifest.yaml` `name` field captured at
	// launch (e.g. "docs-refresh"). Display-only — surfaces it in run lists
	// so dispatcher-spawned bot runs are visually grouped. Empty for
	// plain .bot runs and for bundles whose manifest had no name;
	// consumers fall back to basename(BundlePath) stripped of `.botz`.
	BundleName string `json:"bundle_name,omitempty" bson:"bundle_name,omitempty"`
	// BundleDisplayName is the bundle's friendly persona name (e.g.
	// "Nexie"), captured from manifest.yaml's `display_name` at launch.
	// Empty when the bundle's manifest doesn't declare one. The studio
	// adds a ✨ icon next to the BotChip when this is set, so a run
	// belonging to a named persona reads at a glance.
	BundleDisplayName string         `json:"bundle_display_name,omitempty" bson:"bundle_display_name,omitempty"`
	Status            RunStatus      `json:"status" bson:"status"`
	Inputs            map[string]any `json:"inputs,omitempty" bson:"inputs,omitempty"`
	CreatedAt         time.Time      `json:"created_at" bson:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at" bson:"updated_at"`
	FinishedAt        *time.Time     `json:"finished_at,omitempty" bson:"finished_at,omitempty"`
	Error             string         `json:"error,omitempty" bson:"error,omitempty"`
	Checkpoint        *Checkpoint    `json:"checkpoint,omitempty" bson:"checkpoint,omitempty"`
	ArtifactIndex     map[string]int `json:"artifact_index,omitempty" bson:"artifact_index,omitempty"` // node_id → latest version written
	// WorkDir is the absolute filesystem path the run executes in
	// (the per-run git worktree when Worktree is true, otherwise the
	// engine's resolved cwd at start). Persisted so studio surfaces
	// (e.g. modified-files panel) can locate the run's working tree
	// without re-deriving it from the runtime.
	WorkDir string `json:"work_dir,omitempty" bson:"work_dir,omitempty"`
	// Worktree is true when WorkDir was created by `worktree: auto`,
	// false when WorkDir is the inherited cwd.
	Worktree bool `json:"worktree,omitempty" bson:"worktree,omitempty"`
	// RepoRoot is the absolute path of the main git repository the
	// worktree was forked from. Used by the studio's modified-files
	// panel after the worktree directory is gc'd to compute the diff
	// against FinalCommit (the persistent branch lives in this repo's
	// shared .git). Empty for non-worktree runs.
	RepoRoot string `json:"repo_root,omitempty" bson:"repo_root,omitempty"`
	// BaseCommit is the SHA of HEAD on the main repo at the moment the
	// worktree was created — i.e. the run's baseline. The post-finalization
	// diff renders FinalCommit relative to this commit. Empty for non-
	// worktree runs and for legacy runs that predate this field.
	BaseCommit string `json:"base_commit,omitempty" bson:"base_commit,omitempty"`
	// FinalCommit is the SHA the worktree's HEAD pointed to when the
	// run finished successfully, captured before the worktree was torn
	// down. Empty when the run made no commits, didn't use a worktree,
	// or didn't finish.
	FinalCommit string `json:"final_commit,omitempty" bson:"final_commit,omitempty"`
	// FinalBranch is the persistent branch name created on
	// FinalCommit (default "iterion/run/<friendly-name>", overridable
	// via launch params). Acts as a GC guard so the commits remain
	// reachable after the worktree directory is removed.
	FinalBranch string `json:"final_branch,omitempty" bson:"final_branch,omitempty"`
	// FinalBranchError is non-empty when FinalCommit is set but
	// finalizeWorktree could not create a persistent branch on it —
	// the commits exist (reachable via reflog) but the GC guard is
	// missing. The studio surfaces this so the operator can run
	// `git branch <name> <FinalCommit>` before the reflog expires.
	FinalBranchError string `json:"final_branch_error,omitempty" bson:"final_branch_error,omitempty"`
	// MergedInto is the branch the engine fast-forwarded to FinalCommit
	// after the run, or empty when the FF was skipped (dirty main,
	// non-FF, branch divergence, opt-out, or detached HEAD at start).
	MergedInto string `json:"merged_into,omitempty" bson:"merged_into,omitempty"`
	// MergeStrategy is the strategy used (or planned) for landing the
	// run's commits on the target branch: "squash" or "merge". "squash"
	// collapses all run commits into one (default); "merge" fast-forwards
	// the target onto FinalCommit, preserving history.
	MergeStrategy MergeStrategy `json:"merge_strategy,omitempty" bson:"merge_strategy,omitempty"`
	// AutoMerge captures the launch-time intent: when true, the engine
	// applies MergeStrategy synchronously at end of run; when false, the
	// merge is deferred to a UI-driven action (POST /api/runs/{id}/merge).
	AutoMerge bool `json:"auto_merge,omitempty" bson:"auto_merge,omitempty"`
	// MergeStatus tracks whether the merge has happened yet:
	//   "pending"  — storage branch created, merge awaiting user action
	//   "merged"   — merge succeeded; MergedInto + MergedCommit are set
	//   "skipped"  — explicit opt-out (merge_into="none") or no commits
	//   "failed"   — auto-merge attempted but failed; user can retry
	MergeStatus MergeStatus `json:"merge_status,omitempty" bson:"merge_status,omitempty"`
	// MergedCommit is the SHA on the target branch after the merge.
	// Equal to FinalCommit for "merge" (FF) strategy; a fresh squash
	// commit SHA for "squash". Empty when not yet merged.
	MergedCommit string `json:"merged_commit,omitempty" bson:"merged_commit,omitempty"`
	// PendingMergeMessage is the squash commit message captured when
	// the deferred merge hit content conflicts. Preserved so the
	// conflict-resolver UI can finalize with the same message the
	// original attempt was going to use, without recomputing (the
	// upstream `git log` walk that BuildSquashMessage runs is
	// non-trivial). Cleared on merge success or abort.
	PendingMergeMessage string `json:"pending_merge_message,omitempty" bson:"pending_merge_message,omitempty"`
	// PendingMergeInto is the target branch the original merge attempt
	// was aimed at. Recorded alongside PendingMergeMessage so the
	// finalize call doesn't second-guess the target if the user
	// switched branches between conflict-time and resolution-time.
	PendingMergeInto string `json:"pending_merge_into,omitempty" bson:"pending_merge_into,omitempty"`

	// --- Cloud-only fields (plan §D.1) -------------------------------
	// These mirror the BSON layout for cloud runs but are zero-valued
	// in filesystem mode. JSON omitempty keeps them out of legacy
	// run.json files; bson omitempty keeps Mongo documents compact.

	// SchemaVersion is the BSON schema version of the persisted document.
	// Set on Mongo writes only; absent from filesystem run.json. The
	// MongoRunStore refuses to decode documents with v > known.
	SchemaVersion int `json:"-" bson:"v,omitempty"`
	// QueuedAt is the wall-clock time the run was accepted by the
	// server and pushed onto the NATS queue. Set in cloud mode at
	// CreateRun time; zero in local mode. Used to compute
	// queue-position aggregates and queue-wait duration metrics.
	QueuedAt *time.Time `json:"queued_at,omitempty" bson:"queued_at,omitempty"`
	// StartedAt is set by a runner pod the first time it picks up the
	// run (transitioning queued → running). Distinct from CreatedAt.
	StartedAt *time.Time `json:"started_at,omitempty" bson:"started_at,omitempty"`
	// RunnerID identifies the runner pod currently holding the lease
	// (e.g. hostname or k8s pod name). Cleared on completion. Used by
	// the observability dashboards to correlate runs to runner pods.
	RunnerID string `json:"runner_id,omitempty" bson:"runner_id,omitempty"`
	// RepoURL / RepoSHA describe the workspace the runner clones for
	// the run. Empty in local mode (workspace is the user's cwd).
	RepoURL string `json:"repo_url,omitempty" bson:"repo_url,omitempty"`
	RepoSHA string `json:"repo_sha,omitempty" bson:"repo_sha,omitempty"`
	// ProjectPath is the stable forge slug ("group/project" on GitLab,
	// "owner/repo" on GitHub/Forgejo) the run targets. Distinct from the
	// raw RepoURL clone URL: it is the normalized, human-meaningful
	// identifier used to filter/group runs by repository in the studio.
	// Set by inbound-webhook launches (the repo-scoped cloud case);
	// empty for local runs and non-webhook cloud launches.
	ProjectPath string `json:"project_path,omitempty" bson:"project_path,omitempty"`
	// BotID is the bot bundle name (for example, "review-pr") that
	// launched this run. It is persisted so cloud resume/retry can
	// re-resolve bot-secret bindings after the initial queue message
	// has been consumed or its sealed secrets bundle has expired. Empty
	// for plain .bot launches and legacy runs.
	BotID string `json:"bot_id,omitempty" bson:"bot_id,omitempty"`
	// BotSourceTenant records WHICH stored-bot tier this run's bundle was
	// resolved from at launch: the owning team id for a team-authored bot,
	// botsource.PlatformTenantID for a platform override, empty for a baked
	// catalog / loose bot. Resume re-resolves the SAME row (fresh version)
	// instead of re-deriving the tier from a path string — without it, a
	// team bot's resume silently picked up a same-slug platform override
	// (or the baked bundle), and a unique-slug team bot could not resume
	// at all.
	BotSourceTenant string `json:"bot_source_tenant,omitempty" bson:"bot_source_tenant,omitempty"`
	// KeyOverrides pins a BYOK key per LLM provider (provider → api_key id)
	// for this run, persisted so cloud resume re-resolves with the same
	// keys. Set by webhook launches carrying per-webhook key bindings;
	// empty otherwise. See docs/byok.md.
	KeyOverrides map[string]string `json:"key_overrides,omitempty" bson:"key_overrides,omitempty"`
	// SecretOverrides pins a stored secret per workflow-secret name (name ->
	// secret id) for this run, persisted so cloud resume re-resolves the same
	// secrets. Set by webhook launches. See docs/byok.md.
	SecretOverrides map[string]string `json:"secret_overrides,omitempty" bson:"secret_overrides,omitempty"`
	// QueueMsgID is the NATS Nats-Msg-Id header value, exposed so a
	// cancel can target the queued message before pickup. Empty after
	// pickup or when not in cloud mode.
	QueueMsgID string `json:"queue_msg_id,omitempty" bson:"queue_msg_id,omitempty"`
	// CASVersion is the optimistic-lock counter incremented on every
	// SaveCheckpoint / UpdateRunStatus in cloud mode. The runner's
	// checkpoint write conditions on the previous value; a mismatch
	// signals two runners raced and one must back off. Zero in local
	// mode (filesystem flock guards single-writer semantics).
	CASVersion int64 `json:"-" bson:"version,omitempty"`

	// TenantID is the team_id the run belongs to. Set on every cloud
	// run at Launch from the JWT's active team. Local-mode runs leave
	// it empty — the filesystem store is implicitly single-tenant.
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	// OwnerID is the user_id of the principal who launched the run.
	// Empty for local mode (TTY user) and for legacy runs predating
	// multitenancy.
	OwnerID string `json:"owner_id,omitempty" bson:"owner_id,omitempty"`

	// Attachments holds the metadata for binary inputs declared in
	// the workflow's `attachments:` block and uploaded at launch.
	// Bytes live in the storage backend keyed by Name; this map
	// stays light enough that a Mongo document with attachments
	// remains well under the 16 MB BSON ceiling.
	Attachments map[string]AttachmentRecord `json:"attachments,omitempty" bson:"attachments,omitempty"`

	// LaunchEnv captures the iterion-relevant env vars active at run
	// creation. Operators tune model + effort + provider via env
	// (ITERION_RENOVACY_MODEL_CLAUDE, ITERION_RENOVACY_EFFORT_*,
	// RESCUE_PROVIDER, …); the run's behaviour depends on those
	// values, so the run record needs them to be reproducible months
	// later. Only env vars whose name starts with the configured
	// prefixes (ITERION_, RESCUE_, …; see store/run_env.go) are
	// captured — process-wide PATH / HOME / etc. would bloat the
	// record without adding signal. Empty for legacy runs predating
	// this field.
	LaunchEnv map[string]string `json:"launch_env,omitempty" bson:"launch_env,omitempty"`

	// IterionVersion is the iterion build identifier (commit SHA, or
	// version string for tagged releases) at the moment the run was
	// created. Different daemon builds can drive the same recipe to
	// different outcomes — capturing the version makes "why did the
	// 2026-05-10 run finish but the 2026-05-15 run fail" answerable
	// without git-bisecting blindly. Empty for legacy runs.
	IterionVersion string `json:"iterion_version,omitempty" bson:"iterion_version,omitempty"`

	// ForkedFrom, when non-empty, identifies the parent run this run
	// was forked from via POST /api/runs/{id}/fork. ForkAnchor records
	// the (node_id, turn_index) snapshot the fork was anchored at.
	// SourceHash is the parent's workflow hash at fork time; the
	// current Run.WorkflowHash captures the *child's* workflow at
	// fork, which may differ when the source .bot has changed (fork
	// bypasses the strict-hash check by default — see plan Phase 3).
	ForkedFrom string      `json:"forked_from,omitempty" bson:"forked_from,omitempty"`
	ForkAnchor *ForkAnchor `json:"fork_anchor,omitempty" bson:"fork_anchor,omitempty"`
	SourceHash string      `json:"source_hash,omitempty" bson:"source_hash,omitempty"`

	// Source records the originating action that produced this run —
	// today, only "dispatcher" runs carry an Issue back-reference, but
	// the shape leaves room for CLI / studio / cloud-API variants
	// without another schema bump. The studio's RunHeader reads
	// IssueID / IssueIdentifier / IssueTitle to render a "from ticket
	// #X" link back to the kanban; resume re-stamps the same Source
	// so the linkage survives.
	Source *RunSource `json:"source,omitempty" bson:"source,omitempty"`

	// WatchedIssueIDs is the set of native-kanban issue IDs this run has
	// subscribed to (MVP3b). When a watched issue changes board state,
	// the server-side watch coordinator enqueues a user-message onto the
	// run so the bot sees the transition between turns. Populated by the
	// engine's onNodeFinished hook from a dispatch node's `dispatched_ids`
	// output, and by the explicit POST/DELETE /api/runs/{id}/watch
	// endpoints. Empty for runs that never dispatched / subscribed.
	WatchedIssueIDs []string `json:"watched_issue_ids,omitempty" bson:"watched_issue_ids,omitempty"`

	// CallbackURL, when set, is an http/https endpoint the engine POSTs
	// a run-completion webhook to when the run reaches a terminal state
	// (see pkg/notify). Supplied at launch by a programmatic caller — a
	// chat adapter, a CI bridge — that wants to be told when the run
	// finished without polling. Empty for the common case (CLI / studio
	// launches). The delivery passes an SSRF guard; the URL may embed a
	// caller correlation secret in its query string, so it is never
	// logged.
	CallbackURL string `json:"callback_url,omitempty" bson:"callback_url,omitempty"`
	// CallbackToken is an opaque value echoed back verbatim in the
	// completion payload's callback_token field. Lets the receiver
	// correlate the callback to the originating request (e.g. a chat
	// thread id) without keeping server-side state. Empty when no
	// callback was requested.
	CallbackToken string `json:"callback_token,omitempty" bson:"callback_token,omitempty"`
	// CallbackAnswerNode optionally names the node whose latest artifact
	// holds the run's user-facing answer (read from the "final_answer"
	// field). When empty, the notifier scans every artifact-producing
	// node for a "final_answer" field. Set by callers that know their
	// bot's terminal node id, to disambiguate.
	CallbackAnswerNode string `json:"callback_answer_node,omitempty" bson:"callback_answer_node,omitempty"`
}

// dedupeNonEmpty returns ids with empty strings and duplicates removed,
// first-seen order preserved. Returns nil when the result is empty so
// the WatchedIssueIDs field stays omitted from the persisted record.
func dedupeNonEmpty(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeWatchedIssues returns the deduped union of existing and add,
// insertion order preserved (existing entries lead). dedupeNonEmpty
// preserves first-seen order, so feeding it slices.Concat(existing, add)
// produces the same observable ordering as the prior hand-rolled append.
func mergeWatchedIssues(existing, add []string) []string {
	return dedupeNonEmpty(slices.Concat(existing, add))
}

// removeWatchedIssues returns existing with every entry in drop removed.
// Returns nil when the result is empty so the field stays omitted.
func removeWatchedIssues(existing, drop []string) []string {
	if len(existing) == 0 {
		return nil
	}
	dropSet := make(map[string]struct{}, len(drop))
	for _, id := range drop {
		dropSet[id] = struct{}{}
	}
	out := make([]string, 0, len(existing))
	for _, id := range existing {
		if _, ok := dropSet[id]; ok {
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Recognised values for RunSource.Kind. The field stays a free-form
// string so the schema is forward-compatible with future producers
// (cloud-API, scheduled runs, fork variants); these consts cover the
// known set today.
const (
	RunSourceKindDispatcher = "dispatcher"
	RunSourceKindSchedule   = "schedule"
)

// RunSource captures who originated this run. Populated by the
// dispatcher when an issue is claimed and by the scheduled-launch
// paths (host-cron, trigger spine, cloudsched); empty for CLI /
// studio / fork-spawned runs.
type RunSource struct {
	// Kind is the producer of this run (see RunSourceKind* consts).
	Kind string `json:"kind,omitempty" bson:"kind,omitempty"`
	// IssueID is the tracker-specific opaque id (e.g.
	// "native:55283bbc-…" or "github:repo#123") of the issue that
	// triggered the dispatch.
	IssueID string `json:"issue_id,omitempty" bson:"issue_id,omitempty"`
	// IssueIdentifier is the short, human-facing identifier the
	// tracker exposes (e.g. "55283bbc" or "123") — used in URLs and
	// in the studio's chip label.
	IssueIdentifier string `json:"issue_identifier,omitempty" bson:"issue_identifier,omitempty"`
	// IssueTitle is the issue's title at dispatch time. Snapshot —
	// not re-fetched on resume, so a later title edit doesn't
	// rewrite the historical run record.
	IssueTitle string `json:"issue_title,omitempty" bson:"issue_title,omitempty"`
	// ScheduleID is the stable schedule identity for
	// RunSourceKindSchedule runs: ScheduleEntry.Name (host-cron),
	// trigger.Subscription.ID (spine), cloudsched.ScheduledBot.ID
	// (cloud). Deliberately distinct from IssueID — the overlap gate
	// queries it (ListRunsBySchedule) and the issue index must not be
	// polluted with schedule identities.
	ScheduleID string `json:"schedule_id,omitempty" bson:"schedule_id,omitempty"`
	// ScheduleName is the human label of the schedule when it differs
	// from ScheduleID (display only).
	ScheduleName string `json:"schedule_name,omitempty" bson:"schedule_name,omitempty"`
}

// ForkAnchor identifies where a forked run resumes inside the parent's
// execution graph. NodeID is the node whose turn we forked at; TurnIndex
// is the monotonic per-(node,iteration) index (0-based, populated by the
// OnStepFinish observer); LoopIter is the loop iteration count for that
// node when the snapshot was taken. RewindCode is true when the fork
// also reset the worktree to the snapshot's git ref (Phase 3+).
type ForkAnchor struct {
	NodeID     string `json:"node_id" bson:"node_id"`
	LoopIter   int    `json:"loop_iter" bson:"loop_iter"`
	TurnIndex  int    `json:"turn_index" bson:"turn_index"`
	RewindCode bool   `json:"rewind_code,omitempty" bson:"rewind_code,omitempty"`
}

// MergeStrategy enumerates how the run's commits are landed on the
// user's target branch at finalization (or via the deferred UI action).
type MergeStrategy string

const (
	MergeStrategySquash MergeStrategy = "squash"
	MergeStrategyMerge  MergeStrategy = "merge"
)

// MergeStatus enumerates the lifecycle of the merge step independently
// from the overall RunStatus — a finished run may still have a pending
// merge if AutoMerge was off.
type MergeStatus string

const (
	MergeStatusPending MergeStatus = "pending"
	MergeStatusMerged  MergeStatus = "merged"
	MergeStatusSkipped MergeStatus = "skipped"
	MergeStatusFailed  MergeStatus = "failed"
	// MergeStatusConflicted means `git merge --squash` produced
	// content conflicts and the worktree is currently in the
	// conflicted state (UU paths, markers on disk). The operator
	// resolves via the in-studio editor or aborts; once every file
	// is staged, the finalize endpoint commits the squash and the
	// status flips to "merged".
	MergeStatusConflicted MergeStatus = "conflicted"
)

// NodeSessionSlot is the durable persist slot for one LLM node (ADR-089).
// StateRef names a blob in BackendSessionStore. Empty StateRef means the
// slot is not rehydratable (id-only is not a slot).
type NodeSessionSlot struct {
	Backend         string `json:"backend"`
	SessionID       string `json:"session_id,omitempty"`
	Fingerprint     string `json:"fingerprint,omitempty"`
	StateRef        string `json:"state_ref,omitempty"`
	ConversationRef string `json:"conversation_ref,omitempty"`
}

// Checkpoint captures the runtime state at a pause point (human node or
// backend interaction), enabling exact resume without replaying upstream nodes.
//
// The checkpoint embedded in run.json is the authoritative source of truth for
// resume. Events (events.jsonl) are observational only — they are not replayed
// to reconstruct state. If the checkpoint is lost, recovery is not possible via
// event replay. The separate interaction file (interactions/<id>.json) is a
// convenience for tooling; InteractionQuestions is embedded here for resilience.
type Checkpoint struct {
	NodeID             string                    `json:"node_id" bson:"node_id"`                                               // the node where we paused
	InteractionID      string                    `json:"interaction_id" bson:"interaction_id,omitempty"`                       // pending interaction ID
	Outputs            map[string]map[string]any `json:"outputs" bson:"outputs"`                                               // per-node outputs accumulated so far
	LoopCounters       map[string]int            `json:"loop_counters" bson:"loop_counters"`                                   // current loop iteration counts
	RoundRobinCounters map[string]int            `json:"round_robin_counters,omitempty" bson:"round_robin_counters,omitempty"` // round-robin router counters (keyed by router node ID)
	// LoopPreviousOutput / LoopCurrentOutput preserve the rotating snapshot
	// of source-node outputs at each loop-edge traversal so that
	// {{loop.<name>.previous_output}} resolves correctly across resume.
	// Without these, a paused/failed run would lose the prior-iteration
	// snapshot and the very next iteration would see nil.
	LoopPreviousOutput map[string]map[string]any `json:"loop_previous_output,omitempty" bson:"loop_previous_output,omitempty"`
	LoopCurrentOutput  map[string]map[string]any `json:"loop_current_output,omitempty" bson:"loop_current_output,omitempty"`
	// LoopBudgetMarks preserves what each loop had consumed, per budget
	// dimension, at its last back-edge crossing (or at loop entry) — the
	// basis on which the runtime prices one more iteration against the
	// budget's remaining allowance. Without it a resume landing on a
	// loop's decision node would cross with no measurement and launch a
	// pass the budget cannot fund, which is exactly the stranding the
	// affordability guard exists to prevent.
	LoopBudgetMarks  map[string]map[string]float64 `json:"loop_budget_marks,omitempty" bson:"loop_budget_marks,omitempty"`
	ArtifactVersions map[string]int                `json:"artifact_versions" bson:"artifact_versions"` // next artifact version per node
	// SelectedIncoming records, per destination node, the incoming edges
	// that routing actually selected for the current visit of that node.
	// buildNodeInputRS applies with-mappings only from those edges so an
	// unselected conditional sibling cannot clobber a selected path
	// (issue #484). Absent on checkpoints written before the field
	// existed — the resolver then falls back to "every incoming edge
	// whose source has produced output".
	SelectedIncoming map[string][]IncomingEdge `json:"selected_incoming,omitempty" bson:"selected_incoming,omitempty"`
	Vars             map[string]any            `json:"vars" bson:"vars"` // resolved workflow variables
	// InteractionQuestions embeds the questions from the interaction record
	// so that resume is self-sufficient even if the interaction file is deleted.
	InteractionQuestions map[string]any `json:"interaction_questions,omitempty" bson:"interaction_questions,omitempty"`
	// BackendSessionID is the session ID of a blocked backend, enabling
	// re-invocation with session: inherit on resume.
	BackendSessionID string `json:"backend_session_id,omitempty" bson:"backend_session_id,omitempty"`
	// BackendName identifies which backend was used.
	BackendName string `json:"backend_name,omitempty" bson:"backend_name,omitempty"`
	// BackendConversation is the opaque, backend-specific persisted
	// conversation captured at the moment of an ask_user pause. On
	// resume, the backend rehydrates from this blob (claw: []api.Message)
	// and appends a tool_result block answering BackendPendingToolUseID,
	// avoiding a stateless restart from system+user prompts.
	BackendConversation json.RawMessage `json:"backend_conversation,omitempty" bson:"backend_conversation,omitempty"`
	// BackendPendingToolUseID is the ID of the tool_use block awaiting
	// an answer in BackendConversation. Required when BackendConversation
	// is non-nil.
	BackendPendingToolUseID string `json:"backend_pending_tool_use_id,omitempty" bson:"backend_pending_tool_use_id,omitempty"`
	// NodeSessions holds per-node persist slots (ADR-089). Keyed by node id.
	NodeSessions map[string]NodeSessionSlot `json:"node_sessions,omitempty" bson:"node_sessions,omitempty"`
	// BackendSessionStateRef is the in-flight CLI ask_user pack (ADR-089).
	// Distinct from NodeSessions: that map is the last *completed* visit.
	BackendSessionStateRef string `json:"backend_session_state_ref,omitempty" bson:"backend_session_state_ref,omitempty"`
	// NodeAttempts records prior failed attempts per (node_id, error_code) so
	// that resume preserves the recovery dispatcher's retry budget. Outer key
	// is the node ID, inner key is the runtime error code (string-typed).
	NodeAttempts map[string]map[string]int `json:"node_attempts,omitempty" bson:"node_attempts,omitempty"`

	// BudgetTokensUsed / BudgetCostUSD / BudgetIterationsUsed / BudgetElapsedNS
	// persist the run-scoped SharedBudget consumption so a resume continues
	// from what was already spent instead of re-granting the full
	// max_tokens / max_cost_usd / max_iterations / max_duration allowance.
	// Without them a run that auto-resumes (dispatcher retry, transient
	// failure) could spend an unbounded multiple of its declared budget, and
	// the max_iterations runaway-loop guard would reset on every resume.
	// Zero/omitted on legacy checkpoints (a fresh budget — the prior behaviour).
	BudgetTokensUsed int     `json:"budget_tokens_used,omitempty" bson:"budget_tokens_used,omitempty"`
	BudgetCostUSD    float64 `json:"budget_cost_usd,omitempty" bson:"budget_cost_usd,omitempty"`
	// BudgetUnpricedTokens / BudgetUnpricedNodes carry the volume the cost
	// axis could not price. Absent from checkpoints written before they
	// existed, which restores as zero — the prior behaviour.
	BudgetUnpricedTokens int   `json:"budget_unpriced_tokens,omitempty" bson:"budget_unpriced_tokens,omitempty"`
	BudgetUnpricedNodes  int   `json:"budget_unpriced_nodes,omitempty" bson:"budget_unpriced_nodes,omitempty"`
	BudgetIterationsUsed int   `json:"budget_iterations_used,omitempty" bson:"budget_iterations_used,omitempty"`
	BudgetElapsedNS      int64 `json:"budget_elapsed_ns,omitempty" bson:"budget_elapsed_ns,omitempty"`
	// CostUSDTotal is the run's cumulative LLM spend across ALL execution
	// segments. Persisted so the daily-spend-cap ledger (a monotonic max of
	// the per-run cumulative) keeps climbing after a resume instead of
	// restarting from 0 — otherwise post-resume spend stays invisible to the
	// cap until it re-exceeds the pre-pause peak.
	CostUSDTotal float64 `json:"cost_usd_total,omitempty" bson:"cost_usd_total,omitempty"`
}

// IncomingEdge identifies a workflow edge that actually fired into a node
// for the current visit. Identity is the routing shape (from/to plus the
// guard and loop/foreach marks), not the with-mapping list: two edges
// between the same pair with different conditions are distinct.
type IncomingEdge struct {
	From          string `json:"from" bson:"from"`
	To            string `json:"to" bson:"to"`
	Condition     string `json:"condition,omitempty" bson:"condition,omitempty"`
	Negated       bool   `json:"negated,omitempty" bson:"negated,omitempty"`
	ExpressionSrc string `json:"expression,omitempty" bson:"expression,omitempty"`
	IsElse        bool   `json:"else,omitempty" bson:"else,omitempty"`
	LoopName      string `json:"loop,omitempty" bson:"loop,omitempty"`
	ForeachName   string `json:"foreach,omitempty" bson:"foreach,omitempty"`
}

// ---------------------------------------------------------------------------
// Artifact — structured output of a node
// ---------------------------------------------------------------------------

// Artifact is a versioned output persisted under artifacts/<node>/<version>.json.
type Artifact struct {
	RunID   string         `json:"run_id" bson:"run_id"`
	NodeID  string         `json:"node_id" bson:"node_id"`
	Version int            `json:"version" bson:"version"`
	Data    map[string]any `json:"data" bson:"data"`
	// Labels categorise the artifact (e.g. "plan", "verdict") so the studio
	// can group artifacts by label. Sourced from the node's DSL
	// `artifact_labels:` plus a shape heuristic (pkg/artifactlabels). Empty
	// on legacy artifacts written before this field existed.
	Labels    []string  `json:"labels,omitempty" bson:"labels,omitempty"`
	WrittenAt time.Time `json:"written_at" bson:"written_at"`
}

// ---------------------------------------------------------------------------
// Interaction — human input/output exchange
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// AttachmentRecord — per-run binary input metadata
// ---------------------------------------------------------------------------

// AttachmentRecord is the lightweight metadata persisted for one
// attachment uploaded as part of a run. The bytes themselves live
// in the storage backend (filesystem or S3). Only metadata is
// attached to Run.Attachments so run.json / Mongo documents stay
// compact and replayable.
type AttachmentRecord struct {
	// Name is the declared attachment name from the .bot
	// `attachments:` block (e.g. "logo", "spec").
	Name string `json:"name" bson:"name"`
	// OriginalFilename is the filename provided by the uploader
	// before the server normalised it. Preserved so the resolved
	// path stays human-readable to agents inspecting the file.
	OriginalFilename string `json:"original_filename" bson:"original_filename"`
	// MIME is the validated content type after server-side sniff
	// (http.DetectContentType on the first 512 bytes).
	MIME string `json:"mime" bson:"mime"`
	// Size is the byte length of the upload as stored.
	Size int64 `json:"size" bson:"size"`
	// SHA256 is the hex-encoded SHA-256 of the upload, computed
	// streaming during ingestion.
	SHA256 string `json:"sha256" bson:"sha256"`
	// CreatedAt is the time the upload was promoted to the run
	// (i.e. moved out of the staging area into the run-scoped
	// storage).
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
	// StorageRef is the canonical key in the storage backend.
	// For the filesystem backend this is the path relative to the
	// store root (e.g. "runs/<id>/attachments/<name>/<filename>").
	// For the S3 backend it is the object key
	// (e.g. "attachments/<id>/<name>/<filename>"). Callers should
	// not parse this — go through RunStore.OpenAttachment instead.
	StorageRef string `json:"storage_ref" bson:"storage_ref"`
}

// ---------------------------------------------------------------------------
// Interaction — human input/output exchange
// ---------------------------------------------------------------------------

// Interaction kinds (ADR-081). The zero value ("") is a legacy/blocking
// pause interaction — the run is paused_waiting_human on it.
const (
	// InteractionKindAsync is a non-blocking question posted by an
	// interaction: async agent via the ask_user_async tool. The run keeps
	// executing; pending = AnsweredAt == nil.
	InteractionKindAsync = "async"
	// InteractionKindAwait is the synthetic pause interaction created when
	// an agent calls the await_answers tool while async questions are
	// still pending. Its Questions carry the pending async interaction IDs.
	InteractionKindAwait = "await"
)

// Interaction records a human pause/resume exchange.
type Interaction struct {
	ID          string         `json:"id" bson:"interaction_id"`
	RunID       string         `json:"run_id" bson:"run_id"`
	NodeID      string         `json:"node_id" bson:"node_id"`
	Kind        string         `json:"kind,omitempty" bson:"kind,omitempty"` // "" (blocking pause) | InteractionKindAsync | InteractionKindAwait
	RequestedAt time.Time      `json:"requested_at" bson:"requested_at"`
	AnsweredAt  *time.Time     `json:"answered_at,omitempty" bson:"answered_at,omitempty"`
	Questions   map[string]any `json:"questions,omitempty" bson:"questions,omitempty"`
	Answers     map[string]any `json:"answers,omitempty" bson:"answers,omitempty"`
	// Turns is the ordered companion↔human dialogue for a review gate
	// (interaction: review). The gate re-pauses on the same interaction ID
	// each round and appends a turn, so the whole conversation lives here
	// and re-renders verbatim on resume. Empty for ordinary single-shot
	// human pauses.
	Turns []InteractionTurn `json:"turns,omitempty" bson:"turns,omitempty"`
	// TenantID mirrors Run.TenantID so cross-tenant access checks can
	// be enforced at the interaction layer too. Empty for legacy
	// filesystem records.
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	// RetiredAt marks a question the run no longer asks — set when a
	// rewind invalidates the node that posted it.
	//
	// ADR-081's async pair is level-triggered against the STORE, not the
	// checkpoint: await_answers parks until the pending list is empty and
	// returns every answered record. So a rewind that only cleared the
	// checkpoint pointer left its questions live, and the replayed node
	// blocked on the union of its new questions and the abandoned ones —
	// or mixed pre- and post-rewind answers into one output. A retired
	// record is in neither list.
	RetiredAt *time.Time `json:"retired_at,omitempty" bson:"retired_at,omitempty"`
}

// InteractionTurn is one message in a guided review-gate dialogue.
// The companion (an LLM that walks the human through testing the change)
// and the human alternate turns until the gate squash-merges.
type InteractionTurn struct {
	Role    string         `json:"role" bson:"role"`                           // "companion" | "human"
	Content string         `json:"content,omitempty" bson:"content,omitempty"` // rendered companion message, or the human's reply text
	Verdict map[string]any `json:"verdict,omitempty" bson:"verdict,omitempty"` // companion's structured verdict (decision/confidence/blockers)
	At      time.Time      `json:"at" bson:"at"`
}
