package model

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
)

// nodeSessionStore stashes per-(runID, nodeID) message history so the
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

func sessionKey(runID, nodeID string) string {
	return runID + "\x00" + nodeID
}

// SaveSnapshot decodes a JSON-serialized [[]api.Message] snapshot and
// writes it under (runID, nodeID). Used by the claw runner sub-binary
// at startup to seed its local store from a [delegate.EnvelopeSessionReplay]
// snapshot (V2-4). Empty snapshots evict the entry.
func (s *nodeSessionStore) SaveSnapshot(runID, nodeID string, snapshot []byte) error {
	if s == nil {
		return nil
	}
	if len(snapshot) == 0 {
		s.save(runID, nodeID, nil)
		return nil
	}
	var messages []api.Message
	if err := json.Unmarshal(snapshot, &messages); err != nil {
		return err
	}
	s.save(runID, nodeID, messages)
	return nil
}

// LoadSnapshot returns the JSON-serialized snapshot for (runID,
// nodeID), or nil when no session exists. Used by the launcher's
// pre-spawn check (V2-4) to seed [delegate.EnvelopeSessionReplay].
func (s *nodeSessionStore) LoadSnapshot(runID, nodeID string) []byte {
	if s == nil {
		return nil
	}
	msgs := s.load(runID, nodeID)
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
func (s *nodeSessionStore) load(runID, nodeID string) []api.Message {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionKey(runID, nodeID)]
	if !ok || len(sess.messages) == 0 {
		return nil
	}
	out := make([]api.Message, len(sess.messages))
	copy(out, sess.messages)
	return out
}

// save replaces the stored messages for the given key. Passing a nil
// or empty slice is equivalent to evicting the session.
func (s *nodeSessionStore) save(runID, nodeID string, messages []api.Message) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionKey(runID, nodeID)
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
func (s *nodeSessionStore) evict(runID, nodeID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionKey(runID, nodeID))
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
func (s *nodeSessionStore) compact(runID, nodeID string, cfg clawrt.CompactionConfig) (removed int, fired bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionKey(runID, nodeID)]
	if !ok || len(sess.messages) == 0 {
		return 0, false
	}
	res := clawrt.CompactMessages(sess.messages, cfg)
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

// SessionCaptureSink mirrors per-(runID, nodeID) message snapshots out
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
	Capture(runID, nodeID string, snapshot []byte)
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
// (runID, nodeID) to opts.Messages. When no session is wired or empty,
// returns opts unchanged. Called by the claw backend before issuing
// GenerateTextDirect — the prior messages are how the LLM sees its
// own past tool calls and post-compaction summaries on retry.
func applySessionMessages(ctx context.Context, nodeID string, opts GenerationOptions) GenerationOptions {
	runID, store := runtimeContextFrom(ctx)
	if store == nil || runID == "" || nodeID == "" {
		return opts
	}
	prior := store.load(runID, nodeID)
	if len(prior) == 0 {
		return opts
	}
	merged := make([]api.Message, 0, len(prior)+len(opts.Messages))
	merged = append(merged, prior...)
	merged = append(merged, opts.Messages...)
	opts.Messages = merged
	return opts
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
func captureSessionMessages(ctx context.Context, nodeID string, result *TextResult) {
	if result == nil || len(result.Messages) == 0 {
		return
	}
	runID, store, sink := runtimeContextFull(ctx)
	if runID == "" || nodeID == "" {
		return
	}
	if store != nil {
		store.save(runID, nodeID, result.Messages)
	}
	if sink != nil {
		snapshot, err := json.Marshal(result.Messages)
		if err == nil {
			sink.Capture(runID, nodeID, snapshot)
		}
	}
}
