package mcp

import (
	"errors"
	"io"
	"sync"
	"time"
)

// BrowserSession represents a Chromium instance attached to a node
// during run execution. Sessions are per-(run, node, tool-call-id)
// scoped: when a Playwright MCP tool first issues a `browser_*` call
// from inside the runtime, the manager spawns Chromium and registers
// the resulting session here so the editor's WS proxy can dial in.
//
// CDPConn is an io.ReadWriteCloser carrying CDP wire frames. The
// production transport is a long-lived
// `chromium --remote-debugging-pipe` process driven via stdio; the
// pipe contract is one JSON-RPC message followed by a single `\0`
// byte in either direction. The ChromiumRunner abstraction lets
// tests substitute an in-memory ReadWriteCloser.
type BrowserSession struct {
	SessionID string
	RunID     string
	NodeID    string
	// CDPConn carries CDP JSON-RPC frames bidirectionally. The
	// runtime owns it; the editor's WS proxy calls Acquire/Release
	// rather than holding a raw reference, so the registry can
	// enforce single-consumer semantics if a future iterion
	// transport demands it.
	CDPConn   io.ReadWriteCloser
	StartedAt time.Time
}

// BrowserRegistry tracks active browser sessions per run. Lifecycle:
//
//   - Attach(): runtime calls when Playwright MCP spawns Chromium for
//     the first `browser_*` tool call of a node. Returns a session
//     with CDPConn already plumbed.
//   - Get(): editor WS proxy looks up by sessionID.
//   - List(): editor's "which sessions are active" UI poll.
//   - Detach(): runtime calls on tool-loop tear-down or run
//     finalisation; closes CDPConn.
//
// The default implementation is in-memory and process-local; cloud
// deployments where the editor and runtime live in different pods
// would front this with a Mongo-backed registry keyed by
// tenant + run + node.
type BrowserRegistry interface {
	Attach(session BrowserSession) error
	Get(runID, sessionID string) (BrowserSession, bool)
	ListByRun(runID string) []BrowserSession
	Detach(runID, sessionID string) error
}

// ErrSessionNotFound is returned by Detach when no session matches.
var ErrSessionNotFound = errors.New("browser registry: session not found")

// ErrSessionAlreadyAttached is returned by Attach when the same
// session ID is registered twice. Rare in practice (session IDs are
// generated per (run, tool-call-id)), but worth catching: the
// duplicate would otherwise overwrite a live session and orphan its
// CDPConn.
var ErrSessionAlreadyAttached = errors.New("browser registry: session already attached")

// memoryBrowserRegistry is the default in-memory implementation.
// Safe for concurrent use; sessions are keyed (run_id, session_id).
type memoryBrowserRegistry struct {
	mu       sync.RWMutex
	sessions map[string]map[string]BrowserSession // run_id → session_id → session
}

// NewMemoryBrowserRegistry returns the default in-memory registry.
// Wire it into pkg/runview at server-construction time and let the
// MCP manager + WS proxy share the same instance.
func NewMemoryBrowserRegistry() BrowserRegistry {
	return &memoryBrowserRegistry{
		sessions: make(map[string]map[string]BrowserSession),
	}
}

func (r *memoryBrowserRegistry) Attach(session BrowserSession) error {
	if session.SessionID == "" || session.RunID == "" {
		return errors.New("browser registry: session and run id required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket, ok := r.sessions[session.RunID]
	if !ok {
		bucket = make(map[string]BrowserSession)
		r.sessions[session.RunID] = bucket
	}
	if _, dup := bucket[session.SessionID]; dup {
		return ErrSessionAlreadyAttached
	}
	if session.StartedAt.IsZero() {
		session.StartedAt = time.Now().UTC()
	}
	bucket[session.SessionID] = session
	return nil
}

func (r *memoryBrowserRegistry) Get(runID, sessionID string) (BrowserSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bucket, ok := r.sessions[runID]
	if !ok {
		return BrowserSession{}, false
	}
	s, ok := bucket[sessionID]
	return s, ok
}

func (r *memoryBrowserRegistry) ListByRun(runID string) []BrowserSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bucket := r.sessions[runID]
	if len(bucket) == 0 {
		return nil
	}
	out := make([]BrowserSession, 0, len(bucket))
	for _, s := range bucket {
		out = append(out, s)
	}
	return out
}

func (r *memoryBrowserRegistry) Detach(runID, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket, ok := r.sessions[runID]
	if !ok {
		return ErrSessionNotFound
	}
	s, ok := bucket[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	delete(bucket, sessionID)
	if len(bucket) == 0 {
		delete(r.sessions, runID)
	}
	if s.CDPConn != nil {
		_ = s.CDPConn.Close()
	}
	return nil
}

// ChromiumRunner is the abstraction over "how do we spawn a Chromium
// the runtime can drive over CDP". The host runner uses a direct
// `chromium --remote-debugging-pipe` process; a Docker variant
// would shell out to `docker exec` against the run's sandbox
// container.
type ChromiumRunner interface {
	// Start launches a Chromium attached to the given run and
	// returns a CDP wire-protocol pipe. The runtime is responsible
	// for calling Close on the pipe when the session ends.
	Start(runID, nodeID string) (io.ReadWriteCloser, error)
}

// ErrChromiumNotImplemented is returned by the stub runner. Wired
// when no real runner is configured so the WS proxy surfaces a
// clean 503 to the editor instead of crashing.
var ErrChromiumNotImplemented = errors.New("browser: chromium runner not yet implemented")

// stubChromiumRunner returns ErrChromiumNotImplemented unconditionally.
type stubChromiumRunner struct{}

// NewStubChromiumRunner returns a runner that always errors.
func NewStubChromiumRunner() ChromiumRunner {
	return stubChromiumRunner{}
}

func (stubChromiumRunner) Start(_, _ string) (io.ReadWriteCloser, error) {
	return nil, ErrChromiumNotImplemented
}
