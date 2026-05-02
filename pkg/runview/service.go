package runview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// LaunchSpec describes a workflow invocation. Mirrors the inputs of
// `iterion run` but framed as data so HTTP handlers (and any future
// programmatic caller) construct it without going through cobra flags.
type LaunchSpec struct {
	FilePath string            // absolute .iter path; sandbox check is the caller's job
	Vars     map[string]string // --var-style overrides
	RunID    string            // optional explicit ID; auto-generated when empty
	Timeout  time.Duration     // 0 disables
	// MergeInto controls the worktree-finalization fast-forward target
	// for `worktree: auto` runs. "" or "current" → FF the user's
	// currently-checked-out branch (default); "none" → skip FF;
	// <branch-name> → FF that branch (only honoured when it matches
	// the currently-checked-out branch).
	MergeInto string
	// BranchName overrides the default storage branch
	// `iterion/run/<friendly>` created on the worktree's HEAD.
	BranchName string
}

// ResumeSpec describes a resume request.
type ResumeSpec struct {
	RunID    string
	FilePath string                 // .iter file (loaded fresh; must match the run's WorkflowHash unless Force)
	Answers  map[string]interface{} // answers for human nodes; ignored for failed_resumable
	Force    bool                   // skip workflow hash check
	Timeout  time.Duration          // 0 disables
}

// RunSummary is the lightweight per-row shape returned by List.
// Heavier fields (events, artifacts, checkpoint detail) live in
// RunSnapshot — call Snapshot for the full view.
type RunSummary struct {
	ID string `json:"id"`
	// Name is the deterministic, human-friendly label for the run.
	// Empty for legacy runs persisted before this field existed.
	Name         string          `json:"name,omitempty"`
	WorkflowName string          `json:"workflow_name"`
	Status       store.RunStatus `json:"status"`
	FilePath     string          `json:"file_path,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	Error        string          `json:"error,omitempty"`
	// Active reports whether the run is currently held by this
	// process's manager. A run with status "running" but Active=false
	// belongs to another process or to a previous boot — Cancel won't
	// reach it from here.
	Active bool `json:"active"`
	// Worktree finalization summary (only populated for `worktree:
	// auto` runs that reached a clean exit). See store.Run for the
	// full semantics.
	FinalCommit string `json:"final_commit,omitempty"`
	FinalBranch string `json:"final_branch,omitempty"`
	MergedInto  string `json:"merged_into,omitempty"`
}

// ListFilter scopes a List request. Empty fields mean no filter.
type ListFilter struct {
	Status   store.RunStatus // exact match
	Workflow string          // exact match on WorkflowName
	Since    time.Time       // UpdatedAt >= Since
	Limit    int             // 0 = no limit
	// Node filters runs to those whose persisted events include at
	// least one node_started for this IR node ID. Used by the editor
	// to surface "this node was touched by N runs" without scanning
	// every run on the client. Scanning happens at request time —
	// fine for hundreds of runs; wire an inverted index later if the
	// store grows past low thousands.
	Node string
}

// ArtifactSummary is the lightweight shape returned by ListArtifacts —
// just enough for the UI to populate a version selector without
// reading every artifact body.
type ArtifactSummary struct {
	Version   int       `json:"version"`
	WrittenAt time.Time `json:"written_at"`
}

// Service is the canonical façade over runtime + store + broker +
// manager. The HTTP server, the editor, and (optionally) the CLI all
// route through here — keeping a single source of truth for run
// lifecycle, validation, and event fan-out.
type Service struct {
	store    *store.RunStore
	storeDir string
	logger   *iterlog.Logger
	broker   *EventBroker
	manager  *Manager

	// recoveryDispatch is built once on construction so each Launch /
	// Resume reuses the same dispatcher rather than allocating a new
	// recipes map + closure on the per-run hot path.
	recoveryDispatch runtime.RecoveryDispatch

	// extraObservers are runtime EventObservers chained alongside
	// the broker fan-out. Used to attach Prometheus / OTLP / custom
	// observers when constructing a server-side service.
	extraObservers []func(store.Event)

	// wireWFCache memoises WireWorkflow projections by (filePath, hash)
	// so /api/runs/{id}/workflow doesn't re-parse + re-compile on every
	// request. Invalidated implicitly when the .iter source changes
	// (hash mismatch). See workflow_export.go.
	wireWFCache wireWorkflowCache

	// runLogs holds a per-run log buffer for the lifetime of each
	// in-process run. Created in spawnRun, removed when the run
	// goroutine exits. The buffer captures the iterion logger output
	// scoped to that run and fans it out to live WS subscribers; see
	// runlog.go and the /api/runs/{id}/log endpoint.
	runLogsMu sync.RWMutex
	runLogs   map[string]*RunLogBuffer

	// draining is set by Drain at the start of graceful shutdown.
	// Once true, Launch and Resume early-return runtime.ErrServerDraining
	// so the HTTP layer can map it to 503 Service Unavailable.
	draining atomic.Bool
}

