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
//     Every replica polls; the per-tenant CAS cursor
//     (AdvanceTriggerCursor) elects the batch's publisher, so each board
//     event enters the NATS bus exactly once. The tailed tenant set is
//     the tenants holding enabled board-kind subscriptions.
//   - StartCloudTriggerCoordinator: evaluator subscription on the shared
//     bus + the source. The cloud board DISPATCHER (boarddispatch.go, 5s
//     poll) remains the promote path's launch authority and safety net —
//     no nudger exists (or is needed) on cloud.

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

// cloudBoardSource poll-tails each subscribed tenant's board_events and
// publishes the card transitions onto the bus.
type cloudBoardSource struct {
	coord   *boardmongo.Coordinator
	tenants cloudTenantLister
	bus     eventbus.Bus
	logger  *iterlog.Logger
	tick    time.Duration
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

// tickOnce drains every subscribed tenant's new events. Per tenant: read the
// cursor, fetch the batch, CAS-advance; only the winning replica publishes.
func (s *cloudBoardSource) tickOnce() {
	tenants, err := s.tenants.DistinctBoardTenants(s.ctx)
	if err != nil {
		s.warn("trigger: cloud board source: list tenants: %v", err)
		return
	}
	for _, tenant := range tenants {
		if err := s.drainTenant(tenant); err != nil && s.ctx.Err() == nil {
			s.warn("trigger: cloud board source: tenant %s: %v", tenant, err)
		}
	}
}

func (s *cloudBoardSource) drainTenant(tenant string) error {
	st := s.coord.StoreFor(tenant)
	cursor, err := st.TriggerCursor()
	if err != nil {
		return err
	}
	events, err := st.EventsAfter(cursor, 200)
	if err != nil || len(events) == 0 {
		return err
	}
	last := events[len(events)-1].Seq
	won, err := st.AdvanceTriggerCursor(cursor, last)
	if err != nil || !won {
		return err // another replica owns this batch
	}
	for _, evt := range events {
		if !trigger.IsCardEvent(evt.Type) || evt.IssueID == "" {
			continue
		}
		te, ok := trigger.NormalizeBoardEvent(st.Get, evt, tenant, "", "cloud")
		if !ok {
			continue
		}
		if err := s.bus.Publish(s.ctx, te); err != nil {
			s.warn("trigger: cloud board source: publish %s: %v", te.ID, err)
		}
	}
	return nil
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
		bus:     bus,
		logger:  logger,
		tick:    cloudBoardTickInterval(),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	go src.run()
	if logger != nil {
		logger.Info("server: cloud trigger spine active (board_events poll every %s)", src.tick)
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
