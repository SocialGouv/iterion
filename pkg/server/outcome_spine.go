package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// The run-outcome spine: ONE back-writer for every record that launches a
// run and must report what the run DID, not just that it was dispatched
// (#1501 — the class #1426 rendered on schedules alone).
//
// Shape, taken from outcome_router.go: the run-outcome event is the FAST
// path and the store sweep is the AUTHORITY. The bus is lossy by design
// (at-most-once on core NATS: no subscriber at publish instant and the
// event is gone) and terminal paths exist that publish nothing at all, so
// the event-only subscriber #1426 shipped loses stamps silently. #1426's
// arbitration — "the write sits at the run-terminal chokepoint, NEVER a
// poller" — is honoured by KEEPING the event as the chokepoint: the sweep
// is the net under it, not its replacement.
//
// Unlike the router, the spine needs no durable claim registry: its action
// is a targeted, monotonicity-guarded write of the run's own outcome tuple
// (MarkRunOutcome on each record kind), so a redelivery of one event, or
// two replicas sweeping at once, converge on the same row. The router
// claims because routing — relabel, merge, relaunch — is not idempotent;
// a stamp is.
//
// A fifth record kind is a fifth runOutcomeSink, never an engine `if` — the
// inbound webhook delivery was added exactly that way (#1501 unit 1501.3).
const (
	// runOutcomeSpineName is the eventbus subscriber (and NATS queue
	// group) name of the spine's fast path. One replica handles each
	// event.
	runOutcomeSpineName = "run-outcome-spine"

	// spineSweepInterval is the worst-case latency between a silent
	// terminal and its stamp. Matches the sibling sweepers.
	spineSweepInterval = 60 * time.Second

	// spineSweepGrace keeps the sweep off runs the event path may still
	// be handling.
	spineSweepGrace = 2 * time.Minute

	// spineSweepLookback bounds a pass. Deliberately SHORT, unlike the
	// router's 24h: what the sweep repairs is a bus gap (a replica
	// restart, a terminal path that publishes nothing), which is seconds
	// to a minute wide, and the stamp it writes is a rendered health
	// read whose miss self-heals at the record's next firing — nothing
	// downstream decides on it. A long window would instead re-offer
	// every terminal of the day on every pass, one run load each. The
	// constant is the escape hatch if an operator ever needs to reach
	// further back.
	//
	// A run already stamped is re-offered on every pass until it leaves the
	// window — one targeted _id write each time, idempotent under the
	// monotonicity guard. Deliberate: suppressing it would cost a record
	// READ per candidate per pass, or a claim registry, both dearer than
	// the ~15 redundant sub-millisecond writes a cadence-fired terminal
	// collects in 15 minutes. The board sink, which holds the compared
	// values already, skips its own write (native.BoardStore.SetRunOutcome).
	spineSweepLookback = 15 * time.Minute

	// spineSweepBatch bounds one page; spineSweepMaxPages bounds a pass.
	// The keyset cursor (updated_at) is what keeps a backlog larger than
	// one page from starving the oldest rows.
	spineSweepBatch    = 500
	spineSweepMaxPages = 20
)

// runOutcome is the tuple every record kind stamps: what the last run this
// record launched turned out to be.
type runOutcome struct {
	RunID string
	// Status is the run's terminal store.RunStatus, as a string: the
	// records persist the wire value, not the typed one.
	Status string
	// Error and ErrorCode are empty on success — a successful outcome
	// CLEARS whatever a previous failure left on the record.
	Error     string
	ErrorCode string
	// At is the run document's UpdatedAt — the instant each store's
	// monotonicity guard compares, so an out-of-order delivery cannot move
	// a record's health backwards. Deliberately NOT called a terminal
	// instant: a late write on the run (the bank's branch push refreshes
	// updated_at minutes after the status) moves it. It is the ordering
	// key #1426 already chose, kept so the schedule row's meaning does not
	// change under it.
	At time.Time
}