// ServiceOption configures a Service at construction time.
type ServiceOption func(*Service)

// WithLogger sets the logger used for service-level diagnostics.
func WithLogger(l *iterlog.Logger) ServiceOption {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithBroker injects an existing event broker. When omitted, the
// service creates its own.
func WithBroker(b *EventBroker) ServiceOption {
	return func(s *Service) {
		if b != nil {
			s.broker = b
		}
	}
}

// WithManager injects an existing lifecycle manager. When omitted,
// the service creates its own.
func WithManager(m *Manager) ServiceOption {
	return func(s *Service) {
		if m != nil {
			s.manager = m
		}
	}
}

// WithExtraEventObservers adds observers chained alongside the
// broker.Publish observer. Use this to wire Prometheus / OTLP
// exporters into the HTTP service's run goroutines.
func WithExtraEventObservers(observers ...func(store.Event)) ServiceOption {
	return func(s *Service) { s.extraObservers = append(s.extraObservers, observers...) }
}

// NewService constructs a Service rooted at storeDir.
func NewService(storeDir string, opts ...ServiceOption) (*Service, error) {
	if storeDir == "" {
		storeDir = ".iterion"
	}
	logger := iterlog.New(iterlog.LevelInfo, os.Stderr)
	st, err := store.New(storeDir, store.WithLogger(logger))
	if err != nil {
		return nil, fmt.Errorf("runview: open store: %w", err)
	}
	s := &Service{
		store:            st,
		storeDir:         storeDir,
		logger:           logger,
		broker:           NewEventBroker(),
		manager:          NewManager(),
		recoveryDispatch: recovery.Dispatch(recovery.DefaultRecipes()),
		runLogs:          make(map[string]*RunLogBuffer),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.reconcileOrphans()
	return s, nil
}

// reconcileOrphans flips runs whose status is "running" but whose
// owning process is gone (lock released by the OS) to a terminal
// status. Without this, every server restart leaves the editor's
// run list polluted with stale "running" rows from CLI invocations
// that exited (cleanly or otherwise) without persisting a final
// status — flock(2) is auto-released on crash, but the engine's
// status writer is not.
//
// Logic per orphan:
//   - has Checkpoint  → failed_resumable (user can iterion resume)
//   - no Checkpoint   → failed           (no recovery point; restart)
//
// We use the lock as the liveness probe: a non-blocking flock that
// succeeds proves no other process holds the run. Held runs are left
// untouched, so a second iterion instance running in the same store
// dir cannot clobber the first instance's in-flight work.
func (s *Service) reconcileOrphans() {
	ids, err := s.store.ListRuns()
	if err != nil {
		s.logger.Warn("runview: reconcile: list runs: %v", err)
		return
	}
	for _, id := range ids {
		r, err := s.store.LoadRun(id)
		if err != nil {
			continue
		}
		if r.Status != store.RunStatusRunning {
			continue
		}
		// .pid present + PID alive → runner outlived the previous
		// server lifetime; re-attach. Stale .pid → remove and fall
		// through to the flock probe. Missing .pid → in-process or
		// older run; flock probe applies.
		if s.tryReattachByPID(id) {
			continue
		}
		// Try to grab the lock; non-blocking semantics mean we
		// either own it instantly (orphan) or fail fast (live).
		lock, err := s.store.LockRun(id)
		if err != nil {
			continue
		}
		// Re-load under the lock — another process could have
		// just released between ListRuns and LockRun and updated
		// the status to a terminal state.
		r2, err := s.store.LoadRun(id)
		if err != nil || r2.Status != store.RunStatusRunning {
			_ = lock.Unlock()
			continue
		}
		newStatus := store.RunStatusFailed
		if r2.Checkpoint != nil {
			newStatus = store.RunStatusFailedResumable
		}
		if err := s.store.UpdateRunStatus(id, newStatus, "process orphaned: server restart found run in 'running' state"); err != nil {
			s.logger.Warn("runview: reconcile %s: %v", id, err)
		} else {
			s.logger.Info("runview: reconciled orphan run %s → %s", id, newStatus)
		}
		_ = lock.Unlock()
	}
}

// tryReattachByPID handles the .pid path of reconcileOrphans. Returns
// true if the run was re-attached (caller should skip the orphan
// reconcile). Removes a stale .pid as a side effect so the next
// reconcile cycle doesn't false-positive on it.
func (s *Service) tryReattachByPID(runID string) bool {
	pid, err := s.store.ReadPIDFile(runID)
	if err != nil || pid <= 0 {
		return false
	}
	if pidAlive(pid) == nil {
		s.reattachDetached(runID, pid)
		return true
	}
	_ = s.store.RemovePIDFile(runID)
	return false
}

// reattachDetached re-establishes the editor server's view of a
// detached runner that survived a previous server lifetime. It
// installs an in-memory log buffer (so WS subscribers can stream
// live), starts the file-based event + log tailers, and registers a
// manager handle whose Cancel signals the runner's process group and
// whose done channel is closed by a watcher goroutine that polls for
// process exit.
//
// We can't cmd.Wait() on the runner here — we are not its parent —
// so liveness is inferred via kill -0 polling at 1s cadence. That
// resolution is fine: the runner can take seconds to reach its
// shutdown checkpoints anyway, and the watcher's only consumer is
// Drain (timing-sensitive) and the broker.CloseRun call (post-mortem).
func (s *Service) reattachDetached(runID string, pid int) {
	s.prepareRunLogNoFile(runID)

	done := make(chan struct{})
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
				s.logger.Warn("runview: reattach: signal pgrp %d: %v", pid, err)
			}
		})
	}

	if err := s.manager.RegisterDetached(runID, pid, cancel, done); err != nil {
		s.logger.Warn("runview: reattach: register %s pid=%d: %v", runID, pid, err)
		return
	}

	go func() {
		watchDetachedExit(s, runID, pid, done)
	}()

	startEventSource(s, runID, done)
	startLogSource(s, runID, done)

	s.logger.Info("runview: re-attached detached run %s (pid=%d) across server restart", runID, pid)
}

