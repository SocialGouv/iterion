package server

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// This file is the CLOUD half of the trigger spine's board wiring — the
// multi-tenant counterpart of trigger_coordinator.go's local
// StartTriggerCoordinator. Three pieces:
//
//   - cloudBoardEffect: BoardEffect + LabelConsumer over the per-tenant
//     Mongo board (promote = stamp Bot for the board dispatcher's claim;
//     consume = boardmongo.ConsumeLabels, a single atomic UpdateOne so
//     concurrent replicas cannot double-launch a consume_labels trigger).
//   - cloudBoardSource: a poll-tailer over the board_events collection.
//     Every replica polls; matched (event, subscription) pairs are
//     materialized into the durable effect outbox BEFORE the per-tenant
//     CAS cursor advances, and executed by the EffectWorker under leased
//     claims (ADR-094 — board events no longer ride the lossy bus). The
//     tailed tenant set is the tenants holding enabled board-kind
//     subscriptions.
//   - StartCloudTriggerCoordinator: evaluator subscription on the shared
//     bus (run-outcome and other non-board sources) + the source. The
//     cloud board DISPATCHER (boarddispatch.go, 5s poll) remains the
//     promote path's launch authority and safety net — no nudger exists
//     (or is needed) on cloud.

// defaultCloudBoardTickInterval paces the board_events poll. Override with
// ITERION_CLOUD_BOARD_TICK (a Go duration).
const defaultCloudBoardTickInterval = 3 * time.Second

func cloudBoardTickInterval() time.Duration {
	if v := os.Getenv("ITERION_CLOUD_BOARD_TICK"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultCloudBoardTickInterval
}

// cloudBoardEffect realises board-mode plans against the multi-tenant Mongo
// board. Unlike the local NativeBoardEffect (bound to ONE store), it
// resolves the tenant store per call from the plan/event tenant.
type cloudBoardEffect struct {
	coord  *boardmongo.Coordinator
	logger *iterlog.Logger
}

func (c *cloudBoardEffect) storeFor(tenantID string) (*boardmongo.Store, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("trigger: cloud board effect needs a tenant id")
	}
	return c.coord.StoreFor(tenantID), nil
}

// Promote stamps the matched card's bot + bot-args so the cloud board
// dispatcher's Claim picks it up on its next 5s tick. Idempotent, mirroring
// the local effect.
func (c *cloudBoardEffect) Promote(_ context.Context, plan trigger.LaunchPlan) (string, error) {
	st, err := c.storeFor(firstNonEmptyStr(plan.TenantID, plan.Event.TenantID))
	if err != nil {
		return "", err
	}
	id := plan.Event.Subject.ID
	if id == "" {
		return "", fmt.Errorf("trigger: board plan has no card id")
	}
	iss, err := st.Get(id)
	if err != nil {
		return "", fmt.Errorf("trigger: get card %s: %w", id, err)
	}
	if iss.Bot == plan.BotID {
		return id, nil // already pinned — no churn
	}
	bot := plan.BotID
	args := plan.Vars
	if _, err := st.Update(id, native.Patch{Bot: &bot, BotArgs: &args}); err != nil {
		return "", fmt.Errorf("trigger: stamp bot on card %s: %w", id, err)
	}
	return id, nil
}

// ConsumeMatchLabels delegates to the store's atomic single-update consume —
// the multi-replica-safe one-shot gate.
func (c *cloudBoardEffect) ConsumeMatchLabels(_ context.Context, tenantID, issueID string, labels []string) (bool, error) {
	st, err := c.storeFor(tenantID)
	if err != nil {
		return false, err
	}
	return st.ConsumeLabels(issueID, labels)
}

var _ trigger.BoardEffect = (*cloudBoardEffect)(nil)
var _ trigger.LabelConsumer = (*cloudBoardEffect)(nil)

// cloudTenantLister narrows the subscription store to the one extra query
// the source needs (satisfied by trigger.MongoSubscriptionStore).
type cloudTenantLister interface {
	DistinctBoardTenants(ctx context.Context) ([]string, error)
}

