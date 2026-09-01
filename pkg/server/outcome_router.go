package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/routing"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// Contract fields the router does not execute yet, on purpose:
// MaxRelaunches and the "resume" allowed action are validated at launch
// and recorded with the decision, but relaunch/resume execution is not
// wired — the registry row says so explicitly and an operator acts.
//
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
	// re-offering an already-decided run is cheap (on Mongo: a refused
	// insert plus one conditional read — measured ~0.3ms per offer).
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
	rds := store.AsRouteDecisionStore(s.cfg.Store)
	if rds == nil {
		s.logWarn("server: outcome router requested but the store has no decision registry — router stays off")
		return
	}
	// Establish the activation watermark at BOOT, not at the first tick:
	// a run that terminates between the two must land above it. First
	// writer wins, so a restart or a second replica reads the original
	// instant. Failure is non-fatal — the sweep re-ensures every pass
	// and refuses to sweep until it holds (fail closed).
	if _, err := rds.EnsureRouterWatermark(store.WithoutTenantFilter(context.Background())); err != nil {
		s.logWarn("server: outcome router: establish watermark: %v", err)
	}
	bus := s.eventsBus()
	if bus != nil {
		if cancel, err := s.attachOutcomeRouter(bus); err != nil {
			s.logWarn("server: outcome router subscribe failed: %v — the sweep net still runs", err)
		} else {
			s.outcomeRouterCancel = cancel
		}
	}
	errtrack.Go("server.outcomeRouterSweep", s.outcomeRouterSweepLoop)
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
	// Hot kill-switch: turning the router ON is an explicit rollout,
	// but turning it OFF during an incident must not require one — a
	// router that merges needs a stop that works immediately.
	if !outcomeRouterEnabled() {
		return
	}
	rds := store.AsRouteDecisionStore(s.cfg.Store)
	if rds == nil {
		return
	}
	// The activation watermark bounds the lookback: flipping the fleet
	// switch on must not retro-route historical terminals (up to a full
	// batch of merges pushed to the forge in the first minute). No
	// watermark ⇒ no sweep — never guess how far back is safe.
	watermark, err := rds.EnsureRouterWatermark(store.WithoutTenantFilter(ctx))
	if err != nil {
		s.logWarn("server: outcome router sweep: watermark: %v", err)
		return
	}
	since := time.Now().Add(-routerSweepLookback)
	if watermark.After(since) {
		since = watermark
	}
	ids, err := rds.ListRoutableRuns(store.WithoutTenantFilter(ctx), since, routerSweepBatch)
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
	if runID == "" || s.cfg.Store == nil || s.runs == nil || !outcomeRouterEnabled() {
		return
	}
	rds := store.AsRouteDecisionStore(s.cfg.Store)
	if rds == nil {
		return
	}
	run, err := s.cfg.Store.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil {
		// The only silent exit that would hide a STORE failure, not a
		// non-candidate — every other refusal below is a judgment on a
		// run this one never got to read.
		s.logWarn("server: outcome router: load run %s: %v", runID, err)
		return
	}
	if run == nil || run.RoutingPolicy == nil {
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

	// --- The contract's verdict, adjusted by what the BANK allows.
	// Evaluate is the single trusted reading and enforces its own
	// status preconditions (a merge verdict only ever names a run that
	// completed its workflow — an interrupted run's earlier-pass
	// outputs escalate inside Evaluate itself). What Evaluate cannot
	// know is the bank: those two guards live here. ---
	verdict := routing.Evaluate(run)
	decision := verdict.Decision
	reason := verdict.Reason
	if decision == routing.DecisionMerge && run.FinalBranchError != "" {
		decision = routing.DecisionEscalate
		reason = fmt.Sprintf("contract says merge but the bank recorded an error: %s", run.FinalBranchError)
	}
	if decision == routing.DecisionMerge && (run.FinalBranch == "" || run.FinalCommit == "") {
		// NOT a decision: the terminal status lands BEFORE the bank
		// push (measured up to 10+ minutes on large repos, and the run
		// doc's updated_at does not move in between). Claiming
		// "escalate" here would burn the episode while the branch is
		// still on its way — the bank write does not open a new one.
		// Refuse silently; the bank's SaveRun refreshes updated_at and
		// the sweep re-offers.
		return
	}

	// --- One decision per episode: the registry claim. ---
	tctx := store.WithIdentity(store.WithoutTenantFilter(ctx), run.TenantID, outcomeRouterName)
	staleBefore := time.Now().Add(-store.RouteClaimLease)
	claimed, existing, err := rds.ClaimRouteDecision(tctx, store.RouteDecision{
		RunID:      run.ID,
		OutcomeSeq: run.OutcomeSeq,
		Decision:   string(decision),
		Reason:     reason,
		PolicyHash: run.RoutingPolicy.Hash,
	}, staleBefore)
	if err != nil {
		s.logWarn("server: outcome router: claim %s:%d: %v", run.ID, run.OutcomeSeq, err)
		return
	}
	if !claimed {
		if existing != nil && existing.State == store.RouteDecisionClaimed &&
			existing.Attempts >= store.MaxRouteDecisionAttempts && existing.ClaimedAt.Before(staleBefore) {
			// Poison episode: every permitted claimant died mid-action (or
			// every escalate delivery failed) and the steal cap now refuses
			// re-arming. The operator is the only exit — so this branch
			// re-offers the alert EVERY sweep until a channel takes it
			// (the delivery claim dedups; a failed delivery releases it).
			// Once the operator has heard it, settle the row so the sweep
			// stops re-offering a decided run.
			if err := s.notifyRouteDecisionErr(tctx, run, alert.KindRouteActionFailed,
				fmt.Sprintf("routing decision %q exhausted its %d attempts without completing — operator action required", existing.Decision, existing.Attempts),
				fmt.Sprintf("route:%s:%d:exhausted", run.ID, run.OutcomeSeq)); err == nil {
				s.finishRouteDecision(tctx, rds, run, store.RouteDecisionFailed, "attempts exhausted — operator alerted")
			} else {
				s.logWarn("server: outcome router: exhausted-claim alert %s:%d: %v", run.ID, run.OutcomeSeq, err)
			}
		} else if existing != nil && s.logger != nil {
			s.logger.Debug("server: outcome router: episode %s:%d already decided (%s/%s at %s)", run.ID, run.OutcomeSeq, existing.Decision, existing.State, existing.ClaimedAt.Format(time.RFC3339))
		}
		return
	}

	// --- Act. Every branch finishes the registry row. ---
	// Re-read the run under the claim before any effect: the claim
	// serialises DECISIONS, not the run document — an operator resume
	// or a new episode landing between the guards above and this point
	// must void the action, not race it.
	if decision == routing.DecisionMerge {
		fresh, ferr := s.cfg.Store.LoadRun(tctx, run.ID)
		if ferr != nil || fresh == nil ||
			fresh.Status != store.RunStatusFinished ||
			fresh.OutcomeSeq != run.OutcomeSeq ||
			fresh.MergeStatus == store.MergeStatusMerged {
			s.finishRouteDecision(tctx, rds, run, store.RouteDecisionFailed,
				fmt.Sprintf("run moved between decision and action (err=%v) — nothing merged", ferr))
			return
		}
	}
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
			// Best-effort: the failed row's bounded re-claim retries the
			// MERGE; the alert claim keys per episode so retries don't spam.
			s.notifyRouteDecision(tctx, run, alert.KindRouteActionFailed,
				"contract-decided merge failed: "+mergeErr.Error(),
				fmt.Sprintf("route:%s:%d:action_failed", run.ID, run.OutcomeSeq))
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
		// escalation record; the alert is what makes it heard.
		s.logWarn("server: outcome router: run %s episode %d — contract permits relaunch, execution not enabled: operator action required (%s)", run.ID, run.OutcomeSeq, reason)
		s.finishRouteDecision(tctx, rds, run, store.RouteDecisionFailed, "relaunch execution not enabled — operator action required")
		s.notifyRouteDecision(tctx, run, alert.KindRouteActionFailed,
			"contract permits relaunch but execution is not enabled — operator action required ("+reason+")",
			fmt.Sprintf("route:%s:%d:action_failed", run.ID, run.OutcomeSeq))
	default: // escalate
		s.logWarn("server: outcome router: run %s episode %d ESCALATED: %s", run.ID, run.OutcomeSeq, reason)
		// The alert IS escalate's action: escalate is the DEFAULT
		// decision and a registry row nobody reads is silence. But a
		// DELIVERY failure is not an ACTION failure — finishing the row
		// failed would burn the 3-attempt cap in ~3 sweep minutes on a
		// webhook blip (a Mattermost rolling restart) and silence the
		// escalation permanently. Leave the row CLAIMED instead: the
		// 15-min lease steal re-delivers at a calmer cadence, and once
		// the steal cap is spent the exhausted-claim branch above keeps
		// offering the alert every sweep until a channel takes it. The
		// delivery claim makes a retry after a half-delivered attempt a
		// no-op.
		if err := s.notifyRouteDecisionErr(tctx, run, alert.KindRouteEscalated, reason,
			fmt.Sprintf("route:%s:%d:escalated", run.ID, run.OutcomeSeq)); err != nil {
			s.logWarn("server: outcome router: escalate %s:%d alert delivery failed — retrying on the lease cadence: %v", run.ID, run.OutcomeSeq, err)
			return
		}
		s.finishRouteDecision(tctx, rds, run, store.RouteDecisionSucceeded, "")
	}
}