// watchDetachedExit polls kill(0) on pid until the process exits,
// then performs the same cleanup spawnDetached's cmd.Wait goroutine
// would: clean up the .pid file, close subscriptions, and Deregister
// the handle (which closes done). Used only on the re-attach path
// where we don't own the cmd. 5s cadence is fine because runners
// typically run for minutes; a faster probe would just burn syscalls.
func watchDetachedExit(s *Service, runID string, pid int, done chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if err := pidAlive(pid); err != nil {
				_ = s.store.RemovePIDFile(runID)
				s.broker.CloseRun(runID)
				s.dropRunLog(runID)
				s.manager.Deregister(runID)
				return
			}
		}
	}
}

// Broker exposes the event broker for transports that need to
// subscribe directly (the WS handler).
func (s *Service) Broker() *EventBroker { return s.broker }

// StoreDir returns the on-disk store directory. Exposed so HTTP
// handlers can fall back to persisted run.log when the in-memory
// buffer is gone.
func (s *Service) StoreDir() string { return s.storeDir }

// GetLogBuffer returns the live log buffer for runID, or nil if the
// run is not held by this process. Valid only while the run is
// active; the buffer is Close'd and removed when the run goroutine
// exits.
func (s *Service) GetLogBuffer(runID string) *RunLogBuffer {
	s.runLogsMu.RLock()
	defer s.runLogsMu.RUnlock()
	return s.runLogs[runID]
}

// prepareRunLog creates a per-run log buffer (also persisting to
// <store-dir>/runs/<runID>/run.log when the store dir is writable)
// and wraps the service's writer + buffer into a per-run logger.
// Returns the buffer for cleanup and the logger to thread through
// both BuildExecutor and runtime.WithLogger so every iterion log line
// emitted during this run is captured for the WS subscribers.
func (s *Service) prepareRunLog(runID string) (*RunLogBuffer, *iterlog.Logger) {
	var filePath string
	if s.storeDir != "" {
		runDir := filepath.Join(s.storeDir, "runs", runID)
		if err := os.MkdirAll(runDir, 0o755); err == nil {
			filePath = filepath.Join(runDir, "run.log")
		}
	}
	buf, fileErr := NewRunLogBuffer(filePath)
	if fileErr != nil {
		s.logger.Warn("runview: open run.log for %s: %v — proceeding without disk persistence", runID, fileErr)
	}

	s.runLogsMu.Lock()
	if old, ok := s.runLogs[runID]; ok {
		// Defensive: a previous run goroutine for this ID didn't
		// fully clean up. The store lock should make this impossible,
		// but if it ever happens we want the WS subscribers of the
		// stale buffer to see EOF rather than dangle forever.
		old.Close()
	}
	s.runLogs[runID] = buf
	s.runLogsMu.Unlock()

	perRunLogger := iterlog.New(s.logger.Level(), io.MultiWriter(s.logger.Writer(), buf))
	return buf, perRunLogger
}

