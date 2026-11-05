package runview

import (
	"context"
	"errors"
	"strings"

	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sessionboard"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// ObserveRun streams a run's events for an external observer (the
// supervisor coordinator): a catch-up replay of everything persisted so
// far — so a late-attaching observer can reconstruct the currently
// active node — followed by live events, deduplicated by seq. The
// returned channel is closed when the run terminates (broker CloseRun)
// or ctx is cancelled.
//
// The caller MUST invoke release exactly once when done; it cancels the
// live subscription and releases the on-demand file tailer (started for
// runs this process did not launch in-process, e.g. a dispatcher- or
// CLI-spawned run observed from a studio process).
//
// Local broker mode only — cloud event-source mode is out of scope for
// the supervisor's local attach path and returns a typed error.
func (s *Service) ObserveRun(ctx context.Context, runID string) (<-chan *store.Event, func(), error) {
	if runID == "" {
		return nil, nil, errors.New("runview: run_id is required")
	}
	if s.broker == nil {
		return nil, nil, errors.New("runview: no broker wired (cannot observe run)")
	}
	// Subscribe BEFORE the catch-up read so any event persisted during
	// the read is buffered on the live channel and deduped by seq —
	// never dropped between snapshot and live.
	var releaseSrc func()
	if !s.Active(runID) {
		// Bridge events.jsonl -> broker for runs not produced in-process.
		releaseSrc = s.ensureEventSource(runID)
	}
	sub := s.broker.Subscribe(runID)

	out := make(chan *store.Event, subscriberBufferSize)
	release := func() {
		sub.Cancel()
		if releaseSrc != nil {
			releaseSrc()
		}
	}

	go func() {
		defer close(out)
		var lastSeq int64 = -1
		// Catch-up replay from disk.
		if events, err := s.store.LoadEvents(ctx, runID); err == nil {
			for _, e := range events {
				if e == nil {
					continue
				}
				select {
				case out <- e:
					if e.Seq > lastSeq {
						lastSeq = e.Seq
					}
				case <-ctx.Done():
					return
				}
			}
		}
		// Live events, deduped against the catch-up tail.
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-sub.C:
				if !ok {
					return
				}
				if e == nil || e.Seq <= lastSeq {
					continue
				}
				lastSeq = e.Seq
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, release, nil
}

// Inject enqueues a steering message into runID, scoped to nodeID when
// non-empty (delivered only while that node is the active executing
// node). It wraps QueueMessage + WithMessageNode so a supervisor
// coordinator (pkg/supervise) can drive *Service through the
// supervise.Injector seam without pkg/supervise importing runview.
func (s *Service) Inject(ctx context.Context, runID, nodeID, text string) error {
	_, err := s.QueueMessage(ctx, runID, text, WithMessageNode(nodeID))
	return err
}

// InjectOnce is the crash-safe supervisor delivery path. deliveryID is
// stable for one watcher trigger, so replay after a cursor-write failure does
// not enqueue a second correction or resurrect one already consumed.
func (s *Service) InjectOnce(ctx context.Context, runID, nodeID, text, deliveryID string) error {
	_, err := s.QueueMessage(ctx, runID, text, WithMessageNode(nodeID), withMessageIDOnce(deliveryID))
	return err
}

// startDeclaredSupervisors spawns a supervise.Coordinator for every
// `supervisor NAME:` block on the workflow, each bound to this run's
// lifetime via ctx. They observe through the broker (in-process) and
// steer via Inject. Returns a stop func the caller defers to drain them
// before the run goroutine exits. A no-op when the workflow declares
// none, or when the kill switch (override → ITERION_SUPERVISORS → on)
// says off — the skip is logged by the shared gate.
func (s *Service) startDeclaredSupervisors(ctx context.Context, runID string, wf *ir.Workflow, vars map[string]any, logger *iterlog.Logger, override string) (stop func()) {
	if wf == nil || len(wf.Supervisors) == 0 {
		return func() {}
	}
	if !supervise.DeclaredEnabledOrWarn(override, len(wf.Supervisors), logger) {
		return func() {}
	}
	specs := supervise.SpecsFromWorkflow(wf, vars, logger)
	pool, err := s.frozenPoolStamp(ctx, runID)
	if err != nil {
		// The stamp is unreadable: the surface stays OFF rather than guess
		// the run was never pool-stamped. The run itself is unaffected.
		logger.Warn("supervisors: pool stamp unreadable for run %s — refusing to start (%v)", runID, err)
		return func() {}
	}
	boundary, err := s.frozenContentBoundary(ctx, runID)
	if err != nil {
		logger.Warn("supervisors: content boundary unreadable for run %s — refusing to start (%v)", runID, err)
		return func() {}
	}
	specs = FundedSupervisorSpecs(ctx, specs, boundary, pool, logger)
	return startDeclaredImpl(ctx, s, s, runID, specs, logger)
}

// startDeclaredImpl is the dispatchable seam behind startDeclaredSupervisors
// (the test swaps it to capture the specs that survive the guard).
var startDeclaredImpl = supervise.StartDeclared

// FundedSupervisorSpecs keeps the declared supervisors whose evaluations the
// resolver funds (ADR-161 decision 4) — the list a spawn starts and the
// usage-cap pre-flight meters, one call for both — and drops the rest, each
// named on logger with its reason:
//
//   - the content boundary first (supervisorContentRefusal, D12): a team
//     gateway boundary refuses every spec, a pool stamp every spec whose
//     effective model is not gateway-routed;
//   - then the claw arm, in process, on the model the evaluator resolves at
//     its first evaluation (supervise.ResolveModel: the pin, the env default,
//     the watched nodes' family, the detector) under its provider hint. A
//     spec the arm refuses — the stamp's refusal of the pod's money, the
//     subscription switch, an unfunded hint, no credential at all — would
//     refuse every evaluation, so it is dropped where that is decided. A
//     stamped run's own sealed key serves its supervisor: the stamp refuses
//     the environment only (ADR-131 §7).
//
// A spec whose model resolves nowhere is kept: its evaluator refuses every
// evaluation with its own error (supervise.ErrNoSupervisorModel).
func FundedSupervisorSpecs(ctx context.Context, specs []supervise.Spec, boundary *store.RunContentBoundary, pool string, logger *iterlog.Logger) []supervise.Spec {
	if boundary != nil && boundary.Kind == store.ContentBoundaryTeamGateway {
		logger.Warn("supervisors: the run's content boundary is the team gateway %q — every supervisor spec is refused, no escape", boundary.GatewayHost)
		return nil
	}
	envDefault := ir.LookupEnv("ITERION_DEFAULT_SUPERVISOR_MODEL")
	kept := make([]supervise.Spec, 0, len(specs))
	for _, spec := range specs {
		if r := supervisorContentRefusal(spec, boundary, pool, envDefault); r != nil {
			logger.Warn("supervisor %q: %v", spec.Name, r)
			continue
		}
		resolved, err := supervise.ResolveModel(ctx, spec.Model, spec.ProviderHint)
		if err != nil {
			kept = append(kept, spec)
			continue
		}
		_, err = credroute.Resolve(ctx, credroute.Call{
			Backend: delegate.BackendClaw, Spec: resolved,
			Leg: credroute.Leg{Provider: spec.ProviderHint}, Role: credroute.RoleSupervisor,
		})
		if err != nil && !errors.Is(err, credroute.ErrNoArm) {
			logger.Warn("supervisor %q: dropped — no credential funds its model %q, so every evaluation would be refused: %v", spec.Name, resolved, err)
			continue
		}
		kept = append(kept, spec)
	}
	return kept
}

// supervisorContentRefusal is the content-boundary refusal of a declared
// supervisor (D12): evaluating the run's content through a vendor default
// would spend a provider the run's frozen stamps never funded. A team gateway
// boundary refuses every spec; a pool stamp refuses one whose effective model
// — the pin, an env form expanded exactly like eval time does, else
// envDefault (ITERION_DEFAULT_SUPERVISOR_MODEL, the evaluator's own fallback)
// — is not gateway-routed. Nil where the spec may proceed. It unwraps to
// ErrPoolContentRefused.
func supervisorContentRefusal(spec supervise.Spec, boundary *store.RunContentBoundary, pool, envDefault string) *credroute.Refusal {
	model := strings.TrimSpace(ir.ExpandEnvWithDefault(spec.Model))
	if model == "" {
		model = envDefault
	}
	err := contentRefusal(boundary, pool, model)
	if err == nil {
		return nil
	}
	return &credroute.Refusal{Backend: delegate.BackendClaw, Spec: model, Reason: credroute.ReasonContentBoundary, Cause: err}
}

// Publish persists an updated Session-board spec for runID. It satisfies
// the sessionboard.Emitter seam so a curation Coordinator can drive
// *Service without importing it. The studio picks up the change by
// refetching the spec as the run's event stream advances (board updates
// are infrequent by design — the coordinator's cooldown floor).
func (s *Service) Publish(_ context.Context, runID string, spec sessionboard.Spec) error {
	if s.sbStore == nil {
		return errors.New("runview: session board store not configured")
	}
	return s.sbStore.Save(runID, spec)
}

// SessionBoard returns the persisted Session-board spec for runID, or a
// zero-value spec when none exists / the store is unavailable. Consumed by
// the REST handler and to seed a resuming coordinator.
func (s *Service) SessionBoard(runID string) (sessionboard.Spec, error) {
	if s.sbStore == nil {
		return sessionboard.Spec{}, nil
	}
	return s.sbStore.Load(runID)
}

// startSessionBoard spawns a sessionboard.Coordinator for the run when the
// LLM curation layer is enabled (ITERION_SESSION_BOARD) and a spec store
// is available. Returns a stop func the caller defers. A no-op otherwise —
// the deterministic task-list board (Phase 1) is always on in the studio
// and needs nothing here.
//
// gatewayWatched is supervise.WorkflowNamesGateway(wf), computed by the
// caller which holds the compiled workflow: a gateway-served session refuses
// curation through a vendor default (#2099) — the sessionboard twin of
// supervise.ErrGatewayWatchNeedsPin.
func (s *Service) startSessionBoard(ctx context.Context, runID, botID string, gatewayWatched bool, logger *iterlog.Logger) (stop func()) {
	if s.sbStore == nil || !sessionboard.Enabled() {
		return func() {}
	}
	pool, err := s.frozenPoolStamp(ctx, runID)
	if err != nil {
		// The stamp is unreadable: the surface stays OFF rather than guess
		// the run was never pool-stamped. The run itself is unaffected.
		logger.Warn("session board: pool stamp unreadable for run %s — refusing to start (%v)", runID, err)
		return func() {}
	}
	boundary, err := s.frozenContentBoundary(ctx, runID)
	if err != nil {
		logger.Warn("session board: content boundary unreadable for run %s — refusing to start (%v)", runID, err)
		return func() {}
	}
	if err := contentRefusal(boundary, pool, sessionboard.ModelFromEnv()); err != nil {
		logger.Warn("session board: %v", err)
		return func() {}
	}
	initial, _ := s.sbStore.Load(runID)
	cfg := sessionboard.Config{
		BotID:          botID,
		Model:          sessionboard.ModelFromEnv(),
		GatewayWatched: gatewayWatched,
		Initial:        initial,
	}
	coord := newSessionBoardCoordinator(s, s, runID, cfg, nil, logger)
	if coord == nil {
		return func() {}
	}
	coord.Start(ctx)
	return coord.Close
}

// newSessionBoardCoordinator is the dispatchable seam behind
// startSessionBoard (the test swaps it to count constructions).
var newSessionBoardCoordinator = sessionboard.New
