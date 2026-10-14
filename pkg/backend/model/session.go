package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func clawSessionFingerprint(model string) string {
	provider, _, err := ParseModelSpec(model)
	if err != nil || strings.TrimSpace(provider) == "" {
		return "claw:unknown"
	}
	return "claw:" + strings.ToLower(strings.TrimSpace(provider))
}

// A Claw fingerprint identifies a provider, not a provider-owned CLI session.
// Only recognized fingerprints can share the provider-neutral persisted slot.
func knownClawSessionFingerprint(fingerprint string) bool {
	provider, ok := strings.CutPrefix(fingerprint, "claw:")
	return ok && provider != "" && provider != "unknown" && !strings.ContainsAny(provider, ":/ \t\r\n")
}

// Persistent Claw pauses can resume on another provider. Keep the exact
// operator response and pending call, but remove provider-specific reasoning
// and unrelated incomplete tool calls before the backend appends the result.
func sanitizeClawPersistResume(task *delegate.Task) error {
	if len(task.ResumeConversation) == 0 {
		return nil
	}
	var messages []api.Message
	if err := json.Unmarshal(task.ResumeConversation, &messages); err != nil {
		return fmt.Errorf("claw persisted resume: decode conversation: %w", err)
	}
	pending := task.ResumePendingToolUseID
	uses, results := 0, 0
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" && block.ID == pending {
				if message.Role != "assistant" || block.Name == "" {
					return fmt.Errorf("claw persisted resume: invalid pending tool call")
				}
				uses++
			}
			if block.Type == "tool_result" && block.ToolUseID == pending {
				results++
			}
		}
	}
	if pending == "" || uses != 1 || results != 0 {
		return fmt.Errorf("claw persisted resume: expected exactly one unanswered pending tool call")
	}
	messages, _ = sanitizeToolPairs(messages, map[string]struct{}{pending: {}}, true)
	var err error
	task.ResumeConversation, err = json.Marshal(messages)
	return err
}

// taskSessionKey is the store address of a task's conversation: its explicit
// session_slot when the node declares one, else the node id. The in-process
// backend, the host's mirror of a sandboxed crossing's capture and the
// runner's own local store all address it through this one rule, so a slot is
// ONE conversation whichever side of the boundary a crossing runs on.
func taskSessionKey(task delegate.Task) string {
	if task.SessionSlot != "" {
		return task.SessionSlot
	}
	return task.NodeID
}

// TaskSessionKey exposes [taskSessionKey] to the claw runner sub-binary,
// which lives in `cmd/iterion` and seeds its local store under the same key
// the in-container backend reads.
func TaskSessionKey(task delegate.Task) string { return taskSessionKey(task) }

// nodeSessionKey is taskSessionKey addressed from the workflow instead of a
// built task: the explicit session_slot of a persisted LLM node, else the
// node id. It is the key for the paths that hold a node id and no task — the
// recovery dispatcher's compaction, the executor's post-success eviction.
func nodeSessionKey(node ir.Node) string {
	if llm, ok := node.(ir.LLMNode); ok && llm.GetSession() == ir.SessionPersist && llm.GetSessionSlot() != "" {
		return llm.GetSessionSlot()
	}
	return node.NodeID()
}

// clawSessionWireBudget bounds a session snapshot crossing the sandbox IPC:
// the channel's per-line cap, less room for the envelope's own JSON wrapper
// and its newline.
const clawSessionWireBudget = delegate.MaxEnvelopeLineBytes - 1024

