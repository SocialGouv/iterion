package runview

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The bound log path (R54 slice 4): one merge per subscriber, reading the
// run's bytes from its retained filesystem selection — never from the
// producer's ring — and woken by the run's log lane. The lane carries no
// bytes, only wakes and the epoch's one end value, so a slow reader never
// loses a byte to a full channel: the bytes stay in the store and a wake says
// "read again".
//
// A bound stream ends by one of exactly three sources, and no other:
//
//   - E1 — the producer's end value, delivered on the lane the subscription
//     adopted (Terminal: one last drain then the end; Paused: park for the
//     resume's epoch; Lost: a typed error, never a settle).
//   - E2 — a retained closed epoch: an epoch that closed while the merge was
//     parked keeps its end, its durability and its cause until observed.
//   - E3 — no lane and no local producer: a terminal status, a settle window
//     and one last validated drain. No poll ever ends a live lane.
//
// A poll is never a fourth source: on a live lane it only warns once at
// logProducerStallWarn.

// Bound log bounds. Vars so a test can tighten them; production reads each
// once at subscribe time so a later change cannot retune a running stream.
var (
	// logTerminalSettle is the E3 settle: a terminal status with no lane waits
	// this long for a producer to register, draining the whole window.
	logTerminalSettle = 500 * time.Millisecond
	// logTerminalSettleEvery is the drain cadence inside the settle window.
	logTerminalSettleEvery = 25 * time.Millisecond
	// logProducerStallWarn is the stall threshold of a LIVE lane: a Warn, once,
	// never a fin.
	logProducerStallWarn = 10 * time.Second
)

// The end sources recorded on a liveLogEntry (the instrumented assertion of
// §3.1).
const (
	logEndByLane     = "lane"
	logEndByRetained = "retained"
	logEndBySettle   = "settle"
	logEndByStop     = "stop"
)

// liveLogEntry is one registered bound log merge: the run it streams, its
// cancellation, the channel closed once its cleanup ran, and the source of
// the end it applied.
type liveLogEntry struct {
	runID  string
	cancel context.CancelCauseFunc
	done   chan struct{}
	endBy  string
}

// logEndRecord is what one run's last merged log stream ended with, and how
// many fin applications that took: a first-end-wins stream applies exactly one.
type logEndRecord struct {
	by      string
	applies int
}

// logEndBy returns the end source and application count of runID's last bound
// log stream (test assertion).
func (l *liveEventStreams) logEndBy(runID string) (string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.logEnds[runID]
	return r.by, r.applies
}

// logStreams returns the number of registered bound log merges (test
// assertion).
func (l *liveEventStreams) logStreams() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.logActive)
}

