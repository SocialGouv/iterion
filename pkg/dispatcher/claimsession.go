package dispatcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// claimSession heartbeats one claimed card's lease for as long as the
// dispatcher owns it — from the confirmed claim through the run and the
// asynchronous finish worker's last write. It is deliberately NOT tied
// to runningEntry's lifetime: the entry dies on the actor at finishRun,
// while the finish worker keeps writing (state move, give-up stamp,
// release) for a while after — the exact window the in-memory heartbeat
// used to go dark in (the plan review's F11).
//
// The ticker is unconditional: the alternative — piggybacking on run
// events — goes silent during a long model call, and a silent live
// worker whose lease lapses is a worker the reaper steals from.
//
// On the first ErrClaimConflict from a renewal the session latches
// lost, calls onLost exactly once, and stops: the claim moved on and
// the ONLY correct behaviour left is to stop working (the fenced write
// family refuses everything anyway; onLost lets the dispatcher cancel
// the run instead of burning tokens to the refusal).
type claimSession struct {
	issueID string
	tok     tracker.ClaimToken
	leaser  tracker.ClaimLeaser
	warn    func(format string, args ...any)
	onLost  func(err error)
	// interval is the heartbeat cadence — a third of the lease, so two
	// consecutive missed beats still leave slack before expiry.
	// Injectable for tests.
	interval time.Duration

	lost atomic.Bool
	// releasing latches once the owner has begun its OWN final release.
	// The release runs before Stop() (stopping earlier would let the
	// lease lapse while the worker is still writing), so a beat already
	// in flight can come back ErrClaimConflict simply because our own
	// release landed first. That is not a supersession: reporting it
	// warned "claim lost — stopping the worker" on every ordinary finish
	// that happened to straddle a beat, and fired the cancel path for a
	// run that had already ended.
	releasing atomic.Bool
	stop      chan struct{}
	stopOnce  sync.Once
	done      chan struct{}
}

// StartClaimSession begins heartbeating. leaser must be non-nil; the
// caller decides what "no lease backend" means (it logs once and runs
// legacy, per the ClaimLeaser contract).
func StartClaimSession(leaser tracker.ClaimLeaser, issueID string, tok tracker.ClaimToken,
	warn func(string, ...any), onLost func(error)) *claimSession {
	s := &claimSession{
		issueID:  issueID,
		tok:      tok,
		leaser:   leaser,
		warn:     warn,
		onLost:   onLost,
		interval: native.ClaimLeaseDuration / 3,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *claimSession) loop() {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			// The renewal's context dies with the session, not just on its
			// own deadline: Stop() runs ON THE ACTOR and waits for this
			// loop, so a renewal against a slow store would otherwise hold
			// the whole dispatcher hostage for its full timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			stopped := make(chan struct{})
			go func() {
				select {
				case <-s.stop:
					cancel()
				case <-stopped:
				}
			}()
			err := s.leaser.RenewClaim(ctx, s.issueID, s.tok)
			close(stopped)
			cancel()
			switch {
			case err == nil:
			case errors.Is(err, tracker.ErrClaimConflict):
				if s.releasing.Load() {
					// Our OWN release landed between this beat's send and
					// its reply. Nothing was superseded and there is nothing
					// left to cancel — the run is over.
					return
				}
				// The claim moved on — latch, tell the owner once, stop.
				s.lost.Store(true)
				s.warn("dispatcher: claim on %s was lost (lease superseded) — stopping the worker: %v", s.issueID, err)
				if s.onLost != nil {
					s.onLost(err)
				}
				return
			case errors.Is(err, context.Canceled):
				// Stop() cancelled the in-flight renewal (the cancel now
				// reaches the store): an ordinary teardown, not a failure —
				// warning here fired on every Stop that crossed a beat,
				// i.e. in a storm exactly when the store was slow.
				return
			default:
				// A transient store error must not kill a live claim: the
				// lease is long against the cadence, so the next beat
				// retries. Say so — a renew that fails every beat is the
				// reaper counting down.
				s.warn("dispatcher: lease renewal for %s failed (retrying next beat): %v", s.issueID, err)
			}
		}
	}
}

// Token returns the ownership token for fenced writes.
func (s *claimSession) Token() tracker.ClaimToken { return s.tok }

// Lost reports whether the claim was observed superseded.
func (s *claimSession) Lost() bool { return s.lost.Load() }

// Releasing announces that the owner is about to drop this claim itself.
// Call it immediately before the release write: from here on a renewal
// conflict is our own release, not a supersession, and must not warn or
// fire onLost. Safe on a nil session so callers stay branch-free.
func (s *claimSession) Releasing() {
	if s != nil {
		s.releasing.Store(true)
	}
}

// Stop ends the heartbeat (idempotent) and waits for the loop to exit.
// Called after the finish worker's LAST write — never earlier, or the
// lease lapses exactly while the worker is still writing.
func (s *claimSession) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}
