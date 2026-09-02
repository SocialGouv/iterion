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

	lost     atomic.Bool
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.leaser.RenewClaim(ctx, s.issueID, s.tok)
			cancel()
			switch {
			case err == nil:
			case errors.Is(err, tracker.ErrClaimConflict):
				// The claim moved on — latch, tell the owner once, stop.
				s.lost.Store(true)
				s.warn("dispatcher: claim on %s was lost (lease superseded) — stopping the worker: %v", s.issueID, err)
				if s.onLost != nil {
					s.onLost(err)
				}
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

// Stop ends the heartbeat (idempotent) and waits for the loop to exit.
// Called after the finish worker's LAST write — never earlier, or the
// lease lapses exactly while the worker is still writing.
func (s *claimSession) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}
