package server

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/errtrack"
)

// backgroundWorker is a loop the shutdown joins after the HTTP drain: the name
// errtrack knows it by, and the channel the loop's goroutine closes when it
// returns.
type backgroundWorker struct {
	name string
	done <-chan struct{}
}

// defaultBackgroundJoinBudget bounds the join of EVERY background loop
// together, not one budget per loop. The grace period is a single resource —
// the chart's terminationGracePeriodSeconds covers ShutdownDelay + teardown and
// nothing else — so a budget spent per worker would carve that window into as
// many slices as the server happens to start loops, which no arithmetic
// upstream provisions.
const defaultBackgroundJoinBudget = 500 * time.Millisecond

// backgroundJoinBudget is the window the shutdown gives every loop to return.
// Overridable per server (bgJoinBudget) so a test can widen it well past its
// own observation window instead of racing it on a starved machine.
func (s *Server) backgroundJoinBudget() time.Duration {
	if s.bgJoinBudget > 0 {
		return s.bgJoinBudget
	}
	return defaultBackgroundJoinBudget
}

// goUntilShutdown runs fn on a context cancelled when the server begins its
// shutdown, and registers the channel fn's return closes so the shutdown can
// join it. The returned cancel stops fn earlier than that signal; a caller with
// no other stop condition can ignore it.
//
// The join is what makes the grace period mean something. A loop stopped by a
// cancel alone returns after its cancel did, so the process can exit between a
// claim and its release, or mid-write into a store already being torn down —
// invisible in a pod, and a TempDir cleanup failure in a test.
//
// Once the shutdown has joined, fn is NOT started: boot and shutdown overlap
// (the signal arm calls Shutdown while ListenAndServe is still wiring loops, a
// window this package has already paid for once), and a loop started past the
// join is precisely the unwatched loop this function exists to prevent.
func (s *Server) goUntilShutdown(name string, fn func(context.Context)) context.CancelFunc {
	cancel, _ := s.tryGoUntilShutdown(name, fn)
	return cancel
}

// tryGoUntilShutdown is goUntilShutdown with a second return value that says
// whether the loop was actually started. Boot-time callers can ignore it (they
// register before the shutdown ever runs); a per-request site that acquired a
// shared resource — the forge→board projection semaphore is the first one —
// needs it so it can release the slot when registration is refused, otherwise
// the resource leaks for the remaining lifetime of the process. Same contract
// as goUntilShutdown otherwise.
func (s *Server) tryGoUntilShutdown(name string, fn func(context.Context)) (context.CancelFunc, bool) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.stateMu.Lock()
	joined := s.bgJoined
	if !joined {
		// Compacting here bounds the registry by the number of LIVE loops
		// rather than by how many have ever been started: boot-time loops
		// stay in the slice for the life of the process, but per-request
		// loops (scheduleForgeBoardProjection, #1345) would otherwise grow
		// it without bound.
		live := s.bgWorkers[:0]
		for _, w := range s.bgWorkers {
			select {
			case <-w.done:
			default:
				live = append(live, w)
			}
		}
		s.bgWorkers = append(live, backgroundWorker{name: name, done: done})
	}
	s.stateMu.Unlock()

	if joined {
		cancel()
		if s.logger != nil {
			s.logger.Warn("server: %s not started — the shutdown already joined its background loops, and one started now would run with nobody waiting for its last write", name)
		}
		return cancel, false
	}

	// Released by fn's return as well as by the shutdown signal: a watcher
	// parked on a shutdown that never comes outlives the loop it watches, and a
	// server that stops a single loop early would leak one goroutine per stop
	// for the lifetime of the process.
	go func() {
		select {
		case <-s.shutdown:
			cancel()
		case <-done:
		}
	}()
	errtrack.Go(name, func() {
		defer close(done)
		defer cancel()
		fn(ctx)
	})
	return cancel, true
}

// joinBackgroundWorkers waits for every registered loop — plus any extra whose
// stop handle lives elsewhere — to return, all inside ONE budget, so a single
// slow loop does not eat the window the others need and the exit costs the same
// whether the server started two loops or twenty.
//
// Loops still running when the budget is spent are named in a warning rather
// than waited out: past this point the process is exiting, and the only thing
// an operator can act on is WHICH loop overran. The warning reports the wait
// that actually happened, not the budget: a caller whose own deadline is
// already spent gets no window at all, and saying otherwise would send an
// operator hunting a slow loop that was one channel-send from returning.
func (s *Server) joinBackgroundWorkers(ctx context.Context, extra ...backgroundWorker) {
	s.stateMu.Lock()
	workers := make([]backgroundWorker, 0, len(s.bgWorkers)+len(extra))
	workers = append(workers, s.bgWorkers...)
	s.bgWorkers = nil
	s.bgJoined = true
	s.stateMu.Unlock()
	workers = append(workers, extra...)

	started := time.Now()
	joinCtx, cancel := context.WithTimeout(ctx, s.backgroundJoinBudget())
	defer cancel()
	// Waited one after another against ONE deadline, which costs exactly what
	// waiting on all of them at once would: the loops are already running, so
	// this ends when the last of them returns or when the shared deadline
	// lands, whichever comes first. A goroutine per loop would buy nothing and
	// would add a fan-out to the one path that runs while the process exits.
	var slow []string
	for _, w := range workers {
		if w.done == nil {
			continue
		}
		// A loop that has already returned is reported as such even when the
		// budget is spent: the two-case select below would decide that by coin
		// flip, and name a finished loop as an overrun.
		select {
		case <-w.done:
			continue
		default:
		}
		select {
		case <-w.done:
		case <-joinCtx.Done():
			slow = append(slow, w.name)
		}
	}
	if len(slow) > 0 && s.logger != nil {
		sort.Strings(slow)
		s.logger.Warn("shutdown: waited %s for the background loops (budget %s) and %d are still running (%s) — their last write falls outside the grace period",
			time.Since(started).Round(time.Millisecond), s.backgroundJoinBudget(), len(slow), strings.Join(slow, ", "))
	}
}