// constrainSessionSnapshotForWire bounds a JSON-serialized [[]api.Message]
// snapshot so it crosses the sandbox IPC as ONE NDJSON line. The store it
// comes from is uncapped — an in-process crossing on the same session key
// fills it, and a tool result alone may carry [delegate.MaxToolResultBytes]
// — while the channel's reader fails the WHOLE channel on a longer line, so
// its producers clamp first (see [delegate.MaxEnvelopeLineBytes]).
//
// Over the budget the conversation is squeezed, keeping as many recent turns
// as fit and collapsing to a protocol-safe continuation summary when none
// do, exactly as an oversized persist replay is. reduced says the caller
// holds something smaller than it asked for. A snapshot still over the
// budget is REFUSED with its size — never written, never silently dropped —
// and the refusal says which of the two it is: compacted and still too long,
// or holding nothing the compactor can summarize.
func constrainSessionSnapshotForWire(snapshot []byte) (json.RawMessage, bool, error) {
	if len(snapshot) <= clawSessionWireBudget {
		return snapshot, false, nil
	}
	var messages []api.Message
	if err := json.Unmarshal(snapshot, &messages); err != nil {
		return nil, false, fmt.Errorf("decode a %d-byte session snapshot: %w", len(snapshot), err)
	}
	// The budget is in BYTES, so the token threshold is set to fire always
	// and the recent window is what shrinks: token estimates undercount a
	// short transcript made of very large tool results.
	compacted := false
	for _, recent := range []int{clawPersistRecent, 1, 0} {
		res := compactMessagesToolSafe(messages, clawrt.CompactionConfig{
			PreserveRecentMessages: recent,
			MaxEstimatedTokens:     1,
		}, nil)
		if res == nil {
			continue
		}
		compacted = true
		raw, err := json.Marshal(res.CompactedMessages)
		if err != nil {
			return nil, false, fmt.Errorf("encode a compacted session snapshot: %w", err)
		}
		if len(raw) <= clawSessionWireBudget {
			return raw, true, nil
		}
	}
	// The two refusals are not the same fact, and an operator reading one
	// needs to know which: a conversation the compactor DID squeeze and that
	// is still too long, or one it had nothing to squeeze — a snapshot
	// already reduced to its own continuation summary is re-emitted whole by
	// every window, never shrunk.
	if !compacted {
		return nil, false, fmt.Errorf("a %d-byte session snapshot of %d messages is over the %d-byte channel budget and holds nothing the compactor can summarize",
			len(snapshot), len(messages), clawSessionWireBudget)
	}
	return nil, false, fmt.Errorf("a %d-byte session snapshot of %d messages stays over the %d-byte channel budget once compacted",
		len(snapshot), len(messages), clawSessionWireBudget)
}

// ConstrainSessionSnapshotForWire exposes [constrainSessionSnapshotForWire]
// to the claw runner sub-binary, which lives in `cmd/iterion` and is the
// other producer of a session snapshot on that channel.
func ConstrainSessionSnapshotForWire(snapshot []byte) (json.RawMessage, bool, error) {
	return constrainSessionSnapshotForWire(snapshot)
}

// sessionKeyOf resolves a node id to its session key through the workflow.
// A node the workflow does not declare keys on its own id: the store then
// holds nothing under it, and the compaction the caller was asked for is
// reported unsupported rather than run against a key nothing writes.
func (e *ClawExecutor) sessionKeyOf(nodeID string) string {
	if node, ok := e.wfNodes[nodeID]; ok {
		return nodeSessionKey(node)
	}
	return nodeID
}

// nodeSessionStore stashes per-(runID, session key) message history so the
// recovery dispatcher's CompactAndRetry action has something concrete
// to shrink before the next attempt. Sessions are evicted when a node
// succeeds (per-node) and when a run terminates (per-run).

type nodeSession struct {
	messages []api.Message
}

type nodeSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*nodeSession
}

func newNodeSessionStore() *nodeSessionStore {
	return &nodeSessionStore{sessions: map[string]*nodeSession{}}
}

func sessionKey(runID, key string) string {
	return runID + "\x00" + key
}

// SaveSnapshot decodes a JSON-serialized [[]api.Message] snapshot and
// writes it under (runID, session key). Used by the claw runner sub-binary
// at startup to seed its local store from a [delegate.EnvelopeSessionReplay]
// snapshot (V2-4). Empty snapshots evict the entry.
func (s *nodeSessionStore) SaveSnapshot(runID, key string, snapshot []byte) error {
	if s == nil {
		return nil
	}
	if len(snapshot) == 0 {
		s.save(runID, key, nil)
		return nil
	}
	var messages []api.Message
	if err := json.Unmarshal(snapshot, &messages); err != nil {
		return err
	}
	s.save(runID, key, messages)
	return nil
}