// subscribeLogs registers one bound log merge delivering into out. out is the
// caller's pipe (serveBoundLogs): a return before the merge goroutine starts
// leaves out untouched for the caller to fail and close; once the merge runs
// it closes out itself.
func (l *liveEventStreams) subscribeLogs(ctx context.Context, runID string, from int64, out runstream.LogPipe) error {
	s := l.svc
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, bound := store.RunDataBindingFrom(ctx); !bound {
		return fmt.Errorf("runview: bound log subscription of %s: %w", runID, store.ErrRunDataBindingRequired)
	}
	// The bytes of a bound subscription are the store's: a store that cannot
	// serve them has no lane path at all.
	if store.AsRunLogStore(s.store) == nil {
		return fmt.Errorf("runview: bound log subscription of %s: the store persists no run.log: %w", runID, store.ErrRunDataCapability)
	}
	if s.logRegistry == nil {
		return fmt.Errorf("runview: bound log subscription of %s: no log lane registry: %w", runID, store.ErrRunDataCapability)
	}

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return runstream.ErrSourceClosed
	}
	if l.logActive == nil {
		l.logActive = make(map[uint64]*liveLogEntry)
	}
	if l.logEnds == nil {
		l.logEnds = make(map[string]logEndRecord)
	}
	c, cancel := context.WithCancelCause(ctx)
	l.next++
	id := l.next
	entry := &liveLogEntry{runID: runID, cancel: cancel, done: make(chan struct{})}
	l.logActive[id] = entry
	l.mu.Unlock()

	// The watch precedes the initial observation (F2): the watch never replays
	// the current state, so an epoch registered and closed between the two is
	// read by the observation, and one that lands after it still wakes the
	// merge. The token pending at the merge's first iteration is a
	// reconciliation of that window.
	watch, cancelWatch := s.logRegistry.watch(runID)
	if hook := l.beforeLogObserved; hook != nil {
		hook()
	}

	m := &liveLogMerge{
		l: l, id: id, runID: runID, ctx: c, entry: entry,
		watch: watch, cancelWatch: cancelWatch,
		dataWake: make(chan struct{}, 1), finalHint: make(chan struct{}, 1),
		out:          out,
		pollInterval: svcEventTerminalPollInterval,
		settle:       logTerminalSettle, settleEvery: logTerminalSettleEvery, stallWarn: logProducerStallWarn,
	}

	lane, _, err := s.observedLogLane(c, runID, true)
	if err != nil {
		cancelWatch()
		close(entry.done)
		l.forgetLog(id)
		return err
	}
	if lane != nil {
		m.adoptLane(lane)
	} else if ferr := s.localLogProducerFailure(runID); ferr != nil {
		// No epoch and a retained local capture failure: the run's log stream
		// is dead (D2) — refused typed, never parked on it.
		cancelWatch()
		close(entry.done)
		l.forgetLog(id)
		return fmt.Errorf("runview: bound log subscription of %s: %w", runID, ferr)
	}
	// The initial observation AND its retained-failure check have run: a test
	// parks here to land a change the merge can only learn from its watch.
	if hook := l.afterLogObserved; hook != nil {
		hook()
	}

	inner, err := l.files.SubscribeLogsHinted(c, runID, from, runstream.FileStreamHints{Drain: m.dataWake, Final: m.finalHint})
	if err != nil {
		cancelWatch()
		if m.lane != nil {
			m.lane.Cancel()
		}
		close(entry.done)
		l.forgetLog(id)
		return err
	}
	m.inner = inner
	go func() {
		defer close(entry.done)
		defer out.Finish(m.cleanup)
		m.run()
	}()
	return nil
}

// forgetLog drops a registration that never reached its merge.
func (l *liveEventStreams) forgetLog(id uint64) {
	l.mu.Lock()
	delete(l.logActive, id)
	l.mu.Unlock()
}

// liveLogMerge is one client's bound log stream: the only producer of out.
type liveLogMerge struct {
	l     *liveEventStreams
	id    uint64
	runID string
	ctx   context.Context
	entry *liveLogEntry
	inner runstream.LogSubscription

	lane         *LogWakeSubscription
	laneEpoch    uint64
	laneRetained bool
	// endedEpoch is the epoch whose end was already applied: the first fin of
	// a generation is sticky (E1 then E2, or E2 then E1, apply once).
	endedEpoch uint64

	watch       <-chan struct{}
	cancelWatch func()
	dataWake    chan struct{}
	finalHint   chan struct{}
	out         runstream.LogPipe

	// Bounds, read once at subscribe time.
	pollInterval                   time.Duration
	settle, settleEvery, stallWarn time.Duration

	// applies counts the fin applications of this merge: a first-end-wins
	// stream applies exactly one (test assertion).
	applies int

	settling    bool
	settleAt    time.Time
	settleTick  *time.Ticker
	settleC     <-chan time.Time
	laneSince   time.Time
	stallWarned bool
}

// laneC is the lane's channel, nil when the merge holds none: a nil channel
// blocks forever in the select, which is exactly the "parked" state.
func (m *liveLogMerge) laneC() <-chan LogWake {
	if m.lane == nil {
		return nil
	}
	return m.lane.C
}

// adoptLane takes a subscription as the merge's live lane.
func (m *liveLogMerge) adoptLane(sub *LogWakeSubscription) {
	if m.lane != nil && m.lane != sub {
		m.lane.Cancel()
	}
	m.stopSettle() // a lane beats E3
	m.lane = sub
	m.laneEpoch = sub.epoch
	m.laneRetained = sub.Closed
	m.laneSince = time.Now()
	m.stallWarned = false
	// A lane adopted late may have bytes the subscription has not read: its
	// producer's wakes went to holders only, and none of them was this merge.
	// A wake carries no truth — it only says "read again" — so one is due.
	signal(m.dataWake)
}

// parkLane drops the lane a Paused epoch ended: the resume's epoch is a new
// generation, announced by the watch.
func (m *liveLogMerge) parkLane() {
	if m.lane != nil {
		m.lane.Cancel()
	}
	m.lane = nil
	m.laneEpoch = 0
	m.laneRetained = false
	m.stallWarned = false
}

