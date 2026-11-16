package runstream

import (
	"context"
	"errors"
	"fmt"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync"
	"time"
)

var fileTerminalPollInterval = 5 * time.Second

// fileTerminalHook, set by tests, runs after a round reports a terminal
// status and before the final drain, so a test can land a write in that
// window. Mutex-guarded: a test sets or clears it while a producer between
// rounds may still read it.
var (
	fileTerminalHookMu sync.Mutex
	fileTerminalHook   func()
)

func setFileTerminalHook(f func()) {
	fileTerminalHookMu.Lock()
	defer fileTerminalHookMu.Unlock()
	fileTerminalHook = f
}

// fileTerminalSettleWindow bounds how long an event stream that observed a
// terminal status keeps re-draining, every fileTerminalSettleInterval, for the
// terminal event its writer persists after that status. A run whose terminal
// event never comes (its emit failed, or it predates them) ends when the
// window elapses. Vars so tests can widen or tighten them.
var (
	fileTerminalSettleWindow   = 500 * time.Millisecond
	fileTerminalSettleInterval = 25 * time.Millisecond
)

// FileStreamHints are optional wake inputs of one event subscription. They
// carry no payload: every event is still read from the subscription's retained
// selection and validated there.
//
// Drain requests a drain now. Terminal requests the end check now, without
// the settle wait: the confirmed status takes a final drain and the end
// check's status read — a run still terminal ends (a terminal event already
// read ends at once), a run resumed since the check keeps tailing. A
// non-positive TerminalPoll keeps the default terminal poll interval.
type FileStreamHints struct {
	Drain, Terminal <-chan struct{}
	TerminalPoll    time.Duration
}

// fileRun is what one selected-file producer runs: its drain, its optional
// end-of-events barrier (nil for logs), its hints and settle bounds, and its
// reporting.
type fileRun struct {
	id                  string
	logs                bool
	drain               func(context.Context) error
	ended               func() bool
	hints               FileStreamHints
	settle, settleEvery time.Duration
	warn, fail          func(error)
}

// FileSource borrows a store and owns only its subscriptions. Payload IO is
// selected by the store; the legacy root argument is never an IO authority.
type FileSource struct {
	store   store.RunStore
	logger  *iterlog.Logger
	mu      sync.Mutex
	closed  bool
	active  map[uint64]context.CancelCauseFunc
	next    uint64
	workers sync.WaitGroup
}

func NewFileSource(st store.RunStore, _ string, logger *iterlog.Logger) *FileSource {
	if logger == nil {
		logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	return &FileSource{store: st, logger: logger}
}

// Close cancels every open subscription with ErrSourceClosed, which each one
// publishes before it ends, and joins them.
func (f *FileSource) Close() error {
	f.mu.Lock()
	f.closed = true
	for _, cancel := range f.active {
		cancel(ErrSourceClosed)
	}
	f.mu.Unlock()
	f.workers.Wait()
	return nil
}
func (f *FileSource) register(ctx context.Context) (context.Context, context.CancelCauseFunc, <-chan struct{}, func(), error) {
	if ctx == nil {
		return nil, nil, nil, nil, store.ErrInvalidRunDataIdentity
	}
	if e := ctx.Err(); e != nil {
		return nil, nil, nil, nil, e
	}
	c, cancel := context.WithCancelCause(ctx)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		cancel(ErrSourceClosed)
		return nil, nil, nil, nil, ErrSourceClosed
	}
	if f.active == nil {
		f.active = make(map[uint64]context.CancelCauseFunc)
	}
	f.next++
	id := f.next
	f.active[id] = cancel
	f.workers.Add(1)
	f.mu.Unlock()
	done := make(chan struct{})
	var once sync.Once
	finish := func() {
		once.Do(func() {
			cancel(context.Canceled)
			f.mu.Lock()
			delete(f.active, id)
			f.mu.Unlock()
			close(done)
			f.workers.Done()
		})
	}
	return c, cancel, done, finish, nil
}

// failIfSourceClosed publishes ErrSourceClosed when the subscription ends
// because its source was closed, so the end is not a clean completion.
func failIfSourceClosed(ctx context.Context, fail func(error)) {
	if errors.Is(context.Cause(ctx), ErrSourceClosed) {
		fail(ErrSourceClosed)
	}
}

// openRetained opens the subscription's retained selection. A transient
// failure (a permission, descriptor exhaustion, an I/O error) is a warning
// and the open is retried with the stream's backoff; a permanent refusal fails
// the subscription; ok is false once the subscription is over.
func openRetained(ctx context.Context, cap store.FilesystemStreamStore, id string, opts store.FilesystemStreamOpenOptions, warn, fail func(error)) (selection store.FilesystemStreamSelection, observed, ok bool) {
	delay := 250 * time.Millisecond
	for {
		selection, observed, e := cap.OpenFilesystemStream(ctx, id, opts)
		if e == nil {
			return selection, observed, true
		}
		if IsPermanentError(e) {
			fail(e)
			return nil, false, false
		}
		if !retryAfter(ctx, fileRetryLabel, e, delay, warn) {
			return nil, false, false
		}
		delay = nextMongoDelay(delay)
	}
}
func (f *FileSource) SubscribeEvents(ctx context.Context, id string, from int64) (EventSubscription, error) {
	return f.SubscribeEventsCursor(ctx, id, EventCursor{Seq: from})
}
func (f *FileSource) SubscribeEventsCursor(ctx context.Context, id string, position EventCursor) (EventSubscription, error) {
	return f.subscribeEvents(ctx, id, position, FileStreamHints{})
}

