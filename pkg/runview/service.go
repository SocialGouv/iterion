package runview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	ID           string          `json:"id"`
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

// Broker exposes the event broker for transports that need to
// subscribe directly (the WS handler).
func (s *Service) Broker() *EventBroker { return s.broker }

// Stop drains every active run for graceful shutdown.
func (s *Service) Stop(ctx context.Context) {
	s.manager.Stop(ctx)
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
			WorkflowName: r.WorkflowName,
			Status:       r.Status,
			FilePath:     r.FilePath,
			CreatedAt:    r.CreatedAt,
			UpdatedAt:    r.UpdatedAt,
			FinishedAt:   r.FinishedAt,
			Error:        r.Error,
			Active:       s.manager.Active(r.ID),
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
	if spec.FilePath == "" {
		return nil, errors.New("runview: file_path is required")
	}
	runID := spec.RunID
	if runID == "" {
		runID = fmt.Sprintf("run_%d", time.Now().UnixMilli())
	}

	wf, hash, err := CompileWorkflowWithHash(spec.FilePath)
	if err != nil {
		return nil, err
	}

	executor, err := BuildExecutor(ExecutorSpec{
		Workflow: wf,
		Vars:     spec.Vars,
		Store:    s.store,
		RunID:    runID,
		Logger:   s.logger,
		StoreDir: s.storeDir,
	})
	if err != nil {
		return nil, err
	}

	inputs := make(map[string]interface{}, len(spec.Vars))
	for k, v := range spec.Vars {
		inputs[k] = v
	}

	return s.spawnRun(parent, runID, wf, hash, spec.FilePath, executor, spec.Timeout, false,
		func(ctx context.Context, eng *runtime.Engine) error {
			return eng.Run(ctx, runID, inputs)
		})
}

// Resume re-enters a paused, failed_resumable, or cancelled run with
// optional answers. The .iter source must be supplied (and must hash-
// match the original unless spec.Force).
func (s *Service) Resume(parent context.Context, spec ResumeSpec) (*LaunchResult, error) {
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
	if err := validateResumable(r, spec.Answers); err != nil {
		return nil, err
	}

	wf, hash, err := CompileWorkflowWithHash(spec.FilePath)
	if err != nil {
		return nil, err
	}

	executor, err := BuildExecutor(ExecutorSpec{
		Workflow: wf,
		Store:    s.store,
		RunID:    spec.RunID,
		Logger:   s.logger,
		StoreDir: s.storeDir,
	})
	if err != nil {
		return nil, err
	}
	if len(r.Inputs) > 0 {
		executor.SetVars(r.Inputs)
	}

	return s.spawnRun(parent, spec.RunID, wf, hash, spec.FilePath, executor, spec.Timeout, spec.Force,
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

// spawnRun owns the lock + register + goroutine + defer-cleanup
// scaffolding shared by Launch and Resume. body is invoked inside the
// goroutine with the registered ctx and the constructed engine; its
// return value is fed into logRunOutcome. spawnRun returns once the
// run handle is registered (so Cancel works from that moment).
func (s *Service) spawnRun(
	parent context.Context,
	runID string,
	wf *ir.Workflow,
	hash, filePath string,
	executor runtime.NodeExecutor,
	timeout time.Duration,
	force bool,
	body func(ctx context.Context, eng *runtime.Engine) error,
) (*LaunchResult, error) {
	lock, err := s.store.LockRun(runID)
	if err != nil {
		return nil, fmt.Errorf("runview: lock run: %w", err)
	}

	ctx, regErr := s.manager.Register(parent, runID)
	if regErr != nil {
		_ = lock.Unlock()
		return nil, regErr
	}

	var cancelTimeout context.CancelFunc
	if timeout > 0 {
		ctx, cancelTimeout = context.WithTimeout(ctx, timeout)
	}

	opts := s.engineOptions(hash, filePath)
	if force {
		opts = append(opts, runtime.WithForceResume(true))
	}
	eng := runtime.New(wf, s.store, executor, opts...)

	done := make(chan struct{})
	go func() {
		defer close(done)
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

// engineOptions builds the standard option set for both Launch and
// Resume: logger, recovery dispatch, broker observer, extra observers,
// workflow hash, and file path.
func (s *Service) engineOptions(hash, filePath string) []runtime.EngineOption {
	opts := []runtime.EngineOption{
		runtime.WithLogger(s.logger),
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
