package store

import (
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// RunStatus — lifecycle state of a run
// ---------------------------------------------------------------------------

// RunStatus represents the current state of a run.
type RunStatus string

const (
	RunStatusRunning            RunStatus = "running"
	RunStatusPausedWaitingHuman RunStatus = "paused_waiting_human"
	RunStatusFinished           RunStatus = "finished"
	RunStatusFailed             RunStatus = "failed"
	RunStatusFailedResumable    RunStatus = "failed_resumable"
	RunStatusCancelled          RunStatus = "cancelled"
	// RunStatusQueued is set on cloud-mode runs that have been submitted
	// to the NATS queue but not yet picked up by a runner pod. Local
	// runs never observe this state — they transition straight to
	// running on Engine.Run(). See cloud-ready plan §A, §F (T-03).
	RunStatusQueued RunStatus = "queued"
)

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
type Run struct {
	FormatVersion int    `json:"format_version" bson:"format_version"`
	ID            string `json:"id" bson:"_id"`
	// Name is a deterministic, human-friendly label derived from
	// (file_path + run_id) at run creation. Display-only — the
	// canonical identifier remains ID. Empty for runs persisted
	// before this field existed; surfaces should fall back to
	// WorkflowName in that case.
	Name          string                 `json:"name,omitempty" bson:"name,omitempty"`
	WorkflowName  string                 `json:"workflow_name" bson:"workflow_name"`
	WorkflowHash  string                 `json:"workflow_hash,omitempty" bson:"workflow_hash,omitempty"` // SHA-256 of the .iter source at run start
	FilePath      string                 `json:"file_path,omitempty" bson:"file_path,omitempty"`         // absolute .iter source path captured at launch (resume without re-supplying file)
	Status        RunStatus              `json:"status" bson:"status"`
	Inputs        map[string]interface{} `json:"inputs,omitempty" bson:"inputs,omitempty"`
	CreatedAt     time.Time              `json:"created_at" bson:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at" bson:"updated_at"`
	FinishedAt    *time.Time             `json:"finished_at,omitempty" bson:"finished_at,omitempty"`
	Error         string                 `json:"error,omitempty" bson:"error,omitempty"`
	Checkpoint    *Checkpoint            `json:"checkpoint,omitempty" bson:"checkpoint,omitempty"`
	ArtifactIndex map[string]int         `json:"artifact_index,omitempty" bson:"artifact_index,omitempty"` // node_id → latest version written
	// WorkDir is the absolute filesystem path the run executes in
	// (the per-run git worktree when Worktree is true, otherwise the
	// engine's resolved cwd at start). Persisted so editor surfaces
	// (e.g. modified-files panel) can locate the run's working tree
	// without re-deriving it from the runtime.
	WorkDir string `json:"work_dir,omitempty" bson:"work_dir,omitempty"`
	// Worktree is true when WorkDir was created by `worktree: auto`,
	// false when WorkDir is the inherited cwd.
	Worktree bool `json:"worktree,omitempty" bson:"worktree,omitempty"`
	// RepoRoot is the absolute path of the main git repository the
	// worktree was forked from. Used by the editor's modified-files
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
)

// Checkpoint captures the runtime state at a pause point (human node or
// backend interaction), enabling exact resume without replaying upstream nodes.
//
// The checkpoint embedded in run.json is the authoritative source of truth for
// resume. Events (events.jsonl) are observational only — they are not replayed
// to reconstruct state. If the checkpoint is lost, recovery is not possible via
// event replay. The separate interaction file (interactions/<id>.json) is a
// convenience for tooling; InteractionQuestions is embedded here for resilience.
type Checkpoint struct {
	NodeID             string                            `json:"node_id" bson:"node_id"`                                               // the node where we paused
	InteractionID      string                            `json:"interaction_id" bson:"interaction_id,omitempty"`                       // pending interaction ID
	Outputs            map[string]map[string]interface{} `json:"outputs" bson:"outputs"`                                               // per-node outputs accumulated so far
	LoopCounters       map[string]int                    `json:"loop_counters" bson:"loop_counters"`                                   // current loop iteration counts
	RoundRobinCounters map[string]int                    `json:"round_robin_counters,omitempty" bson:"round_robin_counters,omitempty"` // round-robin router counters (keyed by router node ID)
	// LoopPreviousOutput / LoopCurrentOutput preserve the rotating snapshot
	// of source-node outputs at each loop-edge traversal so that
	// {{loop.<name>.previous_output}} resolves correctly across resume.
	// Without these, a paused/failed run would lose the prior-iteration
	// snapshot and the very next iteration would see nil.
	LoopPreviousOutput map[string]map[string]interface{} `json:"loop_previous_output,omitempty" bson:"loop_previous_output,omitempty"`
	LoopCurrentOutput  map[string]map[string]interface{} `json:"loop_current_output,omitempty" bson:"loop_current_output,omitempty"`
	ArtifactVersions   map[string]int                    `json:"artifact_versions" bson:"artifact_versions"` // next artifact version per node
	Vars               map[string]interface{}            `json:"vars" bson:"vars"`                           // resolved workflow variables
	// InteractionQuestions embeds the questions from the interaction record
	// so that resume is self-sufficient even if the interaction file is deleted.
	InteractionQuestions map[string]interface{} `json:"interaction_questions,omitempty" bson:"interaction_questions,omitempty"`
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
	// NodeAttempts records prior failed attempts per (node_id, error_code) so
	// that resume preserves the recovery dispatcher's retry budget. Outer key
	// is the node ID, inner key is the runtime error code (string-typed).
	NodeAttempts map[string]map[string]int `json:"node_attempts,omitempty" bson:"node_attempts,omitempty"`
}

// ---------------------------------------------------------------------------
// Artifact — structured output of a node
// ---------------------------------------------------------------------------

// Artifact is a versioned output persisted under artifacts/<node>/<version>.json.
type Artifact struct {
	RunID     string                 `json:"run_id" bson:"run_id"`
	NodeID    string                 `json:"node_id" bson:"node_id"`
	Version   int                    `json:"version" bson:"version"`
	Data      map[string]interface{} `json:"data" bson:"data"`
	WrittenAt time.Time              `json:"written_at" bson:"written_at"`
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
	// Name is the declared attachment name from the .iter
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

// Interaction records a human pause/resume exchange.
type Interaction struct {
	ID          string                 `json:"id" bson:"interaction_id"`
	RunID       string                 `json:"run_id" bson:"run_id"`
	NodeID      string                 `json:"node_id" bson:"node_id"`
	RequestedAt time.Time              `json:"requested_at" bson:"requested_at"`
	AnsweredAt  *time.Time             `json:"answered_at,omitempty" bson:"answered_at,omitempty"`
	Questions   map[string]interface{} `json:"questions,omitempty" bson:"questions,omitempty"`
	Answers     map[string]interface{} `json:"answers,omitempty" bson:"answers,omitempty"`
	// TenantID mirrors Run.TenantID so cross-tenant access checks can
	// be enforced at the interaction layer too. Empty for legacy
	// filesystem records.
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
}
