package runview

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/errtrack"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

var ErrSubbotLivenessUnavailable = errors.New("subbot requires a cross-process run lock before birth")
var ErrSubbotLeaseExpired = errors.New("subbot run lease reached its last acknowledged expiry")

// SubbotLeaseOptions configures the host's advisory liveness, not execution
// authority. Zero cadence derives a conservative interval from the lease TTL.
type SubbotLeaseOptions struct {
	HeartbeatInterval time.Duration
	OnRefreshError    func(error)
}

// SubbotLease keeps preparation and execution under one liveness signal.
// Context retains the original parent's format until child preparation. Release
// cancels that active lifetime; a parked parent must use its original context.
type SubbotLease struct {
	Context           context.Context
	release           func() error
	reservation       store.RunBirthReservation
	parentID, childID string
}

func (l *SubbotLease) Release() error { return l.release() }

// AcquireSubbotLease precedes both the child row and its parent linkage. A
// refreshable provider must declare the effective stable lease TTL and honor
// Refresh cancellation. The local expiry cancels independently of a slow RPC;
// it is not fencing for already-started external effects or a suspended process.
func AcquireSubbotLease(ctx context.Context, runs store.RunStore, parentID, childID string, opts SubbotLeaseOptions) (*SubbotLease, error) {
	if ctx == nil || runs == nil || !runs.Capabilities().CrossProcessLock {
		return nil, ErrSubbotLivenessUnavailable
	}
	format, err := CheckSubbotBirthInvocation(ctx, parentID)
	if err != nil {
		return nil, err
	}
	births := store.AsRunBirthStore(runs)
	if births == nil {
		return nil, store.ErrRunBirthUnavailable
	}
	active, cancel := context.WithCancelCause(ctx)
	lockCtx, err := store.WithRunValueFormat(active, store.RunValueFormat{RunID: childID, Format: format})
	if err != nil {
		cancel(nil)
		return nil, err
	}
	if err := active.Err(); err != nil {
		cancel(nil)
		return nil, err
	}
	started := time.Now()
	lock, err := births.BeginRunBirth(lockCtx, childID)
	if err != nil {
		cancel(nil)
		return nil, fmt.Errorf("subbot %s: acquire run lock: %w", childID, err)
	}
	if lock == nil {
		cancel(nil)
		return nil, ErrSubbotLivenessUnavailable
	}
	refuse := func(err error) (*SubbotLease, error) { cancel(nil); return nil, errors.Join(err, lock.Unlock()) }
	if err := active.Err(); err != nil {
		return refuse(err)
	}
	refresh, renewable := lock.(interface{ Refresh(context.Context) error })
	done := make(chan struct{})
	var mu sync.Mutex
	var expiry time.Time
	var timer *time.Timer
	stopped := false
	var releaseOnce sync.Once
	var releaseErr error
	result := &SubbotLease{Context: active, reservation: lock, parentID: parentID, childID: childID}
	result.release = func() error {
		releaseOnce.Do(func() {
			mu.Lock()
			stopped = true
			if timer != nil {
				timer.Stop()
			}
			mu.Unlock()
			cancel(nil)
			<-done
			releaseErr = lock.Unlock()
		})
		return releaseErr
	}
	if !renewable {
		close(done)
		return result, nil
	}
	declaration, ok := lock.(interface{ LeaseTTL() time.Duration })
	if !ok {
		return refuse(fmt.Errorf("%w: refreshable lock does not declare its lease TTL", ErrSubbotLivenessUnavailable))
	}
	ttl := declaration.LeaseTTL()
	interval := opts.HeartbeatInterval
	if interval <= 0 {
		interval = min(20*time.Second, ttl/3)
	}
	if ttl <= 0 || interval <= 0 || interval >= ttl {
		return refuse(fmt.Errorf("%w: heartbeat interval %s must be positive and below lease TTL %s", ErrSubbotLivenessUnavailable, interval, ttl))
	}
	expiry = started.Add(ttl)
	if !time.Now().Before(expiry) {
		return refuse(ErrSubbotLeaseExpired)
	}
	fail := func(err error, expiredOnly bool) {
		mu.Lock()
		if stopped || (expiredOnly && time.Now().Before(expiry)) {
			mu.Unlock()
			return
		}
		stopped = true
		if timer != nil {
			timer.Stop()
		}
		cancel(errors.Join(runtime.ErrRunInterrupted, err))
		mu.Unlock()
		if opts.OnRefreshError != nil {
			opts.OnRefreshError(err)
		}
	}
	mu.Lock()
	timer = time.AfterFunc(time.Until(expiry), func() {
		fail(ErrSubbotLeaseExpired, true)
	})
	mu.Unlock()
	errtrack.Go("runview.subbot.lease", func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-active.Done():
				return
			case <-ticker.C:
				mu.Lock()
				deadline := expiry
				isStopped := stopped
				mu.Unlock()
				if isStopped {
					return
				}
				requestStarted := time.Now()
				if !requestStarted.Before(deadline) {
					fail(ErrSubbotLeaseExpired, false)
					return
				}
				refreshCtx, stop := context.WithDeadline(lockCtx, deadline)
				err := refresh.Refresh(refreshCtx)
				stop()
				if err != nil {
					if active.Err() == nil {
						fail(err, false)
					}
					return
				}
				next := requestStarted.Add(ttl)
				mu.Lock()
				if stopped || active.Err() != nil {
					mu.Unlock()
					return
				}
				if !time.Now().Before(deadline) || !time.Now().Before(next) {
					mu.Unlock()
					fail(ErrSubbotLeaseExpired, false)
					return
				}
				expiry = next
				timer.Reset(time.Until(expiry))
				mu.Unlock()
			}
		}
	})
	return result, nil
}

// SubbotParkContext preserves the parent's cancellation while borrowing only
// the immutable child's binding, never the active lease's cancelled lifetime.
func SubbotParkContext(parent context.Context, p store.PreparedRunInvocation) (context.Context, error) {
	binding, _, ok := p.OriginalBirth()
	if !ok {
		return nil, fmt.Errorf("%w: subbot park lacks its original birth", store.ErrValueFormatCapability)
	}
	parent = model.WithoutOutputCorrectionOwnership(model.WithExecutionUsageCollector(parent, nil))
	return store.WithRunValueFormat(parent, binding)
}
