package runview

import (
	"context"
	"errors"
	"strings"

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
	specs = PoolSurvivingSpecs(specs, pool, logger)
	return startDeclaredImpl(ctx, s, s, runID, specs, logger)
}

// startDeclaredImpl is the dispatchable seam behind startDeclaredSupervisors
// (the test swaps it to capture the specs that survive the guard).
var startDeclaredImpl = supervise.StartDeclared

// PoolSurvivingSpecs filters declared supervisors against a run's frozen
// pool stamp: on a pool-stamped run, a supervisor whose effective model is
// not gateway-routed is refused (D12) — evaluating the run's content
// through a vendor default would spend a provider the pool team never
// funded. A supervisor with no DSL pin falls back to
// ITERION_DEFAULT_SUPERVISOR_MODEL at eval time, so the filter consults the
// same fallback: a gateway env pin keeps unpinned supervisors alive; a
// vendor env pin or no env at all refuses them. A nil pool keeps every
// spec.
func PoolSurvivingSpecs(specs []supervise.Spec, pool string, logger *iterlog.Logger) []supervise.Spec {
	if pool == "" {
		return specs
	}
	envDefault := ir.LookupEnv("ITERION_DEFAULT_SUPERVISOR_MODEL")
	kept := specs[:0]
	for _, spec := range specs {
		// The pin may be an env form ("${VAR:-openai_compatible/x}") —
		// expand it exactly like eval time does, else a supervisor whose
		// effective model is the gateway would be refused here.
		model := strings.TrimSpace(ir.ExpandEnvWithDefault(spec.Model))
		if model == "" {
			model = envDefault
		}
		if err := poolContentRefusal(pool, model); err != nil {
			logger.Warn("supervisor %q: %v", spec.Name, err)
			continue
		}
		kept = append(kept, spec)
	}
	return kept
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
func (s *Service) startSessionBoard(ctx context.Context, runID, botID string, logger *iterlog.Logger) (stop func()) {
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
	if err := poolContentRefusal(pool, sessionboard.ModelFromEnv()); err != nil {
		logger.Warn("session board: %v", err)
		return func() {}
	}
	initial, _ := s.sbStore.Load(runID)
	cfg := sessionboard.Config{
		BotID:   botID,
		Model:   sessionboard.ModelFromEnv(),
		Initial: initial,
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