// cloudBoardStore is the slice of the per-tenant Mongo store the tail reads
// and writes — an interface so drainTenant's loss-ordering contract is unit-
// testable with a stub (satisfied by *boardmongo.Store).
type cloudBoardStore interface {
	TriggerCursor() (int64, error)
	EventsAfter(afterSeq int64, limit int) ([]native.Event, error)
	AdvanceTriggerCursor(from, to int64) (bool, error)
	Get(id string) (*native.Issue, error)
	trigger.EffectOutbox
}

// cloudBoardSource poll-tails each subscribed tenant's board_events,
// materializes matched (event, subscription) pairs into the durable effect
// outbox (ADR-094), and drains that outbox through the EffectWorker. Board
// events no longer ride the lossy bus at all on cloud: the outbox IS the
// delivery, with a leased claim and bounded retries where the bus had
// at-most-once and warn-only losses.
type cloudBoardSource struct {
	coord   *boardmongo.Coordinator
	tenants cloudTenantLister
	subs    trigger.SubscriptionStore
	eval    *trigger.Evaluator
	logger  *iterlog.Logger
	tick    time.Duration
	// ticks counts tickOnce passes (single goroutine) for the slow-cadence
	// outbox drain.
	ticks int
	// holes tracks, per tenant, the first seq gap the tail is currently
	// refusing to advance past (emit allocates the seq BEFORE inserting, so
	// a younger gap is usually an insert still in flight — skipping it
	// would lose the event forever). Older than boardTailHoleGrace = a dead
	// allocation (failed insert), franchissable. Single goroutine.
	holes map[string]tailHole
	// poisons tracks, per tenant, the event seq whose normalize/match keeps
	// failing; past boardTailPoisonTicks the tail logs once at Error and
	// skips it — a head-of-line poison event must not freeze the tenant's
	// triggers forever. Single goroutine.
	poisons map[string]tailPoison
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

func (s *cloudBoardSource) run() {
	defer close(s.done)
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.tickOnce()
		}
	}
}

// tickOnce, per subscribed tenant: materialize new board events into the
// effect outbox, then drain due effect rows. Every replica ticks; the cursor
// CAS dedups materialization work and the outbox's atomic claim dedups
// execution — a lost race on either is idle, never a double effect.
func (s *cloudBoardSource) tickOnce() {
	tenants, err := s.tenants.DistinctBoardTenants(s.ctx)
	if err != nil {
		s.warn("trigger: cloud board source: list tenants: %v", err)
		return
	}
	// The drain pass runs every tick (it is two indexed reads when idle);
	// the effect worker's claim query only runs when this tick materialized
	// rows (the latency-sensitive case) or on the slow cadence — retries
	// and lease reclaims have a ≥15s backoff, so polling the outbox with a
	// primary-routed FindOneAndUpdate every 3s per tenant per replica would
	// buy nothing but load.
	s.ticks++
	slowTick := s.ticks%effectDrainEveryNTicks == 0
	if slowTick {
		// Union with the tenants holding live effect rows: disabling a
		// tenant's last board subscription must not hibernate its already-
		// materialized rows (they would fire all at once on re-enable,
		// days late) — they still execute, retry or park on the slow
		// cadence.
		if extra, err := s.coord.DistinctEffectTenants(s.ctx); err != nil {
			s.warn("trigger: cloud board source: list effect tenants: %v", err)
		} else {
			seen := make(map[string]bool, len(tenants))
			for _, t := range tenants {
				seen[t] = true
			}
			for _, t := range extra {
				if !seen[t] {
					tenants = append(tenants, t)
				}
			}
		}
	}
	for _, tenant := range tenants {
		st := s.coord.StoreFor(tenant)
		materialized, err := s.drainTenant(tenant, st)
		if err != nil && s.ctx.Err() == nil {
			s.warn("trigger: cloud board source: tenant %s: %v", tenant, err)
		}
		if materialized || slowTick {
			w := &trigger.EffectWorker{Outbox: st, Subs: s.subs, Evaluator: s.eval, Logger: s.logger}
			w.Tick(s.ctx, 20)
		}
	}
}

// effectDrainEveryNTicks paces the outbox claim poll when nothing was just
// materialized: every 5th board tick (~15s at the default 3s interval),
// matching the smallest retry backoff.
const effectDrainEveryNTicks = 5

