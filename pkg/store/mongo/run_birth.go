package mongo

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type runBirth struct {
	mu         sync.Mutex
	store      *Store
	id         string
	format     store.ValueFormat
	lock       store.RunLock
	expiry     time.Time
	lost       bool
	used       bool
	released   bool
	releaseErr error
}

type renewableRunBirth struct {
	*runBirth
	lease interface {
		Refresh(context.Context) error
		LeaseTTL() time.Duration
	}
}

var _ store.RunBirthStore = (*Store)(nil)

// Mongo publishes no run row when acquiring its external lease. Keep the
// lease's request-time expiry through publication, rather than restarting its
// clock when a manager finally admits the child.
func (s *Store) BeginRunBirth(ctx context.Context, id string) (store.RunBirthReservation, error) {
	if ctx == nil {
		return nil, store.ErrInvalidValueFormat
	}
	if !s.Capabilities().CrossProcessLock {
		return nil, store.ErrRunBirthUnavailable
	}
	if err := store.RequireRunValueFormat(ctx, id); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	format, err := store.ValueFormatForWrite(ctx, id)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	lock, err := s.LockRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if lock == nil {
		return nil, store.ErrRunBirthUnavailable
	}
	b := &runBirth{store: s, id: id, format: format, lock: lock}
	fail := func(err error) (store.RunBirthReservation, error) { return nil, errors.Join(err, b.Unlock()) }
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	if refresh, ok := lock.(interface{ Refresh(context.Context) error }); ok {
		lease, ok := refresh.(interface {
			Refresh(context.Context) error
			LeaseTTL() time.Duration
		})
		if !ok || lease.LeaseTTL() <= 0 {
			return fail(store.ErrRunBirthUnavailable)
		}
		b.expiry = started.Add(lease.LeaseTTL())
		if !time.Now().Before(b.expiry) {
			return fail(store.ErrRunBirthExpired)
		}
		return &renewableRunBirth{runBirth: b, lease: lease}, nil
	}
	return b, nil
}

func (b *runBirth) Publish(ctx context.Context, spec store.RunCreateSpec) (*store.Run, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.released || b.used {
		return nil, store.ErrRunBirthState
	}
	b.used = true
	spec, err := spec.Normalize(ctx)
	if err != nil {
		return nil, err
	}
	if spec.ID != b.id || spec.Format != b.format {
		return nil, store.ErrValueFormatChanged
	}
	if b.lost || !b.expiry.IsZero() && !time.Now().Before(b.expiry) {
		return nil, store.ErrRunBirthExpired
	}
	if !b.expiry.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, b.expiry)
		defer cancel()
	}
	r, err := b.store.CreateRunWithValueFormat(ctx, spec)
	// Retain a successful exclusive-create receipt even if cancellation/lease
	// expiry arrives immediately after the transaction commits. No effects may
	// start on an expired acknowledgement; the complete row can be retired.
	if r != nil {
		err = errors.Join(err, ctx.Err())
		if !b.expiry.IsZero() && !time.Now().Before(b.expiry) {
			err = errors.Join(err, store.ErrRunBirthExpired)
		}
	}
	return r, err
}

func (b *runBirth) Unlock() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.released {
		return b.releaseErr
	}
	b.released = true
	b.releaseErr = b.lock.Unlock()
	return b.releaseErr
}

func (b *renewableRunBirth) LeaseTTL() time.Duration { return b.lease.LeaseTTL() }

func (b *renewableRunBirth) Refresh(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.released {
		return store.ErrRunBirthState
	}
	started := time.Now()
	if b.lost || !started.Before(b.expiry) {
		b.lost = true
		return store.ErrRunBirthExpired
	}
	ctx, cancel := context.WithDeadline(ctx, b.expiry)
	defer cancel()
	err := b.lease.Refresh(ctx)
	if err != nil || ctx.Err() != nil || !time.Now().Before(b.expiry) {
		b.lost = true
		return errors.Join(err, ctx.Err(), store.ErrRunBirthExpired)
	}
	b.expiry = started.Add(b.lease.LeaseTTL())
	return nil
}
