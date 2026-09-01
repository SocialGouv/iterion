package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/routing"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The outcome router: the instance-side answer to "a run produced work
// and nobody routed it". A terminal run that carries a RoutingPolicy is
// offered here from two independent paths — the run-outcome bus (fast,
// lossy by design) and a periodic sweep over the store (the source of
// truth: six terminal paths never publish an event, and the bus's own
// doc names the poll as its backstop). The decision is read from the
// launch-frozen contract by pkg/routing (strict, fail-closed), claimed
// once per EPISODE in the durable registry (unique (run_id,
// outcome_seq)), and only then acted on. Measured motivation, one
// campaign, 48h: a converged run waited 8h51 for a human; another was
// marked "handled" by its external observer and converged unseen; a
// third landed while carrying an explicit blocker and reddened every
// downstream launch.
const (
	outcomeRouterName = "outcome-router"

	// outcomeRouterEnv is the fleet-global switch: "on" activates the
	// router. It rides the Deployment manifest, so every replica reads
	// the same value and flipping it is an explicit rollout — not a
	// per-pod accident (the plan review's mixed-fleet finding).
	outcomeRouterEnv = "ITERION_OUTCOME_ROUTER"

	// routerSweepInterval: worst-case latency between a silent terminal
	// and its decision. Matches the sibling sweepers.
	routerSweepInterval = 60 * time.Second

	// routerSweepGrace keeps the sweep off runs the event path may
	// still be handling.
	routerSweepGrace = 2 * time.Minute

	// routerSweepLookback bounds a pass. Generous on purpose: a
	// sleeping episode is the exact class this router exists for, and
	// re-offering an already-decided run costs one registry read (the
	// unique claim stops it).
	routerSweepLookback = 24 * time.Hour

	// routerSweepBatch bounds one pass's candidate list.
	routerSweepBatch = 200
)

// outcomeRouterEnabled reads the fleet switch.
func outcomeRouterEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(outcomeRouterEnv)), "on")
}

// startOutcomeRouter attaches the router to the event spine and starts
// its sweep net. No-op unless the fleet switch is on AND the store
// carries the decision-registry capability (without a durable claim
// there is no idempotence — fail safe, never run unclaimed).
func (s *Server) startOutcomeRouter() {
	if s == nil || s.cfg.Store == nil || s.runs == nil {
		return
	}
	if !outcomeRouterEnabled() {
		return
	}
	if store.AsRouteDecisionStore(s.cfg.Store) == nil {
		s.logWarn("server: outcome router requested but the store has no decision registry — router stays off")
		return
	}
	bus := s.eventsBus()
	if bus != nil {
		if cancel, err := s.attachOutcomeRouter(bus); err != nil {
			s.logWarn("server: outcome router subscribe failed: %v — the sweep net still runs", err)
		} else {
			s.outcomeRouterCancel = cancel
		}
	}
	go s.outcomeRouterSweepLoop()
	if s.logger != nil {
		s.logger.Info("server: outcome router attached (policy-carrying terminal runs are decided by their launch-frozen contract)")
	}
}

func (s *Server) attachOutcomeRouter(bus eventbus.Bus) (func(), error) {
	return bus.Subscribe(outcomeRouterName, trigger.Matcher{
		Sources: []trigger.Source{trigger.SourceRun},
		Kinds:   []string{trigger.KindRunFinished, trigger.KindRunFailed},
	}, func(ctx context.Context, ev trigger.Event) error {
		s.routeOutcomeOffer(ctx, strings.TrimSpace(ev.Subject.ID))
		return nil
	})
}

// outcomeRouterSweepLoop is the source-of-truth net: every interval it
// re-offers every policy-carrying terminal run in the window. The
// registry claim makes a double offer cost one read.
func (s *Server) outcomeRouterSweepLoop() {
	t := time.NewTicker(routerSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.shutdown:
			return
		case <-t.C:
			s.outcomeRouterSweepPass(context.Background())
		}
	}
}

func (s *Server) outcomeRouterSweepPass(ctx context.Context) {
	rds := store.AsRouteDecisionStore(s.cfg.Store)
	if rds == nil {
		return
	}
	ids, err := rds.ListRoutableRuns(store.WithoutTenantFilter(ctx), time.Now().Add(-routerSweepLookback), routerSweepBatch)
	if err != nil {
		s.logWarn("server: outcome router sweep: %v", err)
		return
	}
	cutoff := time.Now().Add(-routerSweepGrace)
	for _, id := range ids {
		s.routeOutcomeOfferBefore(ctx, id, cutoff)
	}
}

// routeOutcomeOffer is the fast-path entry (bus): no grace — the event
// IS the signal the terminal write finished.
func (s *Server) routeOutcomeOffer(ctx context.Context, runID string) {
	s.routeOutcomeOfferBefore(ctx, runID, time.Time{})
}