// boardTailHoleGrace bounds how long the tail waits on a seq gap before
// treating it as a dead allocation. Must exceed boardmongo's per-op timeout
// (10s) so a slow-but-live insert always lands inside the grace.
const boardTailHoleGrace = 30 * time.Second

// boardTailPoisonTicks is how many consecutive failed reads of ONE event the
// tail tolerates before skipping it (with a single Error log).
const boardTailPoisonTicks = 20

type tailHole struct {
	seq       int64
	firstSeen time.Time
}

type tailPoison struct {
	seq   int64
	fails int
	said  bool
}

// drainTenant materializes one tenant's new board events. The ORDER is the
// whole point (the pre-outbox shape lost events forever on a crash or a
// publish error after the cursor had advanced):
//
//  1. normalize + match EVERY event of the batch — a transient store error
//     aborts the batch with the cursor untouched (retry next tick); only a
//     definitively deleted card is skipped;
//  2. write the matched pairs to the durable outbox (idempotent upserts —
//     a racing replica's duplicates collapse on the row key);
//  3. only then CAS-advance the cursor. A crash before 3 re-materializes
//     idempotently; after 3 the rows are already durable.
func (s *cloudBoardSource) drainTenant(tenant string, st cloudBoardStore) (bool, error) {
	cursor, err := st.TriggerCursor()
	if err != nil {
		return false, err
	}
	events, err := st.EventsAfter(cursor, 200)
	if err != nil || len(events) == 0 {
		return false, err
	}
	now := time.Now().UTC()
	// Contiguous-prefix guard: emit allocates the seq BEFORE inserting, so
	// a concurrent emitter can make seq N visible while N-1's insert is
	// still in flight — advancing over that gap would lose N-1 forever.
	// Truncate the batch at the first YOUNG gap (watched < grace); a gap
	// older than the grace is a dead allocation (failed insert) the tail
	// steps over.
	events = s.trimAtYoungGap(tenant, cursor, events, now)
	if len(events) == 0 {
		return false, nil
	}
	var rows []trigger.EffectRow
	for _, evt := range events {
		if !trigger.IsCardEvent(evt.Type) || evt.IssueID == "" {
			continue
		}
		te, ok, nerr := trigger.NormalizeBoardEvent(st.Get, evt, tenant, "", "cloud")
		if nerr != nil {
			if s.poisoned(tenant, evt.Seq) {
				continue // logged once at Error inside poisoned — step over
			}
			return false, fmt.Errorf("normalize event seq %d (issue %s): %w — batch aborted before the cursor, will retry", evt.Seq, evt.IssueID, nerr)
		}
		if !ok {
			continue // card deleted between the transition and the read — definitive
		}
		evRows, merr := trigger.MaterializeEffects(s.ctx, s.subs, te, now)
		if merr != nil {
			if s.poisoned(tenant, evt.Seq) {
				continue
			}
			return false, fmt.Errorf("match subscriptions for event seq %d: %w — batch aborted before the cursor, will retry", evt.Seq, merr)
		}
		rows = append(rows, evRows...)
	}
	if len(rows) > 0 {
		if err := st.UpsertPending(s.ctx, rows); err != nil {
			return false, fmt.Errorf("materialize %d effect(s): %w — batch aborted before the cursor, will retry", len(rows), err)
		}
	}
	last := events[len(events)-1].Seq
	if _, err := st.AdvanceTriggerCursor(cursor, last); err != nil {
		// The rows are durable; a failed advance only means re-materializing
		// the same batch next tick, which the upsert collapses.
		return len(rows) > 0, err
	}
	return len(rows) > 0, nil
}