// LoadSnapshot returns the JSON-serialized snapshot for (runID,
// session key), or nil when no session exists. Used by the launcher's
// pre-spawn check (V2-4) to seed [delegate.EnvelopeSessionReplay].
func (s *nodeSessionStore) LoadSnapshot(runID, key string) []byte {
	if s == nil {
		return nil
	}
	msgs := s.load(runID, key)
	if len(msgs) == 0 {
		return nil
	}
	buf, err := json.Marshal(msgs)
	if err != nil {
		return nil
	}
	return buf
}

// load returns a copy of the stored messages for the given key, or nil
// if no session exists. The returned slice is owned by the caller
// (defensive copy) so the caller can mutate it without holding the
// store mutex.
func (s *nodeSessionStore) load(runID, sessKey string) []api.Message {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionKey(runID, sessKey)]
	if !ok || len(sess.messages) == 0 {
		return nil
	}
	out := make([]api.Message, len(sess.messages))
	copy(out, sess.messages)
	return out
}

// save replaces the stored messages for the given key. Passing a nil
// or empty slice is equivalent to evicting the session.
func (s *nodeSessionStore) save(runID, sessKey string, messages []api.Message) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionKey(runID, sessKey)
	if len(messages) == 0 {
		delete(s.sessions, key)
		return
	}
	dup := make([]api.Message, len(messages))
	copy(dup, messages)
	s.sessions[key] = &nodeSession{messages: dup}
}

// evict drops the session for the given key, regardless of whether it
// existed. Called by the executor when a node finishes (success or
// terminal failure) so retries on later runs do not pick up stale
// state.
func (s *nodeSessionStore) evict(runID, key string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionKey(runID, key))
}

// evictRun drops every session belonging to the given runID. The
// engine calls this when a run terminates (success, terminal failure,
// or cancellation) so per-node sessions left behind by failed nodes
// do not leak across runs that share the same executor.
func (s *nodeSessionStore) evictRun(runID string) {
	if s == nil || runID == "" {
		return
	}
	prefix := runID + "\x00"
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.sessions {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(s.sessions, k)
		}
	}
}

// compact applies the pure-function compactor from claw-code-go to
// the stored session. Returns the number of messages removed and a
// bool indicating whether compaction actually fired (it does not when
// the session is short or absent).
func (s *nodeSessionStore) compact(runID, key string, cfg clawrt.CompactionConfig) (removed int, fired bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionKey(runID, key)]
	if !ok || len(sess.messages) == 0 {
		return 0, false
	}
	res := compactMessagesToolSafe(sess.messages, cfg, nil)
	if res == nil {
		return 0, false
	}
	sess.messages = res.CompactedMessages
	return res.RemovedMessageCount, true
}

// ---------------------------------------------------------------------------
// Context plumbing
// ---------------------------------------------------------------------------

// runIDOnlyKey is the ctx key used by the runtime engine to thread
// runID into executor.Execute. It is the lighter of the two keys —
// the ClawExecutor reads it to learn which run is in progress (so its
// Compact method can locate the right session) and re-wraps ctx with
// the richer runtimeContextKey for downstream backends.
type runIDOnlyKey struct{}

// WithRunID returns a derived ctx carrying the run ID. The runtime
// engine calls this once per node execution, before
// `executor.Execute`. ClawExecutor reads it via RunIDFromContext.
func WithRunID(ctx context.Context, runID string) context.Context {
	if runID == "" {
		return ctx
	}
	return context.WithValue(ctx, runIDOnlyKey{}, runID)
}

// RunIDFromContext returns the run ID set by WithRunID, or "" when
// none is wired (e.g. unit tests that exercise the executor without
// the runtime engine).
func RunIDFromContext(ctx context.Context) string {
	s, _ := ctx.Value(runIDOnlyKey{}).(string)
	return s
}

// nodeIDOnlyKey carries the currently-executing node id so the
// operator-inbox binder can node-scope message delivery (a supervisor
// that watches one node tags its messages with that node; the drain
// only releases a tagged message while that node is the active one).
type nodeIDOnlyKey struct{}