// runOutcomeSink is one record kind's half of the spine. The spine owns the
// subscription, the sweep and the terminal gate; a sink owns only "which
// record of my kind did this run report to" and "how is the tuple written
// onto it".
type runOutcomeSink struct {
	// name labels the sink in logs.
	name string
	// recordID names the record this run reports to, "" when the run
	// belongs to no record of this kind.
	recordID func(run *store.Run) string
	// mark writes the tuple. tenantID is the run's own tenant: some record
	// stores are tenant-scoped (the cloud board is resolved per tenant),
	// and a sink that needs it must not have to dig it back out of ctx.
	//
	// A sink maps its OWN not-found sentinel to a nil error — the record is
	// gone, or the back-reference belongs to a sibling kind — so that
	// everything the spine logs is a real store failure.
	mark func(ctx context.Context, tenantID, recordID string, o runOutcome) error
}

// runOutcomeSinks builds the sink set from what the server is wired with.
// Rebuilt per offer rather than cached: a store wired after boot is picked
// up, and three closures cost nothing against the run load each offer does
// anyway.
func (s *Server) runOutcomeSinks() []runOutcomeSink {
	var sinks []runOutcomeSink
	if sb := s.cfg.ScheduledBots; sb != nil {
		sinks = append(sinks, runOutcomeSink{
			name:     "schedule",
			recordID: scheduleOutcomeRecordID,
			mark: func(ctx context.Context, _, id string, o runOutcome) error {
				err := sb.MarkRunOutcome(ctx, id, o.RunID, o.Status, o.Error, o.ErrorCode, o.At)
				if errors.Is(err, cloudsched.ErrNotFound) {
					// store.RunSource.ScheduleID carries BOTH a
					// cloudsched.ScheduledBot id and a
					// trigger.Subscription id (its own doc says so), so a
					// trigger-fired run reaches this sink and finds no
					// schedule. Not an outage: the sibling sink owns that
					// record. A purged schedule reads the same way.
					return nil
				}
				return err
			},
		})
	}
	if subs := s.cfg.TriggerStore; subs != nil {
		sinks = append(sinks, runOutcomeSink{
			name: "trigger-subscription",
			// Same back-reference field as the schedule sink, by design:
			// store.RunSource.ScheduleID holds a cloudsched id OR a
			// trigger.Subscription id. Both sinks are offered the run and
			// the one that does not own the record answers not-found.
			recordID: scheduleOutcomeRecordID,
			mark: func(ctx context.Context, _, id string, o runOutcome) error {
				err := subs.MarkRunOutcome(ctx, id, o.RunID, o.Status, o.Error, o.ErrorCode, o.At)
				if errors.Is(err, trigger.ErrSubscriptionNotFound) {
					return nil
				}
				return err
			},
		})
	}
	if deliveries := s.webhookDeliveries; deliveries != nil {
		sinks = append(sinks, runOutcomeSink{
			name:     "webhook-delivery",
			recordID: deliveryOutcomeRecordID,
			mark: func(ctx context.Context, _, id string, o runOutcome) error {
				err := deliveries.MarkRunOutcome(ctx, id, o.RunID, o.Status, o.Error, o.ErrorCode, o.At)
				if errors.Is(err, webhooks.ErrNotFound) {
					// The audit row aged out of its TTL window (90 days by
					// default) while the run it launched was still around, or
					// the webhook config was deleted with its deliveries. Not
					// an outage.
					return nil
				}
				return err
			},
		})
	}
	if boardFor := s.cfg.CloudBoardFor; boardFor != nil {
		sinks = append(sinks, runOutcomeSink{
			name:     "board-card",
			recordID: boardOutcomeRecordID,
			mark: func(_ context.Context, tenantID, id string, o runOutcome) error {
				// The board is resolved per tenant, and the store's own
				// methods carry no ctx (native.BoardStore is ctx-less).
				board := boardFor(tenantID)
				if board == nil {
					return nil
				}
				err := board.SetRunOutcome(id, o.RunID, o.Status, o.Error, o.ErrorCode, o.At)
				if errors.Is(err, tracker.ErrNotFound) {
					// A deleted card, or an IssueID from a FOREIGN tracker
					// ("github:repo#123"): the native board does not own it.
					return nil
				}
				return err
			},
		})
	}
	return sinks
}

// deliveryOutcomeRecordID is the inbound-delivery back-reference the webhook
// launch path stamps (store.RunSource.DeliveryID, ARB-41 option B). Gated on
// the Kind too, unlike the board's id-only reading: `delivery_id` has exactly
// one producer, so a value without the webhook kind would be a corrupt
// record rather than a launch path that stamped an id and no kind.
func deliveryOutcomeRecordID(run *store.Run) string {
	if run == nil || run.Source == nil || run.Source.Kind != store.RunSourceKindWebhook {
		return ""
	}
	return run.Source.DeliveryID
}