// SubscribeEventsHinted is SubscribeEventsCursor with wake inputs from the
// subscription's owner. Hints only change when the retained selection is
// drained or checked for a terminal status, never what is delivered.
func (f *FileSource) SubscribeEventsHinted(ctx context.Context, id string, position EventCursor, hints FileStreamHints) (EventSubscription, error) {
	return f.subscribeEvents(ctx, id, position, hints)
}

func (f *FileSource) subscribeEvents(ctx context.Context, id string, position EventCursor, hints FileStreamHints) (EventSubscription, error) {
	if hints.TerminalPoll <= 0 {
		hints.TerminalPoll = fileTerminalPollInterval
	}
	settle, settleEvery := fileTerminalSettleWindow, fileTerminalSettleInterval
	c, cancel, done, finish, e := f.register(ctx)
	if e != nil {
		return nil, e
	}
	cap := store.AsFilesystemStreamStore(f.store)
	if cap == nil {
		finish()
		return nil, store.ErrRunDataCapability
	}
	pipe := NewEventPipeWithClose(func() { cancel(context.Canceled); <-done })
	go func() {
		defer finish()
		defer pipe.Finish(nil)
		defer failIfSourceClosed(c, pipe.Fail)
		selection, observed, ok := openRetained(c, cap, id, store.FilesystemStreamOpenOptions{WaitForInitialDirectory: true}, pipe.Warn, pipe.Fail)
		if !ok {
			return
		}
		if !observed || nilEventSelection(selection) {
			pipe.Fail(store.ErrRunDataCapability)
			return
		}
		defer selection.Close()
		cursor := newFileEventDrain(selection, pipe, position.Seq)
		if cursor.admissionErr != nil {
			pipe.Fail(cursor.admissionErr)
			return
		}
		cursor.after = position.After
		defer cursor.Close()
		f.runSelectedFile(c, selection, fileRun{id: id, drain: cursor.Drain, ended: cursor.Ended, hints: hints, settle: settle, settleEvery: settleEvery, warn: pipe.Warn, fail: pipe.Fail})
	}()
	return pipe, nil
}
func (f *FileSource) SubscribeLogs(ctx context.Context, id string, from int64) (LogSubscription, error) {
	c, cancel, done, finish, e := f.register(ctx)
	if e != nil {
		return nil, e
	}
	cap := store.AsFilesystemStreamStore(f.store)
	if cap == nil {
		finish()
		return nil, store.ErrRunDataCapability
	}
	pipe := NewLogPipeWithClose(func() { cancel(context.Canceled); <-done })
	go func() {
		defer finish()
		defer pipe.Finish(nil)
		defer failIfSourceClosed(c, pipe.Fail)
		selection, observed, ok := openRetained(c, cap, id, store.FilesystemStreamOpenOptions{}, pipe.Warn, pipe.Fail)
		if !ok || !observed {
			return
		}
		if selection == nil {
			pipe.Fail(store.ErrRunDataCapability)
			return
		}
		defer selection.Close()
		cursor := newFileLogDrain(selection, pipe, from)
		defer cursor.Close()
		// Absence is valid only after the retained observation was checked. A
		// joined identity/missing error must never be an empty successful log;
		// a transient failure of the check is retried like the open.
		delay := 250 * time.Millisecond
		for {
			file, e := selection.OpenLog(c)
			if IsPermanentError(e) {
				pipe.Fail(e)
				return
			}
			if isFileMissing(e) {
				return
			}
			if e != nil {
				if !retryAfter(c, fileRetryLabel, e, delay, pipe.Warn) {
					return
				}
				delay = nextMongoDelay(delay)
				continue
			}
			if file == nil {
				pipe.Fail(store.ErrRunDataCapability)
				return
			}
			file.Close()
			break
		}
		f.runSelectedFile(c, selection, fileRun{id: id, logs: true, drain: cursor.Drain, hints: FileStreamHints{TerminalPoll: fileTerminalPollInterval}, warn: pipe.Warn, fail: pipe.Fail})
	}()
	return pipe, nil
}

type fileRound struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	terminal bool
	// hinted is true when the terminal status was confirmed on a Terminal
	// hint, the producer's own end-of-events barrier.
	hinted bool
	err    error
}