// WithNodeID returns a derived ctx carrying the active node id. The
// runtime engine calls this once per node execution, alongside
// WithRunID. Empty nodeID is a no-op (drain falls back to run-scope).
func WithNodeID(ctx context.Context, nodeID string) context.Context {
	if nodeID == "" {
		return ctx
	}
	return context.WithValue(ctx, nodeIDOnlyKey{}, nodeID)
}

// NodeIDFromContext returns the active node id set by WithNodeID, or
// "" when none is wired. "" means "run-scoped drain" — every queued
// message is delivered regardless of its NodeID tag (legacy behaviour).
func NodeIDFromContext(ctx context.Context) string {
	s, _ := ctx.Value(nodeIDOnlyKey{}).(string)
	return s
}

// runtimeContextKey is the richer ctx key set by ClawExecutor before
// it calls a backend. The backend reads (runID, store) and maintains
// the per-node session.
type runtimeContextKey struct{}

type runtimeContext struct {
	runID string
	store *nodeSessionStore
	// sink, when non-nil, fires whenever captureSessionMessages saves
	// a fresh per-node session — it relays the snapshot to a side
	// channel for V2-4's mirror-runner-store-back-to-host wiring.
	sink SessionCaptureSink
}

// SessionCaptureSink mirrors per-(runID, session key) message snapshots out
// of the local store into a side channel as they are saved. V2-4 uses
// it on the runner side: the sink emits [delegate.EnvelopeSessionCapture]
// envelopes so the launcher's host nodeSessionStore stays in sync with
// the runner's, enabling CompactAndRetry to compact the LATEST history
// rather than the last-known-good one. The host side leaves this nil
// — it owns the canonical store directly.
type SessionCaptureSink interface {
	// Capture is called after the local store has saved the snapshot.
	// Implementations should not block — the caller is on the LLM
	// loop's hot path.
	Capture(runID, sessKey string, snapshot []byte)
}

// withRuntimeContext returns a derived ctx carrying the runID and
// session store. The runtime engine calls this before invoking
// executor.Execute. A nil store disables session tracking.
func withRuntimeContext(ctx context.Context, runID string, store *nodeSessionStore) context.Context {
	if runID == "" && store == nil {
		return ctx
	}
	return context.WithValue(ctx, runtimeContextKey{}, runtimeContext{runID: runID, store: store})
}

// WithSandboxRunnerSession is the runner-side entry point (V2-4) that
// hooks a freshly-built session store + capture sink into ctx so the
// in-runner ClawBackend mirrors every saved snapshot back across the
// IPC. The runner constructs the store via [NewNodeSessionStore] and
// the sink as a thin wrapper over the [delegate.EnvelopeWriter].
//
// Exported (where [withRuntimeContext] stays unexported) because the
// runner lives in `cmd/iterion` which can't reach package-private
// helpers — and we don't want every executor caller to discover a
// "sink" parameter they'd have to thread nil through.
func WithSandboxRunnerSession(ctx context.Context, runID string, store *NodeSessionStore, sink SessionCaptureSink) context.Context {
	return context.WithValue(ctx, runtimeContextKey{}, runtimeContext{runID: runID, store: store, sink: sink})
}

type launcherEndsNodeSessionKey struct{}

// WithLauncherEndedNodeSession marks ctx as a sandboxed runner's whose
// launcher wrapped it in the node's session and ends that session when the
// node ends (delegate.IOTask.LauncherEndsNodeSession): Execute then draws no
// session of its own, and what the node starts is stopped and reported by the
// launcher's sweep.
func WithLauncherEndedNodeSession(ctx context.Context) context.Context {
	return context.WithValue(ctx, launcherEndsNodeSessionKey{}, true)
}

func launcherEndsNodeSession(ctx context.Context) bool {
	ended, _ := ctx.Value(launcherEndsNodeSessionKey{}).(bool)
	return ended
}

// runtimeContextFrom extracts the runID + session store from ctx. The
// returned store is nil when no executor wired one in.
func runtimeContextFrom(ctx context.Context) (string, *nodeSessionStore) {
	rc, _ := ctx.Value(runtimeContextKey{}).(runtimeContext)
	return rc.runID, rc.store
}