// boardOutcomeRecordID is the card back-reference ADR-046 put on every
// board-dispatched run. Gated on the id alone, not on Source.Kind: one
// launch path stamps IssueID without a Kind (the ticket-lane admission),
// and the board store's own not-found answer is what filters an id it does
// not own.
func boardOutcomeRecordID(run *store.Run) string {
	if run == nil || run.Source == nil {
		return ""
	}
	return run.Source.IssueID
}

// scheduleOutcomeRecordID is the schedule-cadence back-reference: the
// schedule identity the launcher stamped on the run it fired. One field
// serves both cadence records — store.RunSource.ScheduleID is documented as
// carrying a cloudsched.ScheduledBot id (cloud resident scheduler) or a
// trigger.Subscription id (the trigger spine).
func scheduleOutcomeRecordID(run *store.Run) string {
	if run == nil || run.Source == nil {
		return ""
	}
	return run.Source.ScheduleID
}

// startRunOutcomeSpine attaches the spine's fast path to the event bus and
// starts its sweep net.
//
// No-op unless at least one sink is wired: self-hosted mode has none of
// the cloud record stores, and the records never fire there anyway. The
// sweep additionally needs cfg.NotifiableRuns (the store's terminal-run
// window scan, wired in cloud mode): without it the spine degrades to the
// event-only behaviour #1426 shipped, and says so rather than pretending
// to a net it does not have.
func (s *Server) startRunOutcomeSpine() {
	if s == nil || s.runs == nil {
		return
	}
	if len(s.runOutcomeSinks()) == 0 {
		return
	}
	if bus := s.eventsBus(); bus != nil {
		cancel, err := bus.Subscribe(runOutcomeSpineName, trigger.Matcher{
			Sources: []trigger.Source{trigger.SourceRun},
			Kinds: []string{
				trigger.KindRunFinished,
				trigger.KindRunFailed,
				trigger.KindRunCancelled,
			},
		}, s.handleRunOutcomeEvent)
		if err != nil {
			s.logWarn("server: run-outcome spine subscribe failed: %v — the sweep net still runs", err)
		} else {
			s.runOutcomeSpineCancel = cancel
		}
	}
	net := "event-only (no terminal-run window scan wired)"
	if s.cfg.NotifiableRuns != nil {
		s.goUntilShutdown("server.runOutcomeSweep", s.runOutcomeSweepLoop)
		net = "event fast path + store sweep"
	}
	// Which MODE the spine is in, said once, at Info: a server with no
	// window scan is a correct configuration (single-host, where the
	// in-process bus is not lossy), so it is not a warning — but a cloud
	// replica that lost its sweep must be readable from its own boot log.
	if s.logger != nil {
		s.logger.Info("server: run-outcome spine attached, %s — every record that launched a run reports what the run did", net)
	}
}

// handleRunOutcomeEvent is the eventbus.Handler for the fast path: no
// grace, because the event IS the signal that the terminal write landed.
func (s *Server) handleRunOutcomeEvent(ctx context.Context, ev trigger.Event) error {
	s.stampRunOutcome(ctx, ev.Subject.ID, time.Time{})
	return nil
}

// runOutcomeSweepLoop is the authority: every interval it re-offers the
// terminals of the window, so a run whose event never reached a subscriber
// is still stamped.
func (s *Server) runOutcomeSweepLoop(ctx context.Context) {
	t := time.NewTicker(spineSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// The pass runs detached: a shutdown stops the loop from
			// taking new work, it does not cut a stamp mid-write. The
			// join is what bounds it.
			s.runOutcomeSweepPass(context.WithoutCancel(ctx), time.Now())
		}
	}
}

