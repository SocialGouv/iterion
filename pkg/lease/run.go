package lease

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// Spec names a campaign and paces it.
type Spec struct {
	// Name is the lease every candidate campaigns for.
	Name string
	// Owner identifies this candidate. It must be unique per process: two
	// processes presenting one owner would both believe they hold the lease.
	Owner string
	// TTL is how long an unrenewed lease outlives its holder. The holder
	// renews every TTL/3 and steps down TTL/6 before the expiry the store
	// recorded for the last renewal it can prove, so it stops before any
	// successor can start. A holder that dies without releasing is replaced
	// at most TTL+Retry after its last renewal: the lease has to expire, then
	// a candidate has to try again.
	TTL time.Duration
	// Retry is how often a candidate that does not hold the lease tries to
	// take it. Zero or negative means TTL/3.
	Retry time.Duration
	// Info and Warn receive the term transitions and the store errors.
	// Nil discards them.
	Info, Warn func(format string, args ...any)
}

// releaseTimeout bounds the release a term ends with. It runs on a context
// detached from the one being cancelled (the release is what a shutdown owes
// its successor), so it needs its own bound.
const releaseTimeout = 5 * time.Second

// Run campaigns for spec.Name until ctx is done, and runs fn for as long as
// this candidate holds the lease. fn's context is cancelled once the lease can
// no longer be proven held — at the renewal that finds another owner holding
// it, or TTL/6 before the expiry of the last renewal that succeeded — and fn
// is expected to return promptly then. When fn returns, the lease is released so a successor takes
// over at its next attempt instead of waiting out the TTL, and the campaign
// resumes unless ctx is done.
//
// A store's answer can be lost after its write landed: an attempt that timed
// out, or one cut by the stop, may still have granted the lease. The campaign
// therefore releases after every unanswered attempt, and what is left — the
// write landing after that release — keeps the lease for one TTL, the same
// bound as a crash.
//
// It returns an error only for an unusable spec; store failures are reported
// through spec.Warn and retried, never turned into "elected".
func Run(ctx context.Context, st Store, spec Spec, fn func(context.Context)) error {
	if st == nil {
		return errors.New("lease: nil store")
	}
	if err := validate(spec.Name, spec.Owner, spec.TTL); err != nil {
		return err
	}
	spec.Name, spec.Owner = strings.TrimSpace(spec.Name), strings.TrimSpace(spec.Owner)
	retry := spec.Retry
	if retry <= 0 {
		retry = spec.TTL / 3
	}
	c := &campaign{st: st, spec: spec}
	for {
		if ctx.Err() != nil {
			return nil
		}
		sent, ok, answered := c.tryAcquire(ctx)
		if ok {
			c.serveTerm(ctx, sent, fn)
		} else if !answered {
			c.releaseUnanswered(ctx)
		}
		if ctx.Err() != nil {
			return nil
		}
		t := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

type campaign struct {
	st   Store
	spec Spec
	// failing is the edge of the campaign's store-error episode: the first
	// failure and the recovery are logged, the repeats in between are not.
	failing bool
}

// tryAcquire makes one attempt, bounded by a third of the TTL — never by
// Retry, which may be far shorter than a healthy store's round-trip and would
// then time out every attempt whose write still lands. It returns the instant
// the attempt was sent — the one it stamped, so the origin of the holder's own
// deadline (stepDownAt) — and whether the store's answer arrived.
func (c *campaign) tryAcquire(ctx context.Context) (sent time.Time, ok, answered bool) {
	sent = time.Now()
	callCtx, cancel := context.WithTimeout(ctx, c.spec.TTL/3)
	ok, err := c.st.Acquire(callCtx, c.spec.Name, c.spec.Owner, sent, c.spec.TTL)
	cancel()
	if err != nil {
		if ctx.Err() == nil && !c.failing {
			c.failing = true
			c.warn("lease %q: %s cannot reach the lease store — not running until it can: %v", c.spec.Name, c.spec.Owner, err)
		}
		return sent, false, false
	}
	if c.failing {
		c.failing = false
		c.info("lease %q: %s reaches the lease store again", c.spec.Name, c.spec.Owner)
	}
	return sent, ok, true
}

// serveTerm runs fn while the lease is held, renewing it every TTL/3, and
// ends the term with a release. sent is when the acquiring call left.
func (c *campaign) serveTerm(ctx context.Context, sent time.Time, fn func(context.Context)) {
	spec := c.spec
	c.info("lease %q: %s elected (TTL %s)", spec.Name, spec.Owner, spec.TTL)
	termCtx, stop := context.WithCancel(ctx)
	defer stop()

	hold := spec.TTL - spec.TTL/6
	// The step-down timer fires TTL/6 before the expiry the store recorded for
	// the last call this holder can prove (stepDownAt), so the holder is gone
	// before any successor can take the lease. The stop comes before the log
	// line, which may be slow.
	deadline := time.AfterFunc(time.Until(c.stepDownAt(sent)), func() {
		stop()
		c.warn("lease %q: %s stepping down — no renewal it can prove in %s", spec.Name, spec.Owner, hold)
	})
	defer deadline.Stop()

	var (
		wg   sync.WaitGroup
		lost bool // written by the renewer, read after wg.Wait
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		lost = c.renew(termCtx, stop, deadline, hold)
	}()

	// Deferred so a term that ends in a panic still hands the lease on.
	defer func() {
		stop()
		wg.Wait()
		c.release(ctx, lost)
	}()
	fn(termCtx)
}

// releaseUnanswered gives back what an attempt whose answer was lost may have
// taken, so a candidate that cannot hear the store does not hold the lease
// without serving until the TTL runs out. Quiet: an attempt that took nothing
// leaves nothing to release, and a failure here is the outage tryAcquire
// already reported, or the stop.
func (c *campaign) releaseUnanswered(ctx context.Context) {
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	_ = c.st.Release(relCtx, c.spec.Name, c.spec.Owner)
}

// release ends a term: it gives the lease up on a context detached from ctx,
// bounded by releaseTimeout. lost says the renewer already reported the loss.
func (c *campaign) release(ctx context.Context, lost bool) {
	spec := c.spec
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	switch err := c.st.Release(relCtx, spec.Name, spec.Owner); {
	case err == nil:
		c.info("lease %q: %s released", spec.Name, spec.Owner)
	case errors.Is(err, ErrLost):
		if !lost { // the renewer already said so otherwise
			c.warn("lease %q: %s had already lost the lease when its term ended", spec.Name, spec.Owner)
		}
	default:
		c.warn("lease %q: %s could not release — a successor waits out the TTL (%s): %v", spec.Name, spec.Owner, spec.TTL, err)
	}
}

// renew keeps the lease for the term and reports whether it ended because
// another owner took it. A renewal the store refuses ends the term at once; a
// renewal the store cannot answer leaves the step-down timer running, so the
// holder keeps working through a blip shorter than hold and stops before its
// lease can expire.
func (c *campaign) renew(termCtx context.Context, stop context.CancelFunc, deadline *time.Timer, hold time.Duration) bool {
	spec := c.spec
	every := spec.TTL / 3
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-termCtx.Done():
			return false
		case <-t.C:
		}
		sent := time.Now()
		callCtx, cancel := context.WithTimeout(termCtx, every)
		ok, err := c.st.Renew(callCtx, spec.Name, spec.Owner, sent, spec.TTL)
		cancel()
		switch {
		case termCtx.Err() != nil:
			return false
		case err != nil:
			c.warn("lease %q: %s could not renew (stepping down in at most %s without one): %v", spec.Name, spec.Owner, hold, err)
		case !ok:
			stop()
			c.warn("lease %q: %s lost the lease to another owner — stepping down", spec.Name, spec.Owner)
			return true
		default:
			deadline.Reset(time.Until(c.stepDownAt(sent)))
		}
	}
}

// stepDownAt is when a holder must stop if the call it sent at sent is the
// last one it can prove: TTL/6 before the expiry the store records for that
// call. The stores keep instants to the millisecond, so that expiry can come
// up to a millisecond before sent+TTL — which matters for short leases — and
// is computed here exactly as they compute it. The result keeps sent's
// monotonic reading, so the timer is immune to wall-clock steps.
func (c *campaign) stepDownAt(sent time.Time) time.Time {
	expires := stamp(stamp(sent).Add(c.spec.TTL))
	return sent.Add(expires.Sub(sent.UTC()) - c.spec.TTL/6)
}

func (c *campaign) info(format string, args ...any) {
	if c.spec.Info != nil {
		c.spec.Info(format, args...)
	}
}

func (c *campaign) warn(format string, args ...any) {
	if c.spec.Warn != nil {
		c.spec.Warn(format, args...)
	}
}