// applyEnd applies one epoch's end value, unless that epoch's end was already
// applied (first end wins). It reports whether the merge must stop now.
func (m *liveLogMerge) applyEnd(epoch uint64, end logEnd, by string) (stop bool) {
	if end == logEndNone {
		return false
	}
	if epoch != 0 && m.endedEpoch == epoch {
		return false
	}
	if epoch != 0 {
		m.endedEpoch = epoch
	}
	m.stopSettle()
	switch end {
	case logEndPaused:
		// The producer parked on a human wait: the lane stays readable for the
		// resume's epoch, which the watch announces.
		m.parkLane()
		return false
	case logEndTerminal:
		m.recordEnd(by)
		signal(m.finalHint)
		// The inner takes one last drain through its cursor and closes; the
		// merge ends then, so bytes written before the end are still shipped.
		return false
	case logEndLost:
		m.recordEnd(by)
		cause := m.l.svc.localLogProducerFailure(m.runID)
		if cause == nil {
			cause = fmt.Errorf("runview: log lane of %s ended without a producer verdict: %w", m.runID, runstream.ErrSourceClosed)
		}
		m.out.Fail(cause)
		return true
	}
	return false
}

// recordEnd notes the source of the stream's end, first application only.
func (m *liveLogMerge) recordEnd(by string) {
	if m.entry.endBy == "" {
		m.entry.endBy = by
	}
	m.applies++
}

// startSettle arms the E3 window: a terminal status with no lane drains for
// logTerminalSettle before the end.
func (m *liveLogMerge) startSettle() {
	if m.settling {
		return
	}
	m.settling = true
	m.settleAt = time.Now().Add(m.settle)
	if m.settleTick == nil {
		m.settleTick = time.NewTicker(m.settleEvery)
	}
	m.settleC = m.settleTick.C
	signal(m.dataWake)
}

// stopSettle disarms the E3 window: a lane appeared, or the end was applied.
func (m *liveLogMerge) stopSettle() {
	if !m.settling {
		return
	}
	m.settling = false
	if m.settleTick != nil {
		m.settleTick.Stop()
		m.settleTick = nil
	}
	m.settleC = nil
}