// runOutcomeSweepPass walks one window, newest first, keyset-paginated on
// updated_at.
func (s *Server) runOutcomeSweepPass(ctx context.Context, now time.Time) {
	list := s.cfg.NotifiableRuns
	if list == nil {
		return
	}
	fctx := store.WithoutTenantFilter(ctx)
	since := now.Add(-spineSweepLookback)
	cutoff := now.Add(-spineSweepGrace)
	var before time.Time
	for page := 0; page < spineSweepMaxPages; page++ {
		refs, err := list(fctx, since, before, spineSweepBatch)
		if err != nil {
			s.logWarn("server: run-outcome spine sweep: list runs: %v", err)
			return
		}
		for _, ref := range refs {
			select {
			case <-ctx.Done():
				return
			default:
			}
			// Judge the LISTING before loading the run. The scan also
			// returns every run parked on a human gate — not time-bounded,
			// so a fleet's long-lived pauses come back on every pass — and
			// those cost nothing here instead of a run load each. The
			// listing is a projection read at scan time: a row the run has
			// since left is simply skipped this pass and offered by the
			// next one, 60 s later.
			if !store.RunStatus(ref.Status).IsTerminal() {
				continue
			}
			// The grace is NOT re-checked here: stampRunOutcome enforces
			// it on the run document, which both paths cross. A second
			// copy against the listing would only shadow it — two guards
			// covering each other, neither witnessable.
			s.stampRunOutcome(ctx, ref.ID, cutoff)
		}
		if len(refs) < spineSweepBatch {
			return
		}
		last := refs[len(refs)-1].UpdatedAt
		if last.IsZero() || (!before.IsZero() && !last.Before(before)) {
			// A lister without updated_at cannot cursor — stop rather
			// than loop on the same page (usernotify's own bound).
			return
		}
		before = last
	}
}

// stampRunOutcome offers one run to every sink. updatedBefore, when
// non-zero, is the sweep's grace cutoff: a run touched after it is left to
// the event path.
//
// Every refusal below is silent by design — the sweep offers plenty of runs
// that belong to no record — except a store read or write that FAILED,
// which is an outage and is warned.
func (s *Server) stampRunOutcome(ctx context.Context, runID string, updatedBefore time.Time) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return
	}
	sinks := s.runOutcomeSinks()
	if len(sinks) == 0 {
		return
	}
	// Snapshot the hot-swappable run service the same way every other
	// consumer does (handleRunsStats): a project switch replaces s.runs
	// under stateMu, and this runs on a bus goroutine that races that
	// swap — reading the field twice unlocked can hand it a half-written
	// interface value (#1510 R5e0731).
	s.stateMu.RLock()
	runsSvc := s.runs
	s.stateMu.RUnlock()
	if runsSvc == nil {
		return
	}
	rs := runsSvc.RunStore()
	if rs == nil {
		return
	}
	// Load with the tenant filter OFF: the spine is a platform-level
	// consumer (queue group "run-outcome-spine" on cloud), not scoped to
	// any one tenant.
	run, err := rs.LoadRun(store.WithoutTenantFilter(ctx), runID)
	if err != nil {
		// A purged run is expected (retention window) and stays silent —
		// the record's own dispatch fact survives. Any OTHER load error is
		// an outage, not a purge.
		if !errors.Is(err, store.ErrRunNotFound) {
			s.logWarn("server: run-outcome spine: load run %s: %v", runID, err)
		}
		return
	}
	if run == nil {
		return
	}
	// The terminal gate. The sweep's listing already filters statuses, but
	// the bus path cannot, and one explicit gate serves both: a cancelled
	// run IS an outcome a record must report (unlike the router, which
	// never auto-acts on an operator's stop).
	if !run.Status.IsTerminal() {
		return
	}
	if !updatedBefore.IsZero() && run.UpdatedAt.After(updatedBefore) {
		return
	}
	o := runOutcome{RunID: runID, Status: string(run.Status), At: run.UpdatedAt}
	if run.Status != store.RunStatusFinished {
		o.Error = run.Error
		o.ErrorCode = string(run.FailureCode)
	}
	// Scope the write to the record's tenant (the run's TenantID, stamped
	// when it was launched). The stores honour WithoutTenantFilter above;
	// the Mongo paths filter by _id only, so this is defensive but correct.
	tctx := store.WithTenant(store.WithoutTenantFilter(ctx), run.TenantID)
	for _, sink := range sinks {
		id := sink.recordID(run)
		if id == "" {
			continue
		}
		if err := sink.mark(tctx, run.TenantID, id, o); err != nil {
			s.logWarn("server: run-outcome spine: %s: mark run %s on %s: %v", sink.name, runID, id, err)
		}
	}
}