// trimAtYoungGap cuts the batch before the first seq gap the tail has not
// yet watched past the grace. Only the FIRST gap per tick is examined; the
// remainder is re-read next tick once the gap resolves or expires.
func (s *cloudBoardSource) trimAtYoungGap(tenant string, cursor int64, events []native.Event, now time.Time) []native.Event {
	if s.holes == nil {
		s.holes = map[string]tailHole{}
	}
	expected := cursor + 1
	for i, evt := range events {
		if evt.Seq == expected {
			expected = evt.Seq + 1
			continue
		}
		// Gap at [expected, evt.Seq-1].
		h, watching := s.holes[tenant]
		if !watching || h.seq != expected {
			s.holes[tenant] = tailHole{seq: expected, firstSeen: now}
			return events[:i]
		}
		if now.Sub(h.firstSeen) < boardTailHoleGrace {
			return events[:i]
		}
		// Dead allocation — a failed insert holds no event. Step over.
		s.warn("trigger: cloud board source: tenant %s: seq gap at %d unfilled for %s — treating as a dead allocation and advancing", tenant, expected, now.Sub(h.firstSeen))
		delete(s.holes, tenant)
		expected = evt.Seq + 1
	}
	delete(s.holes, tenant)
	return events
}

// poisoned counts consecutive failures on one event seq; past the threshold
// it logs ONCE at Error and answers true (skip). Head-of-line poison must
// not freeze every trigger behind it forever — but a skip is a deliberate,
// loud loss, never a silent one.
func (s *cloudBoardSource) poisoned(tenant string, seq int64) bool {
	if s.poisons == nil {
		s.poisons = map[string]tailPoison{}
	}
	p := s.poisons[tenant]
	if p.seq != seq {
		p = tailPoison{seq: seq}
	}
	p.fails++
	if p.fails < boardTailPoisonTicks {
		s.poisons[tenant] = p
		return false
	}
	if !p.said && s.logger != nil {
		s.logger.Error("trigger: cloud board source: tenant %s: event seq %d unreadable after %d ticks — SKIPPING it; the triggers behind it resume, this one is lost (inspect board_events seq %d)", tenant, seq, p.fails, seq)
		p.said = true
	}
	s.poisons[tenant] = p
	return true
}

func (s *cloudBoardSource) warn(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(format, args...)
	}
}

func (s *cloudBoardSource) Stop() {
	s.cancel()
	<-s.done
}

// CloudTriggerCoordinator holds the cloud spine's moving parts for Close.
type CloudTriggerCoordinator struct {
	source    *cloudBoardSource
	cancelSub func()
}

// StartCloudTriggerCoordinator wires the board half of the trigger spine for
// cloud mode: an evaluator (multi-tenant board effect + the standard service
// launcher) subscribed on the shared NATS bus, fed by the board_events
// poll-tail. Returns nil (a no-op, logged) when a prerequisite is missing.
// The schedule/keepalive kinds stay with cloudsched; forge events stay
// observational (webhooks remain the launch authority).
func StartCloudTriggerCoordinator(coord *boardmongo.Coordinator, subs trigger.SubscriptionStore, launcher trigger.Launcher, bus eventbus.Bus, logger *iterlog.Logger) *CloudTriggerCoordinator {
	if coord == nil || subs == nil || bus == nil {
		return nil
	}
	lister, ok := subs.(cloudTenantLister)
	if !ok {
		if logger != nil {
			logger.Warn("server: cloud trigger spine disabled (subscription store cannot list board tenants)")
		}
		return nil
	}
	effect := &cloudBoardEffect{coord: coord, logger: logger}
	eval := trigger.NewEvaluator(subs,
		trigger.WithBoardEffect(effect),
		trigger.WithLauncher(launcher),
		trigger.WithLogger(logger),
	)
	cancelSub, err := bus.Subscribe("trigger-evaluator", trigger.Matcher{}, eval.Handle)
	if err != nil {
		if logger != nil {
			logger.Warn("server: cloud trigger spine disabled (bus subscribe failed): %v", err)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	src := &cloudBoardSource{
		coord:   coord,
		tenants: lister,
		subs:    subs,
		eval:    eval,
		logger:  logger,
		tick:    cloudBoardTickInterval(),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go src.run()
	if logger != nil {
		logger.Info("server: cloud trigger spine active (board_events poll every %s, durable effect outbox)", src.tick)
	}
	return &CloudTriggerCoordinator{source: src, cancelSub: cancelSub}
}

// Close tears down the source and unsubscribes the evaluator. Nil-safe.
func (c *CloudTriggerCoordinator) Close() {
	if c == nil {
		return
	}
	if c.source != nil {
		c.source.Stop()
	}
	if c.cancelSub != nil {
		c.cancelSub()
	}
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
