package runview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/reviewtopology"
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
	// Validate budget overrides up front so a malformed max_duration fails
	// the launch synchronously instead of being silently dropped by
	// newSharedBudget. Mirrors pkg/cli/run.go's pre-flight Validate.
	if spec.Budget != nil {
		if err := spec.Budget.Validate(); err != nil {
			return nil, fmt.Errorf("budget: %w", err)
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
		// Budget overrides ride the RunMessage (queue.RunMessage.Budget);
		// the runner applies them after loading the workflow, under its
		// multitenant cloud ceiling.
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

	// Local pipeline-concurrency gate. Only ROOT launches (no parent)
	// count against the cap; a child belongs to a root that already holds
	// a slot, so children always start immediately. nil queue = unlimited
	// (existing behaviour). Over the limit, the launch is parked as a
	// queued doc (surfaced on the board's TODO lane) and started later by
	// the scheduler when a slot frees.
	if spec.ParentRunID == "" && s.pipelineQueue != nil {
		admitted, pos := s.pipelineQueue.admitOrEnqueue(runID, spec)
		if !admitted {
			return s.enqueuePipeline(parent, runID, spec, pos)
		}
		res, startErr := s.startInProcess(parent, runID, spec, true)
		if startErr != nil {
			// Release the reserved slot so a failed start can't wedge the queue.
			s.pipelineQueue.slotFreed(runID)
		}
		return res, startErr
	}
	return s.startInProcess(parent, runID, spec, true)
}

// startInProcess compiles + builds + spawns a run in this process. It is
// the shared body of an immediate Launch and the scheduler's start of a
// previously-queued root. precreate controls doc creation: true mints a
// fresh running doc (the normal launch path); false starts against an
// existing queued doc (the engine's runResolveDoc transitions it
// queued→running), used when the concurrency gate deferred the launch.
func (s *Service) startInProcess(parent context.Context, runID string, spec LaunchSpec, precreate bool) (*LaunchResult, error) {
	wf, hash, err := compileForLaunch(spec.FilePath, spec.Source)
	if err != nil {
		return nil, err
	}

	// Apply budget overrides AFTER compile but BEFORE BuildExecutor — the
	// executor snapshots Budget at construction, so a later mutation would
	// be invisible to the model/cost layer. Same ordering contract as the
	// CLI path (pkg/cli/run.go).
	if spec.Budget != nil {
		ir.ApplyBudgetOverrides(wf, *spec.Budget)
	}

	_, runLogger := s.prepareRunLog(runID)

	// LaunchSpec.ExtraObservers (ADR-046) reach the run through TWO
	// disjoint seams — WITHOUT wrapping the store (a wrapper would shadow
	// the concrete FilesystemRunStore's optional capabilities against the
	// PlanWriter / RunFilesStore / … type-probes the executor + sandbox
	// run). Backend-hook events (the high-frequency tool stream) fire the
	// observers via ExecutorSpec.EventObservers; engine events fire them
	// via runtime.WithEventObserver (wired in engineOptions from
	// launchExtras.observers). The raw store keeps every capability.
	executor, err := BuildExecutor(ExecutorSpec{
		Workflow:       wf,
		Vars:           spec.Vars,
		Store:          s.store,
		EventObservers: spec.ExtraObservers,
		RunID:          runID,
		Logger:         runLogger,
		StoreDir:       s.storeDir,
		Inbox:          s.inboxBinder(),
		AsyncAsk:       s.asyncAskBinder(),
		Backend:        spec.Backend,
		ModelOverrides: toModelOverrides(spec.ModelOverrides),
		BotID:          spec.BotID,
		BoardRegister:  s.boardRegister,
		Compress:       spec.Compress,
		Permission:     spec.Permission,
		LocalSecrets:   s.localSecrets,
		LocalSealer:    s.localSealer,
	})
	if err != nil {
		s.dropRunLog(runID)
		return nil, err
	}

	// Fold the bundle's file-based presets (presets/<name>.md) into wf so a
	// studio `--preset <name>` selection resolves a file-based sous-bot — not
	// just an in-source presets: entry — and its var overrides apply below.
	// The engine re-applies this as a backstop and also pushes the prompt
	// bias + skill hints into every LLM node ("## Focus").
	if b := ResolveBundleFromFilePath(spec.FilePath); b != nil {
		runtime.MergeBundlePresets(wf, b, runLogger)
	}

	inputs := make(map[string]any, len(spec.Vars))
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

	// Resolve the mono/dual review topology (no-op unless the workflow
	// declares a review_mode var). Mirrors the CLI so studio/API/dispatcher
	// launches auto-detect providers too. The spec override (studio toggle)
	// wins over a --var review_mode; both win over auto.
	if mode, family, injected := reviewtopology.InjectIfDeclared(wf, inputs, detect.Detect(parent), spec.ReviewMode); injected {
		if family != "" {
			runLogger.Info("review topology: %s (family %s)", mode, family)
		} else {
			runLogger.Info("review topology: %s", mode)
		}
	}

	runName := store.GenerateRunName(spec.FilePath + ":" + runID)
	fin := finalizationOpts{
		mergeInto:     spec.MergeInto,
		branchName:    spec.BranchName,
		mergeStrategy: spec.MergeStrategy,
		autoMerge:     spec.AutoMerge,
	}
	cb := callbackOpts{
		url:        spec.CallbackURL,
		token:      spec.CallbackToken,
		answerNode: spec.CallbackAnswerNode,
	}

	precreateInputs := inputs
	if !precreate {
		// The queued doc already exists; let the engine claim it
		// (queued→running via runResolveDoc) instead of re-creating.
		precreateInputs = nil
	}
	return s.spawnRun(parent, runID, wf, hash, spec.FilePath, runName, fin, cb, executor, runLogger, spec.Timeout, false,
		spec.AttachmentPromote, spec.Preset, toRunModelOverrides(spec.ModelOverrides),
		spec.ParentRunID,
		precreateInputs,
		launchExtras{workDir: spec.WorkDir, dailyCap: spec.DailyCap, source: spec.SourceRef, onOutcome: spec.OnOutcome, observers: spec.ExtraObservers},
		s.store,
		func(ctx context.Context, eng *runtime.Engine) error {
			return eng.Run(ctx, runID, inputs)
		})
}

// Resume re-enters a human-paused, operator-paused, failed_resumable,
// or cancelled run with optional answers. The .bot source must be
// supplied (and must hash-match the original unless spec.Force).
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

	// Wait out a previous runner that is still tearing down, BEFORE anything
	// takes a resource it holds. This is the top of the resume path on purpose:
	// the store flock (taken inside spawnRun) is what a resume in the hand-off
	// window actually collides with — it is released two deferred statements
	// before the manager handle — so a wait placed at Register would only cover
	// the sliver between them and the operator would still see a failure, just
	// with a different message. No-op unless the previous runner has announced
	// it is leaving (see Manager.AwaitHandoff).
	s.manager.AwaitHandoff(parent, spec.RunID)

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

	// Compile and compare synchronously before handing the resume to any
	// asynchronous execution mode. Without this preflight, in-process runs
	// returned from spawnRun before Engine.Resume checked the hash, while
	// detached/cloud runners only discovered the mismatch after this service
	// had already reported success (202) to the HTTP caller — the studio then
	// treats the human answer as accepted and hides the form even though the
	// run is still paused. A synchronous error lets the existing UI preserve
	// the answer and offer its explicit force-resume retry.
	//
	// Engine.Resume repeats the same check after acquiring the run lock, so a
	// source/status change between this point and execution still fails closed.
	wf, hash, err := compileForLaunch(spec.FilePath, spec.Source)
	if err != nil {
		return nil, err
	}
	if err := runtime.ValidateResumeWorkflowHash(r.ID, r.WorkflowHash, hash, spec.Force); err != nil {
		return nil, err
	}

	// Cloud-mode resume: republish the RunMessage with ResumeSpec
	// set so the runner pool re-enters the engine via Engine.Resume.
	// Plan §F (T-33). CAS protection on the Mongo checkpoint lives
	// in MongoRunStore.SaveCheckpoint (CASVersion increment).
	if s.publisher != nil {
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

	_, runLogger := s.prepareRunLog(spec.RunID)

	executor, err := BuildExecutor(ExecutorSpec{
		Workflow:      wf,
		Store:         s.store,
		RunID:         spec.RunID,
		Logger:        runLogger,
		StoreDir:      s.storeDir,
		Inbox:         s.inboxBinder(),
		AsyncAsk:      s.asyncAskBinder(),
		BoardRegister: s.boardRegister,
		LocalSecrets:  s.localSecrets,
		LocalSealer:   s.localSealer,
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
	return s.spawnRun(parent, spec.RunID, wf, hash, spec.FilePath, runName, finalizationOpts{}, callbackOpts{}, executor, runLogger, spec.Timeout, spec.Force,
		nil, r.Preset, nil,
		r.ParentRunID,
		nil,
		launchExtras{},
		nil,
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
func validateResumable(r *store.Run, answers map[string]any) error {
	switch r.Status {
	case store.RunStatusPausedWaitingHuman:
		if len(answers) == 0 {
			return fmt.Errorf("no answers provided; resume of paused run requires answers")
		}
		return nil
	case store.RunStatusPausedOperator, store.RunStatusFailedResumable, store.RunStatusCancelled:
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
	cb callbackOpts,
	executor runtime.NodeExecutor,
	runLogger *iterlog.Logger,
	timeout time.Duration,
	force bool,
	promote runtime.AttachmentPromoteFunc,
	preset string,
	modelOverrides []store.RunModelOverride,
	parentRunID string,
	precreateInputs map[string]any,
	ex launchExtras,
	emitStore store.RunStore,
	body func(ctx context.Context, eng *runtime.Engine) error,
) (*LaunchResult, error) {
	// emitStore is the store the engine writes events through. It is the
	// raw s.store — ADR-046 ExtraObservers ride the WithEventObserver /
	// ExecutorSpec.EventObservers seams (see startInProcess), not a store
	// wrapper, so every optional store capability survives to the probes.
	if emitStore == nil {
		emitStore = s.store
	}
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

	// Launch path only (nil on resume, whose doc already exists): persist
	// the run doc BEFORE returning, so a GET /api/runs/{id} issued right
	// after the launch response never 404s on the engine goroutine still
	// being scheduled. The engine's runResolveDoc sees the running doc and
	// claims it instead of re-creating.
	if precreateInputs != nil {
		// ParentRunID is part of the launch identity, so persist it in the
		// SAME create write when the store supports it (ParentedRunCreator).
		// Otherwise a running child doc would exist between CreateRun and the
		// follow-up SaveRun — if that second write failed, the doc stayed
		// `running` with no goroutine until the orphan reconciler caught it
		// (PR #193 M3). The engine option below remains the authoritative path
		// for direct/non-precreated runs.
		var createErr error
		if parentRunID != "" {
			if pc := store.AsParentedRunCreator(s.store); pc != nil {
				_, createErr = pc.CreateChildRun(context.Background(), runID, wf.Name, parentRunID, precreateInputs)
			} else {
				var created *store.Run
				created, createErr = s.store.CreateRun(context.Background(), runID, wf.Name, precreateInputs)
				if createErr == nil {
					created.ParentRunID = parentRunID
					createErr = s.store.SaveRun(context.Background(), created)
				}
			}
		} else {
			_, createErr = s.store.CreateRun(context.Background(), runID, wf.Name, precreateInputs)
		}
		if createErr != nil {
			s.manager.Deregister(runID)
			_ = lock.Unlock()
			s.dropRunLog(runID)
			return nil, fmt.Errorf("runview: create run: %w", createErr)
		}
	}

	var cancelTimeout context.CancelFunc
	if timeout > 0 {
		ctx, cancelTimeout = context.WithTimeout(ctx, timeout)
	}

	opts := s.engineOptions(runLogger, hash, filePath, runName, fin, ex)
	// Subbot nodes need a host-supplied runner (the bare engine can't compile
	// a child .bot — import cycle with runview). Wired on BOTH the launch and
	// resume paths; without it, in-process studio runs of subbot-bearing bots
	// died with "no SubbotRunner is wired" (only the CLI paths were covered).
	opts = append(opts, runtime.WithSubbotRunner(s.subbotRunnerFor(filePath, runLogger)))
	if force {
		opts = append(opts, runtime.WithForceResume(true))
	}
	if promote != nil {
		opts = append(opts, runtime.WithAttachmentPromote(promote))
	}
	if preset != "" {
		opts = append(opts, runtime.WithPreset(preset))
	}
	if parentRunID != "" {
		opts = append(opts, runtime.WithParentRunID(parentRunID))
	}
	// Persist launch-time model/backend overrides on the run record
	// (display-only) so the studio Overview shows what it launched with.
	// Empty on resume, leaving the original launch's value intact.
	if len(modelOverrides) > 0 {
		opts = append(opts, runtime.WithModelOverrides(modelOverrides))
	}
	if cb.url != "" {
		opts = append(opts, runtime.WithCallback(cb.url, cb.token, cb.answerNode))
	}
	// Wire the operator-pause channel so POST /api/runs/{id}/pause
	// can interrupt this run at the next safe boundary. The Manager
	// owns the channel (created in Register above); we hand a
	// receive-only view to the engine via WithPauseSignal.
	if pauseCh, perr := s.manager.PauseSignal(runID); perr == nil {
		opts = append(opts, runtime.WithPauseSignal(pauseCh))
	}
	// Live-steering channel (bump_loop / raise_budget): buffered so a
	// burst of commands never blocks the HTTP handler; the engine
	// drains it at the same safe boundary as the pause signal.
	steerCh := make(chan *runtime.OverrideMsg, 8)
	opts = append(opts, runtime.WithOverrideChannel(steerCh))
	eng := runtime.New(wf, emitStore, executor, opts...)
	// Publish the engine so the store's Event.ActiveMs stamping can read
	// this run's monotonic active elapsed. Removed when the goroutine exits.
	s.registerRunEngine(runID, eng, steerCh)

	done := make(chan struct{})
	go func() {
		var paused bool
		defer close(done)
		// Deregister runs LAST of the teardown (defers are LIFO, so the
		// earliest-registered manager defer executes latest). It closes the
		// handle's done channel, which is what AwaitHandoff releases on — so it
		// has to mean "this goroutine is finished", not "it is partway through
		// its teardown". Registered below dropRunLog / unregisterRunEngine, a
		// released successor raced them: those two are keyed on runID alone,
		// and the successor has already installed its own log buffer and engine
		// by then, so they tore down the NEW run's — a resume that reported
		// success with a dead log tail and no steering channel. That is worse
		// than the "already registered" it replaced, because it is silent.
		defer s.manager.Deregister(runID)
		defer s.unregisterRunEngine(runID)
		defer s.dropRunLog(runID)
		// Keep WS subscribers across a pause: the goroutine exits on
		// ErrRunPaused(Operator) like on any outcome, but the run is only
		// dormant — the resume's goroutine publishes to the same broker
		// runID, and dropping subscribers here loses the very events that
		// announce the pause/resume (the human-gate form then lags until a
		// reload). Terminal outcomes still close the stream.
		defer func() {
			if !paused {
				s.broker.CloseRun(runID)
			}
		}()
		// Release this root's pipeline-concurrency slot. A no-op for run IDs
		// that never held a slot (children, resumes, or when the cap is
		// disabled). It runs before Deregister now; harmless, because the
		// waiter it admits is a DIFFERENT run and pipelineQueue keeps its own
		// accounting — it never consults the Manager.
		defer s.pipelineQueue.slotFreed(runID)
		defer func() { _ = lock.Unlock() }()
		if cancelTimeout != nil {
			defer cancelTimeout()
		}

		// Spawn any DSL-declared supervisors for the lifetime of the run.
		// They observe via the broker (in-process) and steer via
		// QueueMessage; Close drains them before the goroutine exits.
		stopSupervisors := s.startDeclaredSupervisors(ctx, runID, wf, runLogger)
		defer stopSupervisors()

		// Spawn the Session-board curation coordinator (opt-in via
		// ITERION_SESSION_BOARD). No-op when disabled — the deterministic
		// task-list board (Phase 1) runs in the studio regardless.
		stopBoard := s.startSessionBoard(ctx, runID, runName, runLogger)
		defer stopBoard()

		bodyErr := body(ctx, eng)
		// The engine has written its terminal/paused status to the store, so
		// the run now READS resumable while this handle is still held through
		// the teardown below (a completion webhook, a supervisor drain). Say
		// so, and a resume arriving in that window waits the handoff out
		// instead of failing on "already registered".
		s.manager.MarkLeaving(runID)
		paused = errors.Is(bodyErr, runtime.ErrRunPaused) || errors.Is(bodyErr, runtime.ErrRunPausedOperator)
		s.logRunOutcome(runID, bodyErr)
		// Hand the terminal error to a blocking caller (ADR-046: the
		// dispatcher's EngineRunner routing through Launch) BEFORE Done
		// closes, so it returns the same typed error the direct engine.Run
		// would have. No-op for fire-and-forget launches.
		if ex.onOutcome != nil {
			ex.onOutcome(bodyErr)
		}
		// Fire the run-completion webhook (no-op unless the run carries a
		// callback URL). Uses a fresh, tenant-unfiltered ctx: the run ctx
		// may be cancelled at this point, and the runID is already known.
		// FireForRun re-reads the persisted run, so it sees the terminal
		// status the engine just wrote regardless of bodyErr's shape.
		if s.completionNotifier != nil {
			nctx := store.WithoutTenantFilter(context.Background())
			s.completionNotifier.FireForRun(nctx, s.store, runID)
		}
		// Emit the run-outcome trigger event ("runned by iterion"): a
		// finished/failed/cancelled run can fire downstream runs (pipelines,
		// on-failure escalation), and a paused run marks its board card
		// "awaiting input". No-op unless an event publisher is wired.
		s.emitRunOutcome(runID, bodyErr)
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
type callbackOpts struct {
	url        string
	token      string
	answerNode string
}

type finalizationOpts struct {
	mergeInto     string
	branchName    string
	mergeStrategy store.MergeStrategy
	autoMerge     bool
}

// launchExtras groups the per-launch overrides ADR-046 added to
// LaunchSpec so the dispatcher's EngineRunner.Dispatch can converge on
// runview.Service.Launch without losing its workspace / cap / source
// semantics. Each field overrides the matching service-level default
// (s.workDir / s.dailyCap) when set; the zero value inherits it. Resume
// and subbot launches pass the zero value.
type launchExtras struct {
	workDir  string
	dailyCap *runtime.DailyCapGuard
	source   *store.RunSource
	// onOutcome mirrors LaunchSpec.OnOutcome: fired once in the run
	// goroutine with the terminal body error before Done closes, so a
	// blocking caller reads the same typed error engine.Run returned.
	onOutcome func(error)
	// observers mirrors LaunchSpec.ExtraObservers: fired on every
	// engine-level event via runtime.WithEventObserver (the backend-hook
	// half rides ExecutorSpec.EventObservers). Together they feed the
	// dispatcher's stall heartbeat the full store-level event stream
	// without wrapping the store.
	observers []func(store.Event)
}

// engineOptions builds the standard option set for both Launch and
// Resume: logger, recovery dispatch, broker observer, extra observers,
// workflow hash, file path, run name, and worktree-finalization
// targets. The logger is always per-run (built by prepareRunLog) so
// every iterion log line is captured into the run's log buffer for
// streaming to the studio.
func (s *Service) engineOptions(runLogger *iterlog.Logger, hash, filePath, runName string, fin finalizationOpts, ex launchExtras) []runtime.EngineOption {
	if runLogger == nil {
		runLogger = s.logger
	}
	opts := []runtime.EngineOption{
		runtime.WithLogger(runLogger),
		runtime.WithRecoveryDispatch(s.recoveryDispatch),
		runtime.WithEventObserver(s.broker.Publish),
		runtime.WithOnNodeFinished(s.stampWatchedFromOutput),
	}
	// Global sandbox default (sandbox-by-default): injected by the
	// PRODUCT constructors (studio/server/dispatch daemons) via
	// WithSandboxDefault. A Service built without it (tests, embedders)
	// stays neutral, mirroring the engine's own contract.
	if s.sandboxDefault != "" {
		opts = append(opts, runtime.WithSandboxDefault(s.sandboxDefault))
	}
	// Per-launch WorkDir (ADR-046) overrides the service default; the
	// dispatcher points it at the per-issue worktree so ${PROJECT_DIR}
	// resolves there, not the daemon's cwd.
	workDir := s.workDir
	if ex.workDir != "" {
		workDir = ex.workDir
	}
	if workDir != "" {
		opts = append(opts, runtime.WithWorkDir(workDir))
	}
	if s.boardMCPHandler != nil {
		opts = append(opts, runtime.WithBoardMCP(s.boardMCPHandler))
	}
	// Per-launch DailyCap (ADR-046) overrides the service default so the
	// dispatcher's singleton-SpendStore guard writes the one shared ledger.
	dailyCap := s.dailyCap
	if ex.dailyCap != nil {
		dailyCap = ex.dailyCap
	}
	if dailyCap != nil {
		opts = append(opts, runtime.WithDailyCap(dailyCap))
	}
	// Per-launch SourceRef (ADR-046) stamps the originating dispatcher issue
	// onto the run record. Nil for CLI / studio / fork launches.
	if ex.source != nil {
		opts = append(opts, runtime.WithSource(ex.source))
	}
	// Run-health alerting. In-process runs feed the broker directly (not
	// the events.jsonl file tailer, which only runs for detached /
	// reattached / non-Active runs via the runstream file tailer). Without this
	// observer the alert Manager would never see budget / failure events
	// or advance its stall heartbeat for the default in-process path.
	// Detached runs are fed via the runstream file tailer instead; the two paths do
	// not overlap (an Active in-process run never gets a file tailer), so
	// there is no double-observe.
	if s.alertManager != nil {
		opts = append(opts, runtime.WithEventObserver(s.alertManager.Observe))
	}
	for _, obs := range s.extraObservers {
		opts = append(opts, runtime.WithEventObserver(obs))
	}
	// Per-launch ExtraObservers (ADR-046): the engine-level half of the
	// dispatcher's stall-heartbeat seam. The backend-hook half is wired
	// through ExecutorSpec.EventObservers; the two event sets are disjoint
	// (engine emits fire e.onEvent, backend hooks fire the redacting
	// emitter), so an event reaches each observer exactly once.
	for _, obs := range ex.observers {
		opts = append(opts, runtime.WithEventObserver(obs))
	}
	if hash != "" {
		opts = append(opts, runtime.WithWorkflowHash(hash))
	}
	if filePath != "" {
		opts = append(opts, runtime.WithFilePath(filePath))
		// F-NEW-4: studio + cloud launches bypass pkg/cli/run.go's
		// bundle-detect path. When the operator points at
		// <bundle-dir>/main.bot directly, ResolveBundleFromFilePath
		// climbs to the parent and opens it as a bundle so the engine
		// can mirror skills/ + recipes/ + attachments/ into the
		// workspace before any node runs. Nil bundle → engine no-ops
		// (existing behaviour for inline / standalone .bot files).
		if b := ResolveBundleFromFilePath(filePath); b != nil {
			opts = append(opts, runtime.WithBundle(b))
		}
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

// stampWatchedFromOutput subscribes a run to the kanban issues it just
// dispatched. Wired as the engine's onNodeFinished hook: when a node's
// output carries `dispatched_ids` (the assign_to_bots / triage_board
// convention — IDs transitioned to `ready`), those issues are merged
// into Run.WatchedIssueIDs so the server-side watch coordinator fans
// future board transitions back to this run. The convention lives here,
// not in the generic engine, so the runtime stays decoupled from a
// bot-specific schema field.
func (s *Service) stampWatchedFromOutput(runID, _ string, output map[string]any) {
	if output == nil {
		return
	}
	ids := extractStringIDs(output["dispatched_ids"])
	if len(ids) == 0 {
		return
	}
	if _, err := s.store.AddWatchedIssues(context.Background(), runID, ids); err != nil {
		s.logger.Warn("runview: stamp watched issues on run %s: %v", runID, err)
	}
}

// extractStringIDs coerces a node-output value into a slice of non-empty
// string IDs. Tolerates the JSON shapes a `json`-typed schema field
// decodes into: []interface{} of strings, []string, or a single string.
func extractStringIDs(v any) []string {
	switch t := v.(type) {
	case []string:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if e != "" {
				out = append(out, e)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if str, ok := e.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		// A `json`-typed schema field can arrive as the literal *text* of
		// a JSON array — e.g. an LLM emits `dispatched_ids: []` (or a
		// populated `["native:abc"]`) as a string rather than a real
		// array. Parse those so an empty array yields zero IDs instead of
		// a phantom `"[]"` watch (which then 404s in the run console), and
		// a populated one contributes its real elements.
		if s[0] == '[' {
			var arr []any
			if err := json.Unmarshal([]byte(s), &arr); err == nil {
				return extractStringIDs(arr)
			}
			// Looked like an array but didn't parse — drop it rather than
			// watch the malformed literal.
			return nil
		}
		return []string{s}
	}
	return nil
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