// prepareRunLogNoFile is the detached-mode counterpart to
// prepareRunLog: it installs an in-memory-only buffer for runID
// (no file tee) and does NOT return a logger. The runner subprocess
// owns the on-disk run.log; a second writer here would corrupt it.
// File contents reach this buffer via the file_log_source tailer,
// which reads new bytes off disk and pushes them through Write.
func (s *Service) prepareRunLogNoFile(runID string) *RunLogBuffer {
	buf, _ := NewRunLogBuffer("")
	s.runLogsMu.Lock()
	if old, ok := s.runLogs[runID]; ok {
		old.Close()
	}
	s.runLogs[runID] = buf
	s.runLogsMu.Unlock()
	return buf
}

// dropRunLog tears down the per-run buffer at run-completion time:
// closes any active subscribers, the persisted file, and removes the
// map entry. Idempotent.
func (s *Service) dropRunLog(runID string) {
	s.runLogsMu.Lock()
	buf := s.runLogs[runID]
	delete(s.runLogs, runID)
	s.runLogsMu.Unlock()
	if buf != nil {
		buf.Close()
	}
}

// Stop cancels every active run and waits for their goroutines to
// finish, but does not flip persisted statuses or emit any
// observability event. Use Stop in tests or for a quiet teardown
// where the caller takes responsibility for the on-disk state.
//
// Production shutdown should call Drain instead, which additionally
// publishes EventRunInterrupted and flips each in-flight run to
// failed_resumable so the next server boot can offer one-click resume.
func (s *Service) Stop(ctx context.Context) {
	s.manager.Stop(ctx)
}

// Drain performs a graceful shutdown of every active run:
//
//  1. Sets the draining flag so subsequent Launch / Resume return
//     runtime.ErrServerDraining.
//  2. Snapshots active handles and cancels each one.
//  3. Waits on each handle's done channel up to ctx's deadline.
//  4. For every run that was active at the moment of Drain — whether
//     its goroutine exited cleanly within the deadline or not —
//     emits EventRunInterrupted and flips the persisted status to
//     failed_resumable with reason "server drained".
//
// The status flip happens regardless of clean exit so the on-disk
// state is unambiguous; the runtime's own failure event (typically
// EventRunFailed with cause "context canceled") may also land in
// the same events.jsonl, which is acceptable telemetry noise — both
// events accurately describe what happened.
//
// Drain is intended to be called once during process shutdown. After
// it returns, the service should not be used to launch new work.
func (s *Service) Drain(ctx context.Context) {
	s.draining.Store(true)

	handles := s.manager.Snapshot()

	for _, h := range handles {
		h.Cancel()
	}

	for _, h := range handles {
		select {
		case <-h.Done:
		case <-ctx.Done():
			// Out of time — record what's still live then bail out.
			s.markRemainingInterrupted(handles)
			return
		}
	}

	// All goroutines drained within budget. Flip statuses + emit events.
	for _, h := range handles {
		s.markInterrupted(h.RunID)
	}
}

// markRemainingInterrupted marks every snapshot handle as interrupted.
// Used on the deadline-exceeded path where we can't tell which
// individual handles are still live without re-snapshotting; flipping
// all of them is idempotent (UpdateRunStatus tolerates the run already
// being in a terminal state — it just rewrites the status).
func (s *Service) markRemainingInterrupted(handles []HandleSnapshot) {
	for _, h := range handles {
		s.markInterrupted(h.RunID)
	}
}

// markInterrupted emits EventRunInterrupted and flips the run's status
// to failed_resumable with reason "server drained". Errors are logged
// at warn level — drain must not abort over a single run's bookkeeping.
func (s *Service) markInterrupted(runID string) {
	const reason = "server drained: editor process shutting down"
	if _, err := s.store.AppendEvent(runID, store.Event{
		Type:  store.EventRunInterrupted,
		RunID: runID,
		Data:  map[string]interface{}{"reason": reason},
	}); err != nil {
		s.logger.Warn("runview: drain: append run_interrupted for %s: %v", runID, err)
	}
	if err := s.store.UpdateRunStatus(runID, store.RunStatusFailedResumable, reason); err != nil {
		s.logger.Warn("runview: drain: update status for %s: %v", runID, err)
	}
}