// routeOutcomeOfferBefore offers one run. Every refusal is silent by
// design (the sweep offers plenty of runs that need nothing); every
// DECISION leaves a registry row.
func (s *Server) routeOutcomeOfferBefore(ctx context.Context, runID string, updatedBefore time.Time) {
	if runID == "" || s.cfg.Store == nil || s.runs == nil {
		return
	}
	rds := store.AsRouteDecisionStore(s.cfg.Store)
	if rds == nil {
		return
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil || run == nil || run.RoutingPolicy == nil {
		return
	}
	if !updatedBefore.IsZero() && run.UpdatedAt.After(updatedBefore) {
		// Inside the sweep grace: the event path may still be at work.
		return
	}

	// --- Terminal gates. The sweep window already filters statuses,
	// but the bus path cannot, and one explicit gate serves both. ---
	switch run.Status {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusFailedResumable:
	default:
		// Cancelled is an operator's stop — never auto-routed, in any
		// direction. Paused is not terminal. Running moved on.
		return
	}
	// A platform continuation owns the run's future: a queue redelivery
	// or an armed retry WILL act without us — deciding now would race
	// it (the measured probe-resume class, from the other side).
	switch run.ContinuationState {
	case store.ContinuationRedeliveryPending, store.ContinuationRetryArmed:
		return
	}
	if run.OutcomeSeq == 0 {
		// Terminal predating the episode bookkeeping: no stable claim
		// key exists, so the registry cannot make this idempotent.
		// Leave it to its external observer rather than act unclaimed.
		return
	}
	if run.MergeStatus == store.MergeStatusMerged {
		return
	}

	// --- The contract's verdict, adjusted by what the STATE allows.
	// Only a finished run may merge: an interrupted run's checkpoint
	// carries outputs from an EARLIER pass — gates that were true then,
	// not a verdict on the tree as it died. ---
	verdict := routing.Evaluate(run)
	decision := verdict.Decision
	reason := verdict.Reason
	if decision == routing.DecisionMerge && run.Status != store.RunStatusFinished {
		decision = routing.DecisionEscalate
		reason = fmt.Sprintf("contract says merge but the run is %s — stale checkpoint outputs are not a verdict (%s)", run.Status, verdict.Reason)
	}
	if decision == routing.DecisionMerge && (run.FinalBranch == "" || run.FinalCommit == "" || run.FinalBranchError != "") {
		decision = routing.DecisionEscalate
		reason = fmt.Sprintf("contract says merge but the bank is not intact (branch=%q commit=%q err=%q)", run.FinalBranch, run.FinalCommit, run.FinalBranchError)
	}

	// --- One decision per episode: the registry claim. ---
	tctx := store.WithIdentity(store.WithoutTenantFilter(ctx), run.TenantID, outcomeRouterName)
	claimed, existing, err := rds.ClaimRouteDecision(tctx, store.RouteDecision{
		RunID:      run.ID,
		OutcomeSeq: run.OutcomeSeq,
		Decision:   string(decision),
		Reason:     reason,
		PolicyHash: run.RoutingPolicy.Hash,
	})
	if err != nil {
		s.logWarn("server: outcome router: claim %s:%d: %v", run.ID, run.OutcomeSeq, err)
		return
	}
	if !claimed {
		_ = existing // episode already decided (possibly by another replica) — stop.
		return
	}

	// --- Act. Every branch finishes the registry row. ---
	switch decision {
	case routing.DecisionMerge:
		req := runview.MergeRequest{}
		if run.RoutingPolicy.MergeInto != "" {
			req.MergeInto = run.RoutingPolicy.MergeInto
		}
		if run.RoutingPolicy.MergeStrategy != "" {
			req.Strategy = run.RoutingPolicy.MergeStrategy
		}
		_, mergeErr := s.runs.PerformMergeCtx(tctx, run.ID, req)
		if mergeErr != nil {
			s.logWarn("server: outcome router: merge %s: %v", run.ID, mergeErr)
			s.finishRouteDecision(tctx, rds, run, store.RouteDecisionFailed, mergeErr.Error())
			return
		}
		if s.logger != nil {
			s.logger.Info("server: outcome router: run %s episode %d merged by its contract (%s)", run.ID, run.OutcomeSeq, reason)
		}
		s.finishRouteDecision(tctx, rds, run, store.RouteDecisionSucceeded, "")
	case routing.DecisionRelaunch:
		// Execution of relaunches (fresh run anchored on the banked
		// branch) is not wired yet: record the decision honestly and
		// leave the act to an operator. The registry row IS the
		// escalation surface.
		s.logWarn("server: outcome router: run %s episode %d — contract permits relaunch, execution not enabled: operator action required (%s)", run.ID, run.OutcomeSeq, reason)
		s.finishRouteDecision(tctx, rds, run, store.RouteDecisionFailed, "relaunch execution not enabled — operator action required")
	default: // escalate
		s.logWarn("server: outcome router: run %s episode %d ESCALATED: %s", run.ID, run.OutcomeSeq, reason)
		s.finishRouteDecision(tctx, rds, run, store.RouteDecisionSucceeded, "")
	}
}

func (s *Server) finishRouteDecision(ctx context.Context, rds store.RouteDecisionStore, run *store.Run, state, actionErr string) {
	if err := rds.FinishRouteDecision(ctx, run.ID, run.OutcomeSeq, state, actionErr); err != nil {
		s.logWarn("server: outcome router: finish %s:%d: %v", run.ID, run.OutcomeSeq, err)
	}
}