func newFileRound(ctx context.Context, s store.FilesystemStreamSelection, hints FileStreamHints) *fileRound {
	if hints.TerminalPoll <= 0 {
		hints.TerminalPoll = fileTerminalPollInterval
	}
	c, cancel := context.WithCancel(ctx)
	r := &fileRound{ctx: c, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		check := func(hinted bool) bool {
			terminal, e := s.Terminal(c)
			if e != nil || terminal {
				r.terminal, r.hinted, r.err = terminal, hinted, e
				cancel()
				return true
			}
			return false
		}
		// A closed hint channel is a hint that never fires again.
		terminalHint := hints.Terminal
		for {
			select {
			case <-c.Done():
				return
			case _, open := <-terminalHint:
				if !open {
					terminalHint = nil
					continue
				}
				if check(true) {
					return
				}
			case <-timer.C:
				if check(false) {
					return
				}
				timer.Reset(hints.TerminalPoll)
			}
		}
	}()
	return r
}
func (r *fileRound) finish(parent context.Context, e error) (bool, bool, error) {
	r.cancel()
	<-r.done
	if mongoContextErrorOnly(e) && errors.Is(e, context.Canceled) && parent.Err() == nil {
		e = nil
	}
	pollErr := r.err
	if mongoContextErrorOnly(pollErr) && errors.Is(pollErr, context.Canceled) && parent.Err() == nil {
		pollErr = nil
	}
	return r.terminal, r.hinted, errors.Join(e, pollErr)
}
func (f *FileSource) runSelectedFile(ctx context.Context, s store.FilesystemStreamSelection, run fileRun) {
	eventDir, logDir := s.WatchHints()
	dir, name := eventDir, "events.jsonl"
	if run.logs {
		dir, name = logDir, "run.log"
	}
	delay := 250 * time.Millisecond
	for {
		round := newFileRound(ctx, s, run.hints)
		e := watchFileDrains(round.ctx, dir, name, run.drain, run.hints.Drain, f.logger)
		terminal, hinted, e := round.finish(ctx, e)
		if IsPermanentError(e) {
			run.fail(e)
			return
		}
		if ctx.Err() != nil || errors.Is(e, errEventDeliveryStopped) {
			return
		}
		if terminal && e == nil {
			fileTerminalHookMu.Lock()
			terminalHook := fileTerminalHook
			fileTerminalHookMu.Unlock()
			if terminalHook != nil {
				// Test seam: a write landed between the terminal read and the
				// final drain.
				terminalHook()
			}
			var ended bool
			ended, e = f.finalDrain(ctx, s, run, hinted)
			if IsPermanentError(e) {
				run.fail(e)
				return
			}
			if ended || errors.Is(e, errEventDeliveryStopped) {
				return
			}
			if e == nil {
				// The run is no longer terminal (resumed): tail it again.
				continue
			}
		}
		if e == nil {
			e = fmt.Errorf("runstream: filesystem watch ended")
		}
		if !retryAfter(ctx, fileRetryLabel, e, delay, run.warn) {
			return
		}
		delay = nextMongoDelay(delay)
	}
}

// finalDrain flushes what a terminal run persisted, then holds the stream at
// the end-of-events barrier: the terminal writers persist the status before
// the terminal event, so a terminal status alone does not end the events. The
// events end once the drained ones end with a terminal event. Otherwise a
// Terminal hint or the end of the settle window runs the end check (settleEnd):
// the status is read again through the retained selection, so a run resumed
// after a terminal read keeps its stream; a run still terminal ends it (a
// status-only terminal write is legitimate). A log stream (no episode) ends at
// once. ended is false, with a nil error, when the run is no longer terminal
// and its stream goes on.
func (f *FileSource) finalDrain(ctx context.Context, s store.FilesystemStreamSelection, run fileRun, hinted bool) (bool, error) {
	if e := run.drain(ctx); e != nil {
		return false, e
	}
	if run.ended == nil || run.ended() {
		return true, nil
	}
	if hinted {
		// The hint asked for the check now, without the settle wait: settleEnd
		// drains again and reads the status, so a run resumed after the
		// hint's status read keeps its stream.
		return f.settleEnd(ctx, s, run)
	}
	window := time.NewTimer(run.settle)
	defer window.Stop()
	tick := time.NewTicker(run.settleEvery)
	defer tick.Stop()
	// A closed hint channel is a hint that never fires again.
	terminalHint, drainHint := run.hints.Terminal, run.hints.Drain
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case _, open := <-terminalHint:
			if !open {
				terminalHint = nil
				continue
			}
			return f.settleEnd(ctx, s, run)
		case <-window.C:
			return f.settleEnd(ctx, s, run)
		case _, open := <-drainHint:
			if !open {
				drainHint = nil
				continue
			}
		case <-tick.C:
		}
		if e := run.drain(ctx); e != nil || run.ended() {
			return e == nil, e
		}
	}
}

// settleEnd closes the wait for a terminal event: a last drain, then the
// status read again through the retained selection. A run still terminal
// ends its events, with or without a terminal event (a status-only terminal
// write is legitimate); a run resumed meanwhile keeps its stream.
func (f *FileSource) settleEnd(ctx context.Context, s store.FilesystemStreamSelection, run fileRun) (bool, error) {
	if e := run.drain(ctx); e != nil {
		return false, e
	}
	if run.ended() {
		return true, nil
	}
	terminal, e := s.Terminal(ctx)
	if e != nil {
		return false, e
	}
	if terminal {
		f.logger.Debug("runstream: run %s ends terminal without a terminal event", run.id)
	}
	return terminal, nil
}
