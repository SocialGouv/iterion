package server

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/runwatch"
)

// assistantWatchContextStore guards every mutation initiated by the worker.
// Some stores deliberately ignore context cancellation. After a blocked read
// returns during shutdown, it must not lead to another durable side effect.
// An operation already executing inside a store cannot be revoked here.
type assistantWatchContextStore struct{ runwatch.Store }

func (s assistantWatchContextStore) CreateWatch(ctx context.Context, w runwatch.Watch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.CreateWatch(ctx, w)
}

func (s assistantWatchContextStore) ReconfigureActiveWatch(ctx context.Context, w runwatch.Watch) (runwatch.Watch, bool, error) {
	if err := ctx.Err(); err != nil {
		return runwatch.Watch{}, false, err
	}
	return s.Store.ReconfigureActiveWatch(ctx, w)
}

func (s assistantWatchContextStore) TransferActiveWatch(ctx context.Context, from string, w runwatch.Watch) (runwatch.Watch, bool, error) {
	if err := ctx.Err(); err != nil {
		return runwatch.Watch{}, false, err
	}
	return s.Store.TransferActiveWatch(ctx, from, w)
}

func (s assistantWatchContextStore) StopWatch(ctx context.Context, id, tenant string, state runwatch.WatchState, reason string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.StopWatch(ctx, id, tenant, state, reason, now)
}

func (s assistantWatchContextStore) AdvanceObservedEventSeq(ctx context.Context, id, tenant string, seq int64, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.AdvanceObservedEventSeq(ctx, id, tenant, seq, now)
}

func (s assistantWatchContextStore) InitializeTreeTracking(ctx context.Context, id, tenant string, started, now time.Time) (runwatch.Watch, error) {
	if err := ctx.Err(); err != nil {
		return runwatch.Watch{}, err
	}
	return s.Store.InitializeTreeTracking(ctx, id, tenant, started, now)
}

func (s assistantWatchContextStore) EnsureRunObservation(ctx context.Context, id, tenant string, observation runwatch.RunObservation, now time.Time) (runwatch.RunObservation, bool, error) {
	if err := ctx.Err(); err != nil {
		return runwatch.RunObservation{}, false, err
	}
	return s.Store.EnsureRunObservation(ctx, id, tenant, observation, now)
}

func (s assistantWatchContextStore) AdvanceObservedRunEventSeq(ctx context.Context, id, tenant, run string, seq int64, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.AdvanceObservedRunEventSeq(ctx, id, tenant, run, seq, now)
}

func (s assistantWatchContextStore) CreateEpisode(ctx context.Context, ep runwatch.Episode) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.Store.CreateEpisode(ctx, ep)
}

func (s assistantWatchContextStore) ClaimEpisode(ctx context.Context, id, worker string, now time.Time, lease time.Duration) (runwatch.Episode, bool, error) {
	if err := ctx.Err(); err != nil {
		return runwatch.Episode{}, false, err
	}
	return s.Store.ClaimEpisode(ctx, id, worker, now, lease)
}

func (s assistantWatchContextStore) ReleaseEpisode(ctx context.Context, id, worker string, next time.Time, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.ReleaseEpisode(ctx, id, worker, next, reason)
}

func (s assistantWatchContextStore) CompleteEpisode(ctx context.Context, id, worker string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.CompleteEpisode(ctx, id, worker, now)
}

func (s assistantWatchContextStore) BlockEpisode(ctx context.Context, id, worker string, now time.Time, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.BlockEpisode(ctx, id, worker, now, reason)
}