// run merges the retained selection and the run's log lane until one of the
// three end sources ends it.
func (m *liveLogMerge) run() {
	chunks, errs := m.inner.Chunks(), m.inner.Errors()
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	defer m.stopSettle()
	// A terminal run with no lane must not wait a whole poll interval to end:
	// the first status read is immediate (E3's settle still applies, and a
	// lane adopted meanwhile cancels it).
	if m.lane == nil && m.statusPoll() {
		return
	}
	var fatal error
	for {
		select {
		case <-m.ctx.Done():
			// A Service stop is a typed end for the consumer, whichever of the
			// ready cases the select happens to pick.
			m.failStopped()
			return
		case <-m.out.Done():
			return
		case chunk, ok := <-chunks:
			if !ok {
				m.finish(errs, fatal)
				return
			}
			if len(chunk.Data) > 0 && !m.out.Ship(m.ctx, chunk) {
				return
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if runstream.IsPermanentError(err) {
				// Retained: the selection refused (a successor, a deletion, a
				// broken file) — the end says so, not a clean completion.
				if fatal == nil {
					fatal = err
				}
				continue
			}
			if m.ctx.Err() == nil {
				m.out.Warn(err)
			}
		case w, ok := <-m.laneC():
			if !ok {
				// Closed without an end: the subscription was cancelled by the
				// cleanup, never a verdict.
				m.parkLane()
				continue
			}
			if w.End == logEndNone {
				signal(m.dataWake)
				continue
			}
			by := logEndByLane
			if m.laneRetained {
				by = logEndByRetained
			}
			if m.applyEnd(m.laneEpoch, w.End, by) {
				return
			}
		case <-m.watch:
			if m.rearm() {
				return
			}
		case <-ticker.C:
			if m.statusPoll() {
				return
			}
		case <-m.settleC:
			signal(m.dataWake)
			if time.Now().Before(m.settleAt) {
				continue
			}
			m.stopSettle()
			if m.applyEnd(0, logEndTerminal, logEndBySettle) {
				return
			}
		}
	}
}

// rearm reacts to a registration change: a local capture failure first (it is
// a fin, never a wait), then the current epoch — a retained closed one applies
// its end, an open one replaces the lane (admission at EVERY adoption).
// It reports whether the merge must stop now.
func (m *liveLogMerge) rearm() bool {
	if m.ctx.Err() != nil {
		return true
	}
	if err := m.l.svc.localLogProducerFailure(m.runID); err != nil {
		m.recordEnd(logEndByLane)
		m.out.Fail(err)
		return true
	}
	sub, _, err := m.l.svc.observedLogLane(m.ctx, m.runID, false)
	if err != nil {
		m.out.Fail(err)
		return true
	}
	if sub == nil {
		// No epoch yet: stay parked, the watch will announce one.
		return false
	}
	if m.lane != nil && m.laneEpoch == sub.epoch {
		// The merge already holds this epoch: its own lane carries the end
		// (E1), so the retained value must not be applied here — that WOULD be
		// the same generation's second application (M-STICKY). A subscription
		// onto the open epoch is just another holder joining.
		sub.Cancel()
		return false
	}
	if sub.Closed {
		var end logEnd
		select {
		case w := <-sub.C:
			end = w.End
		default:
			end = logEndLost
		}
		sub.Cancel()
		return m.applyEnd(sub.epoch, end, logEndByRetained)
	}
	m.adoptLane(sub)
	return false
}

// statusPoll is the one status read of the bound stream: it never ends a live
// lane (a finalize writes after the status), and only when no lane and no
// local producer exist does a terminal status arm the E3 settle. It reports
// whether the merge must stop now.
func (m *liveLogMerge) statusPoll() bool {
	if hook := m.l.beforeLogStatusPoll; hook != nil {
		hook()
	}
	if m.lane != nil {
		if !m.stallWarned && !m.laneSince.IsZero() && time.Since(m.laneSince) >= m.stallWarn {
			m.stallWarned = true
			if m.ctx.Err() == nil {
				m.out.Warn(fmt.Errorf("runview: the log producer of %s has not ended %.0fs after its lane was adopted", m.runID, m.stallWarn.Seconds()))
			}
		}
		return false
	}
	if m.settling {
		// The settle ticker drives the E3 end.
		return false
	}
	if m.l.svc.localLogProducerFailure(m.runID) != nil {
		// The watch delivers it; never settle over a known failure.
		return false
	}
	run, err := m.l.svc.store.LoadRun(m.ctx, m.runID)
	if err != nil {
		if runstream.IsPermanentError(err) {
			m.out.Fail(err)
			return true
		}
		if m.ctx.Err() == nil {
			m.out.Warn(fmt.Errorf("runview: terminal check of %s: %w", m.runID, err))
		}
		return false
	}
	if !run.Status.IsTerminal() {
		return false
	}
	m.startSettle()
	return false
}

// finish ends the stream once the inner closed. A Service stop wins over any
// other outcome; a retained permanent refusal is published; otherwise the end
// the merge applied stands (an end value is the only clean completion).
func (m *liveLogMerge) finish(errs <-chan error, fatal error) {
	if errs != nil {
		for err := range errs {
			if runstream.IsPermanentError(err) && fatal == nil {
				fatal = err
			}
		}
	}
	if cause := closedCause(m.ctx); cause != nil {
		m.failStopped()
		return
	}
	if fatal != nil {
		m.out.Fail(fatal)
	}
}

// failStopped publishes a Service stop as the stream's typed end. An end value
// already applied keeps its voie (the first fin wins).
func (m *liveLogMerge) failStopped() {
	cause := closedCause(m.ctx)
	if cause == nil {
		return
	}
	if m.entry.endBy == "" {
		m.entry.endBy = logEndByStop
	}
	m.out.Fail(cause)
}

// cleanup releases what the merge owns: the inner joined first, then the lane
// and the watch, then its registration — which also records the end it
// applied for the witnesses.
func (m *liveLogMerge) cleanup() {
	m.entry.cancel(context.Canceled)
	m.stopSettle()
	if m.inner != nil {
		_ = m.inner.Close()
	}
	m.cancelWatch()
	if m.lane != nil {
		m.lane.Cancel()
	}
	m.l.mu.Lock()
	if m.entry.endBy != "" || m.applies > 0 {
		m.l.logEnds[m.runID] = logEndRecord{by: m.entry.endBy, applies: m.applies}
	}
	delete(m.l.logActive, m.id)
	m.l.mu.Unlock()
}