// runtimeContextFull returns the (runID, store, sink) triple. Used by
// captureSessionMessages to fire the sink in addition to saving
// locally.
func runtimeContextFull(ctx context.Context) (string, *nodeSessionStore, SessionCaptureSink) {
	rc, _ := ctx.Value(runtimeContextKey{}).(runtimeContext)
	return rc.runID, rc.store, rc.sink
}

// NodeSessionStore is the public alias the runner uses to construct a
// store (the unexported alias preserves the in-package call-sites).
type NodeSessionStore = nodeSessionStore

// NewNodeSessionStore exposes the internal store constructor for the
// claw runner sub-binary (V2-4).
func NewNodeSessionStore() *NodeSessionStore { return newNodeSessionStore() }

// applySessionMessages prepends any prior-attempt messages stored for
// (runID, session key) to opts.Messages. When no session is wired or empty,
// returns opts unchanged. Called by the claw backend before issuing
// GenerateTextDirect — the prior messages are how the LLM sees its
// own past tool calls and post-compaction summaries on retry.
func applySessionMessages(ctx context.Context, sessKey string, opts GenerationOptions) GenerationOptions {
	runID, store := runtimeContextFrom(ctx)
	if store == nil || runID == "" || sessKey == "" {
		return opts
	}
	prior := store.load(runID, sessKey)
	if len(prior) == 0 {
		return opts
	}
	merged := make([]api.Message, 0, len(prior)+len(opts.Messages))
	merged = append(merged, prior...)
	merged = append(merged, opts.Messages...)
	opts.Messages = merged
	return opts
}

// applySessionMessagesForTask is applySessionMessages keyed by the task —
// unless the task already carries its history explicitly: a paused
// conversation being resumed (ResumeConversation) or a completed one being
// continued (ContinueConversation). Both replay the very messages the store
// holds, so prepending the store on top duplicated the history — measured
// on a resumed node: its transient retries grew 11 → 22 → 33 messages, and
// its schema retry replayed 91 stored messages followed by the 11 it was
// resuming from.
func applySessionMessagesForTask(ctx context.Context, task delegate.Task, opts GenerationOptions) GenerationOptions {
	if len(task.ResumeConversation) > 0 || len(task.ContinueConversation) > 0 {
		return opts
	}
	return applySessionMessages(ctx, taskSessionKey(task), opts)
}

// captureSessionMessages writes back the final accumulated messages
// from a completed generation so the next retry of the same node can
// resume from there. A nil result (failed call) is a no-op so we
// don't trample any prior session state — meaning compaction can
// run against the still-stored last-good state.
//
// V2-4: when a [SessionCaptureSink] is wired into ctx (the runner-side
// path), this also fires the sink with a JSON-serialized snapshot so
// the host's nodeSessionStore can mirror the in-runner state.
func captureSessionMessages(ctx context.Context, sessKey string, result *TextResult) {
	if result == nil || len(result.Messages) == 0 {
		return
	}
	runID, store, sink := runtimeContextFull(ctx)
	if runID == "" || sessKey == "" {
		return
	}
	if store != nil {
		store.save(runID, sessKey, result.Messages)
	}
	if sink != nil {
		snapshot, err := json.Marshal(result.Messages)
		if err == nil {
			sink.Capture(runID, sessKey, snapshot)
		}
	}
}

// sessionLedgerKey carries the run's delegate.SessionLedger: the record of
// the background work a CLI session's processes lost, keyed by the session id
// so every task that resumes the transcript reads the same entry.
type sessionLedgerKey struct{}

// WithSessionLedger returns a derived ctx carrying the run's session ledger.
// The runtime engine sets it for every node execution, trunk and branches
// alike: its entries are keyed by session id, which no two items share.
func WithSessionLedger(ctx context.Context, l delegate.SessionLedger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionLedgerKey{}, l)
}

// SessionLedgerFromContext returns the ledger set by WithSessionLedger, or
// nil when none is wired (a task then neither reads nor records one).
func SessionLedgerFromContext(ctx context.Context) delegate.SessionLedger {
	l, _ := ctx.Value(sessionLedgerKey{}).(delegate.SessionLedger)
	return l
}