// notifyRouteDecisionErr routes one router outcome to the platform
// operator through the ops-alert dispatcher. A server without alerts
// configured returns nil: the registry row and the Warn log are the
// whole surface there (a local studio has no ops channel to fail on).
func (s *Server) notifyRouteDecisionErr(ctx context.Context, run *store.Run, kind alert.Kind, reason, episodeKey string) error {
	if s.opsAlerts == nil {
		return nil
	}
	a := alert.Alert{
		Kind:        kind,
		RunID:       run.ID,
		RunName:     run.Name,
		Reason:      reason,
		FailureCode: string(run.FailureCode),
	}
	if a.RunName == "" {
		a.RunName = run.WorkflowName
	}
	if s.cfg.PublicURL != "" {
		a.Link = strings.TrimRight(s.cfg.PublicURL, "/") + "/runs/" + run.ID
	}
	return s.opsAlerts.NotifyOperator(ctx, a, episodeKey)
}

// notifyRouteDecision is the best-effort form: delivery failure is
// logged, the caller's own state machine handles redelivery.
func (s *Server) notifyRouteDecision(ctx context.Context, run *store.Run, kind alert.Kind, reason, episodeKey string) {
	if err := s.notifyRouteDecisionErr(ctx, run, kind, reason, episodeKey); err != nil {
		s.logWarn("server: outcome router: operator alert %s: %v", episodeKey, err)
	}
}

func (s *Server) finishRouteDecision(ctx context.Context, rds store.RouteDecisionStore, run *store.Run, state, actionErr string) {
	if err := rds.FinishRouteDecision(ctx, run.ID, run.OutcomeSeq, state, actionErr); err != nil {
		s.logWarn("server: outcome router: finish %s:%d: %v", run.ID, run.OutcomeSeq, err)
	}
}
