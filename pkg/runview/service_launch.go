package runview

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// LaunchResult is returned by Launch on success.
type LaunchResult struct {
	RunID string
	// Done is closed when the run goroutine exits (success or
	// failure). Callers that want to wait can `<-result.Done`. Cloud-
	// mode launches return a Done channel that is already closed —
	// the runner pod owns the lifecycle, not this server.
	Done <-chan struct{}
	// QueuePosition is the 1-based position on the cloud queue at
	// the moment of submission. Zero when launching in-process.
	QueuePosition int
}

// LaunchPublisher routes Launch / Resume / Cancel to the cloud
// queue + Mongo store instead of spawning the runtime in-process.
// When NewService is called with WithLaunchPublisher, every Launch
// becomes a "submit + return queue_position"; the runner pool drains
// the queue separately. Plan §F (T-31, T-32, T-33).
type LaunchPublisher interface {
	// SubmitLaunch persists the run as queued in the cloud store
	// and publishes a RunMessage. Returns the 1-based queue position
	// at submission time.
	SubmitLaunch(ctx context.Context, runID string, spec LaunchSpec, wf *ir.Workflow, hash string) (int, error)
	// CancelRun signals the runner pool to abort the run. Idempotent —
	// flips the Mongo doc to cancelled regardless of whether a runner
	// is currently holding the lease.
	CancelRun(ctx context.Context, runID string) error
	// SubmitResume republishes a RunMessage with ResumeSpec set so
	// the runner picks the run back up.
	SubmitResume(ctx context.Context, spec ResumeSpec, wf *ir.Workflow, hash string) error
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
	if spec.FilePath == "" && spec.Source == "" {
		return nil, errors.New("runview: file_path or source is required")
	}
	if spec.BranchName != "" {
		if err := gitlib.ValidateBranchName(spec.BranchName); err != nil {
			return nil, fmt.Errorf("branch_name: %w", err)
		}
	}
	runID := spec.RunID
	if runID == "" {
		generated, err := store.GenerateRunID()
		if err != nil {
			return nil, fmt.Errorf("mint run id: %w", err)
		}
		runID = generated
	}

	// Cloud-mode: hand off to the runner pool via the queue. The
	// publisher persists the run in Mongo as queued + emits the
	// RunMessage; the runner pod takes it from there. We compile
	// the workflow here so the wire payload carries an inline IR
	// (the runner currently doesn't support IRRef fallback). When
	// Source is supplied (cloud HTTP API) we compile from memory
	// instead of reading from disk — the server pod has no shared
	// filesystem with the client.
	if s.publisher != nil {
		wf, hash, err := compileForLaunch(spec.FilePath, spec.Source)
		if err != nil {
			return nil, err
		}
		pos, err := s.publisher.SubmitLaunch(parent, runID, spec, wf, hash)
		if err != nil {
			return nil, err
		}
		// Synthesise a closed Done channel — the cloud handler is
		// fire-and-forget. UI consumers track lifecycle via the WS
		// event stream the runner pod populates.
		closed := make(chan struct{})
		close(closed)
		return &LaunchResult{RunID: runID, Done: closed, QueuePosition: pos}, nil
	}

	if detachedEnabled() {
		return s.launchDetached(parent, runID, spec)
	}

	wf, hash, err := compileForLaunch(spec.FilePath, spec.Source)
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
		Inbox:    s.inboxBinder(),
		Backend:  spec.Backend,
	})
	if err != nil {
		s.dropRunLog(runID)
		return nil, err
	}

	inputs := make(map[string]interface{}, len(spec.Vars))
	if spec.Preset != "" {
		preset, ok := wf.Presets[spec.Preset]
		if !ok {
			s.dropRunLog(runID)
			available := make([]string, 0, len(wf.Presets))
			for name := range wf.Presets {
				available = append(available, name)
			}
			sort.Strings(available)
			if len(available) == 0 {
				return nil, fmt.Errorf("preset %q: workflow has no presets declared", spec.Preset)
			}
			return nil, fmt.Errorf("preset %q: unknown preset (available: %s)", spec.Preset, strings.Join(available, ", "))
		}
		for k, v := range preset.Values {
			inputs[k] = v
		}
	}
	for k, v := range spec.Vars {
		inputs[k] = v
	}

	runName := store.GenerateRunName(spec.FilePath + ":" + runID)
	fin := finalizationOpts{
		mergeInto:     spec.MergeInto,
		branchName:    spec.BranchName,
		mergeStrategy: spec.MergeStrategy,
		autoMerge:     spec.AutoMerge,
	}

	return s.spawnRun(parent, runID, wf, hash, spec.FilePath, runName, fin, executor, runLogger, spec.Timeout, false,
		spec.AttachmentPromote, spec.Preset,
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

	// Propagate parent so the mongo backend's tenant filter applies:
	// a cross-tenant Resume must resolve to not-found, not panic on a
	// missing tenant_id in ctx (which Background carries).
	r, err := s.store.LoadRun(parent, spec.RunID)
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

	// Cloud-mode resume: republish the RunMessage with ResumeSpec
	// set so the runner pool re-enters the engine via Engine.Resume.
	// Plan §F (T-33). CAS protection on the Mongo checkpoint lives
	// in MongoRunStore.SaveCheckpoint (CASVersion increment).
	if s.publisher != nil {
		wf, hash, err := compileForLaunch(spec.FilePath, spec.Source)
		if err != nil {
			return nil, err
		}
		if err := s.publisher.SubmitResume(parent, spec, wf, hash); err != nil {
			return nil, err
		}
		closed := make(chan struct{})
		close(closed)
		return &LaunchResult{RunID: spec.RunID, Done: closed}, nil
	}

	if detachedEnabled() {
		return s.resumeDetached(parent, spec)
	}

	// Honour spec.Source when supplied (cloud-mode callers materialise
	// .iter contents inline; the runner pod may have no FilePath on
	// disk). The publish path above already uses compileForLaunch for
	// the same reason — this branch was the only one still routing
	// through the disk-only CompileWorkflowWithHash.
	wf, hash, err := compileForLaunch(spec.FilePath, spec.Source)
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
		Inbox:    s.inboxBinder(),
	})
	if err != nil {
		s.dropRunLog(spec.RunID)
		return nil, err
	}
	if len(r.Inputs) > 0 {
		executor.SetVars(r.Inputs)
	}

	// Preserve an existing name; back-fill one for legacy runs that
	// predate the friendly-name field so the studio never falls back
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
		nil, r.Preset,
		func(ctx context.Context, eng *runtime.Engine) error {
			// Re-validate under the lock acquired by spawnRun (TOCTOU
			// guard against a concurrent resume / state change).
			r2, err := s.store.LoadRun(context.Background(), spec.RunID)
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
	hash, filePath, runName string,
	fin finalizationOpts,
	executor runtime.NodeExecutor,
	runLogger *iterlog.Logger,
	timeout time.Duration,
	force bool,
	promote runtime.AttachmentPromoteFunc,
	preset string,
	body func(ctx context.Context, eng *runtime.Engine) error,
) (*LaunchResult, error) {
	lock, err := s.store.LockRun(context.Background(), runID)
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
	if promote != nil {
		opts = append(opts, runtime.WithAttachmentPromote(promote))
	}
	if preset != "" {
		opts = append(opts, runtime.WithPreset(preset))
	}
	// Wire the operator-pause channel so POST /api/runs/{id}/pause
	// can interrupt this run at the next safe boundary. The Manager
	// owns the channel (created in Register above); we hand a
	// receive-only view to the engine via WithPauseSignal.
	if pauseCh, perr := s.manager.PauseSignal(runID); perr == nil {
		opts = append(opts, runtime.WithPauseSignal(pauseCh))
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

		bodyErr := body(ctx, eng)
		s.logRunOutcome(runID, bodyErr)
		// On cancel, the engine flipped run.Status to cancelled but didn't
		// run finalizeWorktree (that's the success path only). If the run
		// produced commits, RecoverFinalize promotes the worktree HEAD to
		// a storage branch so the studio's "Squash and merge" button can
		// act on it without waiting for a daemon restart. Idempotent +
		// scoped to worktree runs with no FinalBranch yet, so it's safe to
		// call unconditionally.
		if errors.Is(bodyErr, runtime.ErrRunCancelled) {
			// Post-cancel housekeeping: the run ctx is cancelled at
			// this point, so use a fresh ctx with WithoutTenantFilter
			// — the runID is already known, and this is a system-level
			// recovery operation, not a tenant-discovery lookup.
			fctx := store.WithoutTenantFilter(context.Background())
			if r, loadErr := s.store.LoadRun(fctx, runID); loadErr == nil {
				if recErr := runtime.RecoverFinalize(fctx, s.store, r, s.logger); recErr != nil {
					s.logger.Warn("runview: post-cancel finalize for %s: %v", runID, recErr)
				}
			}
		}
	}()

	return &LaunchResult{RunID: runID, Done: done}, nil
}

