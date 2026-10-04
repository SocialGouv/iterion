// The sovereign-pool reconciler (#2029, plan v2.1 D4'): the SERVER owns the
// per-pool topology — a pool entry that is not disabled gets its run stream
// and DLQ stream ensured on every pass, and an active/draining pool gets
// its durable consumer (KEDA needs it to exist; a pool runner ATTACHES an
// existing consumer rather than creating one). Idempotent by construction
// (CreateOrUpdate everywhere): two replicas reconcile the same registry to
// the same topology, and a pass that fails is healed by the next one — the
// same self-healing contract as EnsureSchema.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/queue/nats"
)

// poolTopology is the slice of the queue connection the reconciler drives.
type poolTopology interface {
	EnsurePoolSchema(ctx context.Context, pool string) error
	PreparePoolConsumer(ctx context.Context, pool string) (any, error)
}

// reconcileRunnerPools brings the topology in line with the registry:
// every non-disabled entry gets its streams; an active or draining entry
// also gets its consumer. Errors are collected — one broken pool must not
// starve the others, and the next pass heals whatever this one missed.
func (s *Server) reconcileRunnerPools(ctx context.Context, topo poolTopology) error {
	if s.runnerPoolsStore == nil || topo == nil {
		return nil
	}
	rec, err := s.runnerPoolsStore.Get(ctx)
	if err != nil {
		return fmt.Errorf("server: read the runner-pool registry: %w", err)
	}
	if rec == nil {
		return nil
	}
	var errs []error
	for _, p := range rec.Pools {
		if p.State == platformcfg.RunnerPoolDisabled {
			continue
		}
		if err := topo.EnsurePoolSchema(ctx, p.Name); err != nil {
			errs = append(errs, fmt.Errorf("pool %s: %w", p.Name, err))
			continue
		}
		if p.State == platformcfg.RunnerPoolActive || p.State == platformcfg.RunnerPoolDraining {
			if _, err := topo.PreparePoolConsumer(ctx, p.Name); err != nil {
				errs = append(errs, fmt.Errorf("pool %s consumer: %w", p.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// RunRunnerPoolReconciler reconciles immediately and then on every tick
// until ctx ends — the cmd-level entry over the real connection.
func (s *Server) RunRunnerPoolReconciler(ctx context.Context, topo poolTopology, every time.Duration, logf func(format string, args ...any)) {
	s.runRunnerPoolReconciler(ctx, topo, every, logf)
}

// runRunnerPoolReconciler reconciles immediately and then on every tick
// until ctx ends. Registry reads and topology writes are idempotent, so
// two replicas run this side by side without coordination.
func (s *Server) runRunnerPoolReconciler(ctx context.Context, topo poolTopology, every time.Duration, logf func(format string, args ...any)) {
	if s.runnerPoolsStore == nil || topo == nil {
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	reconcile := func() {
		if err := s.reconcileRunnerPools(ctx, topo); err != nil && logf != nil {
			logf("server: %v (retrying next tick)", err)
		}
	}
	reconcile()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			reconcile()
		}
	}
}

// joinErrors is a local multi-error join that keeps nil for an empty list.
func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		joined := errs[0]
		for _, e := range errs[1:] {
			joined = fmt.Errorf("%v; %w", joined, e)
		}
		return joined
	}
}

// natsPoolTopology adapts the real queue connection to poolTopology (the
// consumer's concrete type is narrowed to any — the reconciler discards it;
// the runner loop holds the real consumer).
type natsPoolTopology struct{ conn *nats.Conn }

func (t natsPoolTopology) EnsurePoolSchema(ctx context.Context, pool string) error {
	return t.conn.EnsurePoolSchema(ctx, pool)
}

func (t natsPoolTopology) PreparePoolConsumer(ctx context.Context, pool string) (any, error) {
	return t.conn.PreparePoolConsumer(ctx, pool)
}
