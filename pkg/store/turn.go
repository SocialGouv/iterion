package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// NodeSnapshotRef builds the canonical git ref name for a per-node
// worktree snapshot, written at the end of every successful node
// execution when the run uses a worktree.
//
//	refs/iterion/runs/<runID>/nodes/<nodeID>/<loopIter>
//
// Listable via `git for-each-ref refs/iterion/runs/<run>/`, GC'd
// en-masse by removing the namespace. Shared between the runtime
// (which writes the ref) and the Fork API (which checks it out with
// `git worktree add` when rewind_code=true).
func NodeSnapshotRef(runID, nodeID string, loopIter int) string {
	return fmt.Sprintf("refs/iterion/runs/%s/nodes/%s/%d", runID, nodeID, loopIter)
}

// TurnSnapshotRef is the per-turn counterpart of NodeSnapshotRef. Not
// yet wired (Phase 5 stretch); reserved here so the namespace is
// documented in one place.
//
//	refs/iterion/runs/<runID>/turns/<nodeID>/<loopIter>/<turn>
func TurnSnapshotRef(runID, nodeID string, loopIter, turn int) string {
	return fmt.Sprintf("refs/iterion/runs/%s/turns/%s/%d/%d", runID, nodeID, loopIter, turn)
}

// TurnCheckpoint records the state of an LLM "turn" inside a node's
// execution, persisted under `runs/<id>/turns/<node>/<iter>/<turn>.json`.
//
// A "turn" is a single LLM request/response round-trip (claw) or a
// whole delegate-call boundary (claude_code — the CLI doesn't expose
// intra-call hooks). The asymmetry is documented at the call sites;
// the studio renders both kinds uniformly in the per-node timeline.
//
// TurnCheckpoint is the anchor for the fork-from-here UX: clicking
// "fork from this turn" picks up TurnIndex + SessionID + (claw only)
// MessagesRef, plus the GitRef populated by Phase 2's snapshot helper
// when worktree tracking is on. Without a TurnCheckpoint, a fork has
// no anchor — so this struct is the load-bearing primitive of the
// interactivity feature set, even before fork ships in Phase 3.
type TurnCheckpoint struct {
	RunID    string `json:"run_id"`
	NodeID   string `json:"node_id"`
	LoopIter int    `json:"loop_iter"`
	// TurnIndex is monotonic within (NodeID, LoopIter). For claw, it
	// increments once per LLM step inside GenerateTextDirect's loop.
	// For claude_code, exactly one TurnCheckpoint is written per
	// delegate-call boundary (TurnIndex always 0 for claude_code, by
	// construction, until the SDK exposes intra-call hooks — Phase 6).
	TurnIndex int `json:"turn_index"`
	// Backend identifies the executor that produced this turn:
	// "claw" or "claude_code". Drives the per-backend rehydration
	// branch in the Fork API (Phase 3/4).
	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`
	// FinishReason mirrors the LLM API's stop reason (e.g. "stop",
	// "tool_use", "max_tokens"). Used by the studio timeline to
	// render a stop-glyph next to each turn.
	FinishReason string `json:"finish_reason,omitempty"`
	// ToolCalls is a lightweight summary of the tools invoked during
	// this turn (name + truncated args/result). Full payloads stay in
	// events.jsonl; this slice is for the timeline preview.
	ToolCalls []TurnToolCall `json:"tool_calls,omitempty"`
	// TextDigest is the SHA-256 of the assistant text emitted in this
	// turn. Cheap fingerprint for the studio's "is this turn identical
	// to the previous attempt" hint without loading the full text.
	TextDigest string    `json:"text_digest,omitempty"`
	Usage      TurnUsage `json:"usage,omitempty"`
	// SessionID, when non-empty, is the claude_code CLI session id
	// captured from Result.SessionID. The Fork API passes it to
	// `claude --resume <id> --fork-session` to materialise the child
	// run's conversation. Empty for claw turns (no session id concept).
	SessionID string `json:"session_id,omitempty"`
	// MessagesRef, when non-empty, names a sibling file
	// `<turn>.messages.json` holding the full []api.Message slice the
	// claw loop had accumulated when this turn completed. Populated
	// only for claw (claude_code's conversation lives in the CLI's
	// own session jsonl, not here). Phase 4.
	MessagesRef string `json:"messages_ref,omitempty"`
	// Messages is the raw blob inlined when MessagesRef points at a
	// sibling file but the caller wants to round-trip the snapshot
	// without a separate read. Kept as RawMessage so the store doesn't
	// need to know the backend's wire format.
	Messages json.RawMessage `json:"-"`
	// GitRef is the worktree snapshot anchor written by
	// snapshotWorktree(): `refs/iterion/runs/<run>/turns/<node>/<iter>/<turn>`.
	// Populated in Phase 2. Fork's rewind_code=true does
	// `git reset --hard <GitRef>` on the child worktree.
	GitRef    string    `json:"git_ref,omitempty"`
	WrittenAt time.Time `json:"written_at"`
}

// TurnToolCall is the lightweight wire form of one tool invocation
// captured on a TurnCheckpoint. The full input/output payloads remain
// in events.jsonl / artifacts; this struct is what the studio's
// timeline popover displays without extra fetches.
type TurnToolCall struct {
	Name           string `json:"name"`
	InputPreview   string `json:"input_preview,omitempty"`
	OutputPreview  string `json:"output_preview,omitempty"`
	IsError        bool   `json:"is_error,omitempty"`
	DurationMillis int64  `json:"duration_ms,omitempty"`
}

// TurnUsage mirrors the LLM API's token-accounting block, kept tiny so
// the JSON file stays a fast disk read.
type TurnUsage struct {
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// TurnIndexEntry is the per-node directory's `index.json` row,
// providing cheap O(1) lookup of "what's the latest turn for this
// node?" without scanning the directory. Written in Phase 1 by the
// same observer that writes the TurnCheckpoint.
type TurnIndexEntry struct {
	LoopIter    int       `json:"loop_iter"`
	MaxTurn     int       `json:"max_turn"`
	LastWritten time.Time `json:"last_written"`
}