// finalizationOpts groups the worktree-finalization params Launch (and
// Resume, in case the user wants to revisit the choice mid-run) wants
// to thread through to the engine without inflating engineOptions's
// signature for every callsite.
type finalizationOpts struct {
	mergeInto     string
	branchName    string
	mergeStrategy store.MergeStrategy
	autoMerge     bool
}

// engineOptions builds the standard option set for both Launch and
// Resume: logger, recovery dispatch, broker observer, extra observers,
// workflow hash, file path, run name, and worktree-finalization
// targets. The logger is always per-run (built by prepareRunLog) so
// every iterion log line is captured into the run's log buffer for
// streaming to the studio.
func (s *Service) engineOptions(runLogger *iterlog.Logger, hash, filePath, runName string, fin finalizationOpts) []runtime.EngineOption {
	if runLogger == nil {
		runLogger = s.logger
	}
	opts := []runtime.EngineOption{
		runtime.WithLogger(runLogger),
		runtime.WithRecoveryDispatch(s.recoveryDispatch),
		runtime.WithEventObserver(s.broker.Publish),
	}
	if s.workDir != "" {
		opts = append(opts, runtime.WithWorkDir(s.workDir))
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
	if fin.mergeStrategy != "" {
		opts = append(opts, runtime.WithMergeStrategy(string(fin.mergeStrategy)))
	}
	if fin.autoMerge {
		opts = append(opts, runtime.WithAutoMerge(true))
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
