package server

import (
	"context"
	"fmt"
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
//
// It is a WINDOW, not a guarantee, and it is much shorter than what the sites
// it joins allow themselves — 5s for an audit insert or a last_used_at stamp,
// 15s for an invitation email, 30s for each half of the forge settle and for
// the model-spec refresh's aggregator fetch, 60s for the OAuth connect kick,
// and no deadline at all for the config-share touch: a write that completes
// inside it is joined, and one that does not is still cut by the exit and
// named in the warning below. Widening the window is the chart arithmetic in
// docs/probes-and-graceful-shutdown.md, not a change here.
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

// registerBackgroundWorker reserves the shutdown's wait for ONE goroutine: it
// returns the channel that goroutine must close when it returns, and false
// when the shutdown has already joined — past that point nobody is left to
// wait, and a goroutine registered there would hold a channel the join will
// never look at again. The caller spawns nothing when it is refused.
//
// Compacting here bounds the registry by the number of LIVE goroutines rather
// than by how many have ever been started: boot-time loops stay in the slice
// for the life of the process, but the per-request entries — the forge→board
// projection (#1345), every detached best-effort write (goSafe) — would
// otherwise grow it without bound.
func (s *Server) registerBackgroundWorker(name string) (chan struct{}, bool) {
	done := make(chan struct{})
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgJoined {
		return nil, false
	}
	live := s.bgWorkers[:0]
	for _, w := range s.bgWorkers {
		select {
		case <-w.done:
		default:
			live = append(live, w)
		}
	}
	s.bgWorkers = append(live, backgroundWorker{name: name, done: done})
	return done, true
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

	// The shutdown SIGNAL has already fired (the HTTP drain is running,
	// bgJoined has not flipped yet): a loop started now would run on an
	// already-cancelled ctx and every store call in it would fail with a
	// generic "context canceled". Refuse before registering so the caller
	// — a late webhook's forge→board projection is the first one — can
	// record the delivery as deferred to its sweep instead (#1477
	// follow-up question 2). The select is non-blocking: a shutdown that
	// has not fired costs one channel probe.
	select {
	case <-s.shutdown:
		cancel()
		if s.logger != nil {
			s.logger.Warn("server: %s not started — the shutdown signal has already fired; anything this loop would have written is deferred to its reconciliation sweep", name)
		}
		return cancel, false
	default:
	}

	done, registered := s.registerBackgroundWorker(name)
	if !registered {
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

// joinBackgroundWorkers waits for every registered goroutine — plus any extra
// whose stop handle lives elsewhere — to return, all inside ONE budget, so a
// single slow one does not eat the window the others need and the exit costs
// the same whether the server started two loops or twenty.
//
// Loops still running when the budget is spent are named in a warning rather
// than waited out: past this point the process is exiting, and the only thing
// an operator can act on is WHICH loop overran — so the warning names each one
// ONCE, with how many of its goroutines were still running (summariseOverrun).
// It reports the wait that actually happened, not the budget: a caller whose
// own deadline is already spent gets no window at all, and saying otherwise
// would send an operator hunting a slow loop that was one channel-send from
// returning.
//
// The registry is re-read until it is empty, and bgJoined flips only then: a
// goroutine this join is WAITING for may ask for one more write on its way out
// — a sweep's last audit row, the shape retry_sweeper's abandon path takes —
// and a flag flipped before the wait would refuse exactly the write the join
// exists to cover. The one shared budget still bounds the total, so a chain of
// such writes cannot stretch the exit.
//
// This join is the LAST wait of the shutdown sequence — after the HTTP drain
// and after the board dispatcher's — so when it closes the registry there is
// genuinely nobody left to wait, which is what the refusal says.
func (s *Server) joinBackgroundWorkers(ctx context.Context, extra ...backgroundWorker) {
	started := time.Now()
	joinCtx, cancel := context.WithTimeout(ctx, s.backgroundJoinBudget())
	defer cancel()
	// Waited one after another against ONE deadline, which costs exactly what
	// waiting on all of them at once would: the loops are already running, so
	// this ends when the last of them returns or when the shared deadline
	// lands, whichever comes first. A goroutine per loop would buy nothing and
	// would add a fan-out to the one path that runs while the process exits.
	var slow []string
	pending := append([]backgroundWorker(nil), extra...)
	for {
		s.bgMu.Lock()
		workers := append(pending, s.bgWorkers...)
		s.bgWorkers = nil
		// Nothing left to wait for, or no budget left to wait with: this pass
		// is the last one, so the registry closes here.
		last := len(workers) == 0 || joinCtx.Err() != nil
		if last {
			s.bgJoined = true
		}
		s.bgMu.Unlock()
		pending = nil

		for _, w := range workers {
			if w.done == nil {
				continue
			}
			// A loop that has already returned is reported as such even when
			// the budget is spent: the two-case select below would decide that
			// by coin flip, and name a finished loop as an overrun.
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
		if last {
			break
		}
	}
	if len(slow) > 0 && s.logger != nil {
		s.logger.Warn("shutdown: waited %s for the background goroutines (budget %s) and %d are still running (%s) — their last write falls outside the grace period",
			time.Since(started).Round(time.Millisecond), s.backgroundJoinBudget(), len(slow), summariseOverrun(slow))
	}
}

// maxOverrunNames caps how many distinct tasks the overrun warning names. The
// tail is counted instead: the names come from the source, so a shutdown that
// overruns on more than this many distinct ones has a report to read in the
// count, not in the list.
const maxOverrunNames = 10

// summariseOverrun renders the overrun list as one entry per NAME, noisiest
// first, with how many of that task's goroutines were still running.
//
// Every per-request detached write goes through the registry, so a burst
// answered during the drain puts hundreds of entries under the same goSafe
// label: repeating that label once per entry made a single multi-kilobyte
// Warn line — the size a log collector truncates or drops, at the one moment
// it is the only evidence of the overrun left.
func summariseOverrun(slow []string) string {
	counts := make(map[string]int, len(slow))
	names := make([]string, 0, len(slow))
	for _, name := range slow {
		if counts[name] == 0 {
			names = append(names, name)
		}
		counts[name]++
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	var b strings.Builder
	for i, name := range names {
		if i == maxOverrunNames {
			fmt.Fprintf(&b, ", +%d more", len(names)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(name)
		if counts[name] > 1 {
			fmt.Fprintf(&b, " x%d", counts[name])
		}
	}
	return b.String()
}
