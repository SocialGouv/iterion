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
)

// ---------------------------------------------------------------------------
// Run — top-level run metadata persisted in run.json
// ---------------------------------------------------------------------------

// RunFormatVersion is the current version of the persisted run.json format.
// Bump this when making breaking changes to the Run struct.
const RunFormatVersion = 1

// Run is the top-level metadata for a single workflow invocation.
type Run struct {
	FormatVersion int    `json:"format_version"`
	ID            string `json:"id"`
	// Name is a deterministic, human-friendly label derived from
	// (file_path + run_id) at run creation. Display-only — the
	// canonical identifier remains ID. Empty for runs persisted
	// before this field existed; surfaces should fall back to
	// WorkflowName in that case.
	Name          string                 `json:"name,omitempty"`
	WorkflowName  string                 `json:"workflow_name"`
	WorkflowHash  string                 `json:"workflow_hash,omitempty"` // SHA-256 of the .iter source at run start
	FilePath      string                 `json:"file_path,omitempty"`     // absolute .iter source path captured at launch (resume without re-supplying file)
	Status        RunStatus              `json:"status"`
	Inputs        map[string]interface{} `json:"inputs,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
	FinishedAt    *time.Time             `json:"finished_at,omitempty"`
	Error         string                 `json:"error,omitempty"`
	Checkpoint    *Checkpoint            `json:"checkpoint,omitempty"`
	ArtifactIndex map[string]int         `json:"artifact_index,omitempty"` // node_id → latest version written
	// WorkDir is the absolute filesystem path the run executes in
	// (the per-run git worktree when Worktree is true, otherwise the
	// engine's resolved cwd at start). Persisted so editor surfaces
	// (e.g. modified-files panel) can locate the run's working tree
	// without re-deriving it from the runtime.
	WorkDir string `json:"work_dir,omitempty"`
	// Worktree is true when WorkDir was created by `worktree: auto`,
	// false when WorkDir is the inherited cwd.
	Worktree bool `json:"worktree,omitempty"`
	// RepoRoot is the absolute path of the main git repository the
	// worktree was forked from. Used by the editor's modified-files
	// panel after the worktree directory is gc'd to compute the diff
	// against FinalCommit (the persistent branch lives in this repo's
	// shared .git). Empty for non-worktree runs.
	RepoRoot string `json:"repo_root,omitempty"`
	// BaseCommit is the SHA of HEAD on the main repo at the moment the
	// worktree was created — i.e. the run's baseline. The post-finalization
	// diff renders FinalCommit relative to this commit. Empty for non-
	// worktree runs and for legacy runs that predate this field.
	BaseCommit string `json:"base_commit,omitempty"`
	// FinalCommit is the SHA the worktree's HEAD pointed to when the
	// run finished successfully, captured before the worktree was torn
	// down. Empty when the run made no commits, didn't use a worktree,
	// or didn't finish.
	FinalCommit string `json:"final_commit,omitempty"`
	// FinalBranch is the persistent branch name created on
	// FinalCommit (default "iterion/run/<friendly-name>", overridable
	// via launch params). Acts as a GC guard so the commits remain
	// reachable after the worktree directory is removed.
	FinalBranch string `json:"final_branch,omitempty"`
	// MergedInto is the branch the engine fast-forwarded to FinalCommit
	// after the run, or empty when the FF was skipped (dirty main,
	// non-FF, branch divergence, opt-out, or detached HEAD at start).
	MergedInto string `json:"merged_into,omitempty"`
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
	NodeID             string                            `json:"node_id"`                        // the node where we paused
	InteractionID      string                            `json:"interaction_id"`                 // pending interaction ID
	Outputs            map[string]map[string]interface{} `json:"outputs"`                        // per-node outputs accumulated so far
	LoopCounters       map[string]int                    `json:"loop_counters"`                  // current loop iteration counts
	RoundRobinCounters map[string]int                    `json:"round_robin_counters,omitempty"` // round-robin router counters (keyed by router node ID)
	// LoopPreviousOutput / LoopCurrentOutput preserve the rotating snapshot
	// of source-node outputs at each loop-edge traversal so that
	// {{loop.<name>.previous_output}} resolves correctly across resume.
	// Without these, a paused/failed run would lose the prior-iteration
	// snapshot and the very next iteration would see nil.
	LoopPreviousOutput map[string]map[string]interface{} `json:"loop_previous_output,omitempty"`
	LoopCurrentOutput  map[string]map[string]interface{} `json:"loop_current_output,omitempty"`
	ArtifactVersions   map[string]int                    `json:"artifact_versions"` // next artifact version per node
	Vars               map[string]interface{}            `json:"vars"`              // resolved workflow variables
	// InteractionQuestions embeds the questions from the interaction record
	// so that resume is self-sufficient even if the interaction file is deleted.
	InteractionQuestions map[string]interface{} `json:"interaction_questions,omitempty"`
	// BackendSessionID is the session ID of a blocked backend, enabling
	// re-invocation with session: inherit on resume.
	BackendSessionID string `json:"backend_session_id,omitempty"`
	// BackendName identifies which backend was used.
	BackendName string `json:"backend_name,omitempty"`
	// BackendConversation is the opaque, backend-specific persisted
	// conversation captured at the moment of an ask_user pause. On
	// resume, the backend rehydrates from this blob (claw: []api.Message)
	// and appends a tool_result block answering BackendPendingToolUseID,
	// avoiding a stateless restart from system+user prompts.
	BackendConversation json.RawMessage `json:"backend_conversation,omitempty"`
	// BackendPendingToolUseID is the ID of the tool_use block awaiting
	// an answer in BackendConversation. Required when BackendConversation
	// is non-nil.
	BackendPendingToolUseID string `json:"backend_pending_tool_use_id,omitempty"`
	// NodeAttempts records prior failed attempts per (node_id, error_code) so
	// that resume preserves the recovery dispatcher's retry budget. Outer key
	// is the node ID, inner key is the runtime error code (string-typed).
	NodeAttempts map[string]map[string]int `json:"node_attempts,omitempty"`
}

// ---------------------------------------------------------------------------
// Artifact — structured output of a node
// ---------------------------------------------------------------------------

// Artifact is a versioned output persisted under artifacts/<node>/<version>.json.
type Artifact struct {
	RunID     string                 `json:"run_id"`
	NodeID    string                 `json:"node_id"`
	Version   int                    `json:"version"`
	Data      map[string]interface{} `json:"data"`
	WrittenAt time.Time              `json:"written_at"`
}

// ---------------------------------------------------------------------------
// Interaction — human input/output exchange
// ---------------------------------------------------------------------------

// Interaction records a human pause/resume exchange.
type Interaction struct {
	ID          string                 `json:"id"`
	RunID       string                 `json:"run_id"`
	NodeID      string                 `json:"node_id"`
	RequestedAt time.Time              `json:"requested_at"`
	AnsweredAt  *time.Time             `json:"answered_at,omitempty"`
	Questions   map[string]interface{} `json:"questions,omitempty"`
	Answers     map[string]interface{} `json:"answers,omitempty"`
}