// ---------------------------------------------------------------------------
// Read-side API
// ---------------------------------------------------------------------------

// LoadRun returns the persisted Run metadata for runID.
func (s *Service) LoadRun(runID string) (*store.Run, error) {
	return s.store.LoadRun(runID)
}

// List returns every run in the store filtered by f. The result is
// sorted by CreatedAt descending (newest first); Limit truncates after
// sort.
func (s *Service) List(f ListFilter) ([]RunSummary, error) {
	ids, err := s.store.ListRuns()
	if err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(ids))
	for _, id := range ids {
		r, err := s.store.LoadRun(id)
		if err != nil {
			// A single corrupt run.json shouldn't break the whole listing.
			s.logger.Warn("runview: skip run %s: %v", id, err)
			continue
		}
		if !matchesFilter(r, f) {
			continue
		}
		// Node filter is more expensive (loads events.jsonl for each
		// candidate). Run it last so cheaper rejection criteria above
		// short-circuit first.
		if f.Node != "" && !runTouchedNode(s.store, r.ID, f.Node) {
			continue
		}
		out = append(out, RunSummary{
			ID:           r.ID,
			Name:         r.Name,
			WorkflowName: r.WorkflowName,
			Status:       r.Status,
			FilePath:     r.FilePath,
			CreatedAt:    r.CreatedAt,
			UpdatedAt:    r.UpdatedAt,
			FinishedAt:   r.FinishedAt,
			Error:        r.Error,
			Active:       s.manager.Active(r.ID),
			FinalCommit:  r.FinalCommit,
			FinalBranch:  r.FinalBranch,
			MergedInto:   r.MergedInto,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// runTouchedNode returns true if the run's events.jsonl contains at
// least one node_started event for nodeID. Short-circuits on first
// match. Errors loading events are treated as "didn't touch" — a
// run we can't read shouldn't surface as a hit.
//
// Streams events through ScanEvents instead of materialising the full
// slice via LoadEvents — long-running runs can have hundreds of MB of
// events.jsonl, and a list filter pass that calls this for every
// candidate run would otherwise be O(N*size) memory.
func runTouchedNode(s *store.RunStore, runID, nodeID string) bool {
	hit := false
	_ = s.ScanEvents(runID, func(e *store.Event) bool {
		if e.Type == store.EventNodeStarted && e.NodeID == nodeID {
			hit = true
			return false
		}
		return true
	})
	return hit
}

func matchesFilter(r *store.Run, f ListFilter) bool {
	if f.Status != "" && r.Status != f.Status {
		return false
	}
	if f.Workflow != "" && r.WorkflowName != f.Workflow {
		return false
	}
	if !f.Since.IsZero() && r.UpdatedAt.Before(f.Since) {
		return false
	}
	return true
}

// Snapshot returns the structured RunSnapshot for runID by folding the
// persisted events through the canonical reducer.
func (s *Service) Snapshot(runID string) (*RunSnapshot, error) {
	return BuildSnapshot(s.store, runID)
}

// MaxEventsPerPage caps the number of events any single LoadEvents
// response materialises. A 200MB events.jsonl from a long-running run
// with hundreds of LLM I/O events would otherwise allocate the full
// file into memory on every reconnect / scrubber drag — exhausting
// memory in typical devcontainers. Callers paginate by passing the
// next page's `from` as previous_last.Seq+1; len(out) == cap means
// "more available".
const MaxEventsPerPage = 5000

// LoadEvents returns events in [from, to] (inclusive on from, exclusive
// on to), capped at MaxEventsPerPage. Pass to=0 for "no upper bound".
// Used by the scrubber to lazy-load segments of a long run.
//
// Streams via store.LoadEventsRange so we never materialise more than
// the page-cap worth of events at once; callers paginate.
func (s *Service) LoadEvents(runID string, from, to int64) ([]*store.Event, error) {
	return s.store.LoadEventsRange(runID, from, to, MaxEventsPerPage)
}

// ListArtifacts enumerates the persisted artifacts for one node by
// reading the artifact directory directly — avoids the O(versions)
// JSON-decode of the full bodies that LoadArtifact would do just to
// extract the version number. Returns the versions in ascending order.
func (s *Service) ListArtifacts(runID, nodeID string) ([]ArtifactSummary, error) {
	if err := validatePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := validatePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.storeDir, "runs", runID, "artifacts", nodeID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("runview: list artifacts: %w", err)
	}
	out := make([]ArtifactSummary, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		v, parseErr := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if parseErr != nil {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil {
			continue
		}
		out = append(out, ArtifactSummary{Version: v, WrittenAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// LoadArtifact returns one persisted artifact body.
func (s *Service) LoadArtifact(runID, nodeID string, version int) (*store.Artifact, error) {
	return s.store.LoadArtifact(runID, nodeID, version)
}

// ---------------------------------------------------------------------------
// Write-side API: lifecycle
// ---------------------------------------------------------------------------

// Cancel signals an active run to stop. Returns ErrRunNotActive if the
// run is not held by this process — cross-process cancel is not
// supported in the current design.
func (s *Service) Cancel(runID string) error {
	return s.manager.Cancel(runID)
}

// LaunchResult is returned by Launch on success.
type LaunchResult struct {
	RunID string
	// Done is closed when the run goroutine exits (success or
	// failure). Callers that want to wait can `<-result.Done`.
	Done <-chan struct{}
}

// Launch starts a workflow asynchronously and returns once the run
// handle has been registered with the manager (i.e. Cancel will work
// from the moment Launch returns nil error).
//
// The caller is expected to have already validated spec.FilePath
// against any sandbox / origin policy. The service does not double-
// check origins — its job is lifecycle, not authentication.
func (s *Service) Launch(parent context.Context, spec LaunchSpec) (*LaunchResult, error) {
	if s.draining.Load() {
		return nil, runtime.ErrServerDraining
	}
	if spec.FilePath == "" {
		return nil, errors.New("runview: file_path is required")
	}
	runID := spec.RunID
	if runID == "" {
		runID = fmt.Sprintf("run_%d", time.Now().UnixMilli())
	}

	if detachedEnabled() {
		return s.launchDetached(parent, runID, spec)
	}

	wf, hash, err := CompileWorkflowWithHash(spec.FilePath)
	if err != nil {
		return nil, err
	}

	_, runLogger := s.prepareRunLog(runID)

	executor, err := BuildExecutor(ExecutorSpec{
		Workflow: wf,
		Vars:     spec.Vars,
		Store:    s.store,
		RunID:    runID,
		Logger:   runLogger,
		StoreDir: s.storeDir,
	})
	if err != nil {
		s.dropRunLog(runID)
		return nil, err
	}

	inputs := make(map[string]interface{}, len(spec.Vars))
	for k, v := range spec.Vars {
		inputs[k] = v
	}

	runName := store.GenerateRunName(spec.FilePath + ":" + runID)
	fin := finalizationOpts{mergeInto: spec.MergeInto, branchName: spec.BranchName}

	return s.spawnRun(parent, runID, wf, hash, spec.FilePath, runName, fin, executor, runLogger, spec.Timeout, false,
		func(ctx context.Context, eng *runtime.Engine) error {
			return eng.Run(ctx, runID, inputs)
		})
}

// Resume re-enters a paused, failed_resumable, or cancelled run with
// optional answers. The .iter source must be supplied (and must hash-
// match the original unless spec.Force).
func (s *Service) Resume(parent context.Context, spec ResumeSpec) (*LaunchResult, error) {
	if s.draining.Load() {
		return nil, runtime.ErrServerDraining
	}
	if spec.RunID == "" {
		return nil, errors.New("runview: run_id is required")
	}
	if spec.FilePath == "" {
		return nil, errors.New("runview: file_path is required")
	}

	r, err := s.store.LoadRun(spec.RunID)
	if err != nil {
		return nil, err
	}
	if r.Status == store.RunStatusRunning {
		// Targeted reconcile: turn an orphan running run (server
		// restart, abrupt goroutine exit) into a resumable status
		// before validating, so the user doesn't have to wait for
		// the next NewService call to clean it up.
		reconciled, didReconcile, rcErr := s.reconcileRun(spec.RunID)
		if rcErr != nil {
			return nil, rcErr
		}
		if didReconcile {
			r = reconciled
		}
	}
	if err := validateResumable(r, spec.Answers); err != nil {
		return nil, err
	}

	if detachedEnabled() {
		return s.resumeDetached(parent, spec)
	}

	wf, hash, err := CompileWorkflowWithHash(spec.FilePath)
	if err != nil {
		return nil, err
	}

	_, runLogger := s.prepareRunLog(spec.RunID)

	executor, err := BuildExecutor(ExecutorSpec{
		Workflow: wf,
		Store:    s.store,
		RunID:    spec.RunID,
		Logger:   runLogger,
		StoreDir: s.storeDir,
	})
	if err != nil {
		s.dropRunLog(spec.RunID)
		return nil, err
	}
	if len(r.Inputs) > 0 {
		executor.SetVars(r.Inputs)
	}

	// Preserve an existing name; back-fill one for legacy runs that
	// predate the friendly-name field so the editor never falls back
	// to workflow_name after a resume.
	runName := r.Name
	if runName == "" {
		runName = store.GenerateRunName(spec.FilePath + ":" + spec.RunID)
	}

	// Finalization params for resume: empty (no override). The original
	// launch's choice cannot be re-derived (we don't persist the
	// MergeInto/BranchName decisions on the run), so resume uses
	// engine defaults. If we ever surface "edit finalization on
	// resume" we'd plumb a ResumeSpec field here.
	return s.spawnRun(parent, spec.RunID, wf, hash, spec.FilePath, runName, finalizationOpts{}, executor, runLogger, spec.Timeout, spec.Force,
		func(ctx context.Context, eng *runtime.Engine) error {
			// Re-validate under the lock acquired by spawnRun (TOCTOU
			// guard against a concurrent resume / state change).
			r2, err := s.store.LoadRun(spec.RunID)
			if err != nil {
				return err
			}
			if err := validateResumable(r2, spec.Answers); err != nil {
				return err
			}
			return eng.Resume(ctx, spec.RunID, spec.Answers)
		})
}

// validateResumable returns nil if r is in a state from which Resume
// can proceed; otherwise it returns a descriptive error.
func validateResumable(r *store.Run, answers map[string]interface{}) error {
	switch r.Status {
	case store.RunStatusPausedWaitingHuman:
		if len(answers) == 0 {
			return fmt.Errorf("no answers provided; resume of paused run requires answers")
		}
		return nil
	case store.RunStatusFailedResumable, store.RunStatusCancelled:
		return nil
	default:
		return fmt.Errorf("run %q cannot be resumed (status: %s)", r.ID, r.Status)
	}
}

// reconcileRun is the on-demand counterpart to reconcileOrphans: when a
// resume request arrives for a run still flagged `running` and the
// service has no active handle for it, the run is an orphan from a
// previous server lifetime (or a goroutine that died abruptly). Trying
// to grab the lock — which the OS auto-releases on process exit — proves
// liveness; if it succeeds, the run is genuinely dead and we flip the
// status so resume can proceed. If the lock is held (live goroutine in
// this process or another), nothing happens and resume rejects normally.
//
// Returns the up-to-date run (post-reconcile if it fired) so the caller
// doesn't have to re-load.
func (s *Service) reconcileRun(runID string) (*store.Run, bool, error) {
	r, err := s.store.LoadRun(runID)
	if err != nil {
		return nil, false, err
	}
	if r.Status != store.RunStatusRunning {
		return r, false, nil
	}
	// If the manager already tracks this run, it's live in this
	// process — leave it alone, resume will reject with the active
	// status error.
	if s.manager.Active(runID) {
		return r, false, nil
	}
	lock, err := s.store.LockRun(runID)
	if err != nil {
		// Lock held by a real process — skip reconcile.
		return r, false, nil
	}
	// Re-read under the lock in case another writer raced us.
	r2, err := s.store.LoadRun(runID)
	if err != nil || r2.Status != store.RunStatusRunning {
		_ = lock.Unlock()
		if err != nil {
			return r, false, nil
		}
		return r2, false, nil
	}
	newStatus := store.RunStatusFailed
	if r2.Checkpoint != nil {
		newStatus = store.RunStatusFailedResumable
	} else {
		// No checkpoint means the run died before any node finished —
		// resume from entry is now possible thanks to the engine-side
		// permissive-restart path. Flag as resumable too so the editor
		// can offer the resume button.
		newStatus = store.RunStatusFailedResumable
	}
	const reason = "orphan reconciled on resume request: server had no live goroutine for run"
	if err := s.store.UpdateRunStatus(runID, newStatus, reason); err != nil {
		_ = lock.Unlock()
		return r2, false, fmt.Errorf("reconcile %s: %w", runID, err)
	}
	_ = lock.Unlock()
	if s.logger != nil {
		s.logger.Info("runview: reconciled orphan run %s on demand → %s", runID, newStatus)
	}
	r3, _ := s.store.LoadRun(runID)
	if r3 == nil {
		return r2, true, nil
	}
	return r3, true, nil
}

// spawnRun owns the lock + register + goroutine + defer-cleanup
// scaffolding shared by Launch and Resume. body is invoked inside the
// goroutine with the registered ctx and the constructed engine; its
// return value is fed into logRunOutcome. spawnRun returns once the
// run handle is registered (so Cancel works from that moment).
func (s *Service) spawnRun(
	parent context.Context,
	runID string,
	wf *ir.Workflow,
	hash, filePath, runName string,
	fin finalizationOpts,
	executor runtime.NodeExecutor,
	runLogger *iterlog.Logger,
	timeout time.Duration,
	force bool,
	body func(ctx context.Context, eng *runtime.Engine) error,
) (*LaunchResult, error) {
	lock, err := s.store.LockRun(runID)
	if err != nil {
		s.dropRunLog(runID)
		return nil, fmt.Errorf("runview: lock run: %w", err)
	}

	ctx, regErr := s.manager.Register(parent, runID)
	if regErr != nil {
		_ = lock.Unlock()
		s.dropRunLog(runID)
		return nil, regErr
	}

	var cancelTimeout context.CancelFunc
	if timeout > 0 {
		ctx, cancelTimeout = context.WithTimeout(ctx, timeout)
	}

	opts := s.engineOptions(runLogger, hash, filePath, runName, fin)
	if force {
		opts = append(opts, runtime.WithForceResume(true))
	}
	eng := runtime.New(wf, s.store, executor, opts...)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer s.dropRunLog(runID)
		defer s.broker.CloseRun(runID)
		defer s.manager.Deregister(runID)
		defer func() { _ = lock.Unlock() }()
		if cancelTimeout != nil {
			defer cancelTimeout()
		}

		s.logRunOutcome(runID, body(ctx, eng))
	}()

	return &LaunchResult{RunID: runID, Done: done}, nil
}

// finalizationOpts groups the worktree-finalization params Launch (and
// Resume, in case the user wants to revisit the choice mid-run) wants
// to thread through to the engine without inflating engineOptions's
// signature for every callsite.
type finalizationOpts struct {
	mergeInto  string
	branchName string
}

// engineOptions builds the standard option set for both Launch and
// Resume: logger, recovery dispatch, broker observer, extra observers,
// workflow hash, file path, run name, and worktree-finalization
// targets. The logger is always per-run (built by prepareRunLog) so
// every iterion log line is captured into the run's log buffer for
// streaming to the editor.
func (s *Service) engineOptions(runLogger *iterlog.Logger, hash, filePath, runName string, fin finalizationOpts) []runtime.EngineOption {
	if runLogger == nil {
		runLogger = s.logger
	}
	opts := []runtime.EngineOption{
		runtime.WithLogger(runLogger),
		runtime.WithRecoveryDispatch(s.recoveryDispatch),
		runtime.WithEventObserver(s.broker.Publish),
	}
	for _, obs := range s.extraObservers {
		opts = append(opts, runtime.WithEventObserver(obs))
	}
	if hash != "" {
		opts = append(opts, runtime.WithWorkflowHash(hash))
	}
	if filePath != "" {
		opts = append(opts, runtime.WithFilePath(filePath))
	}
	if runName != "" {
		opts = append(opts, runtime.WithRunName(runName))
	}
	if fin.mergeInto != "" {
		opts = append(opts, runtime.WithMergeInto(fin.mergeInto))
	}
	if fin.branchName != "" {
		opts = append(opts, runtime.WithBranchName(fin.branchName))
	}
	return opts
}

// logRunOutcome emits a single line at the end of a run goroutine so
// an HTTP-only operator (no console attached) gets at least one record
// of terminal status. The user-facing surfacing is via events on disk
// + the WS stream; this is a service-level breadcrumb.
func (s *Service) logRunOutcome(runID string, err error) {
	if err == nil {
		s.logger.Info("runview: run %s finished", runID)
		return
	}
	switch {
	case errors.Is(err, runtime.ErrRunPaused):
		s.logger.Info("runview: run %s paused (waiting for human input)", runID)
	case errors.Is(err, runtime.ErrRunCancelled):
		s.logger.Info("runview: run %s cancelled", runID)
	default:
		s.logger.Warn("runview: run %s failed: %v", runID, err)
	}
}

// validatePathComponent rejects empty / traversal / separator-bearing
// path components. Mirrors the store's defensive check; we duplicate
// it here for ListArtifacts which reads the directory directly.
func validatePathComponent(name, component string) error {
	if component == "" {
		return fmt.Errorf("runview: %s must not be empty", name)
	}
	if strings.Contains(component, "..") || strings.ContainsAny(component, "/\\") || strings.ContainsRune(component, 0) {
		return fmt.Errorf("runview: %s %q contains illegal characters", name, component)
	}
	return nil
}
