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

// FileSource borrows a store and owns only its subscriptions. Payload IO is
// selected by the store; the legacy root argument is never an IO authority.
type FileSource struct {
	store   store.RunStore
	logger  *iterlog.Logger
	mu      sync.Mutex
	closed  bool
	active  map[uint64]context.CancelFunc
	next    uint64
	workers sync.WaitGroup
}

func NewFileSource(st store.RunStore, _ string, logger *iterlog.Logger) *FileSource {
	if logger == nil {
		logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	return &FileSource{store: st, logger: logger}
}
func (f *FileSource) Close() error {
	f.mu.Lock()
	f.closed = true
	for _, cancel := range f.active {
		cancel()
	}
	f.mu.Unlock()
	f.workers.Wait()
	return nil
}
func (f *FileSource) register(ctx context.Context) (context.Context, context.CancelFunc, <-chan struct{}, func(), error) {
	if ctx == nil {
		return nil, nil, nil, nil, store.ErrInvalidRunDataIdentity
	}
	if e := ctx.Err(); e != nil {
		return nil, nil, nil, nil, e
	}
	c, cancel := context.WithCancel(ctx)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		cancel()
		return nil, nil, nil, nil, errors.New("runstream: file source closed")
	}
	if f.active == nil {
		f.active = make(map[uint64]context.CancelFunc)
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
			cancel()
			f.mu.Lock()
			delete(f.active, id)
			f.mu.Unlock()
			close(done)
			f.workers.Done()
		})
	}
	return c, cancel, done, finish, nil
}
func (f *FileSource) SubscribeEvents(ctx context.Context, id string, from int64) (EventSubscription, error) {
	return f.SubscribeEventsCursor(ctx, id, EventCursor{Seq: from})
}
func (f *FileSource) SubscribeEventsCursor(ctx context.Context, id string, position EventCursor) (EventSubscription, error) {
	c, cancel, done, finish, e := f.register(ctx)
	if e != nil {
		return nil, e
	}
	cap := store.AsFilesystemStreamStore(f.store)
	if cap == nil {
		finish()
		return nil, store.ErrRunDataCapability
	}
	pipe := NewEventPipeWithClose(func() { cancel(); <-done })
	go func() {
		defer finish()
		defer pipe.Finish(nil)
		selection, observed, e := cap.OpenFilesystemStream(c, id, store.FilesystemStreamOpenOptions{WaitForInitialDirectory: true})
		if e != nil {
			if IsPermanentError(e) || c.Err() == nil {
				pipe.fail(e)
			}
			return
		}
		if !observed || nilEventSelection(selection) {
			pipe.fail(store.ErrRunDataCapability)
			return
		}
		defer selection.Close()
		cursor := newFileEventDrain(selection, pipe, position.Seq)
		if cursor.admissionErr != nil {
			pipe.fail(cursor.admissionErr)
			return
		}
		cursor.after = position.After
		defer cursor.Close()
		f.runSelectedFile(c, selection, false, cursor.Drain, pipe.Warn, pipe.fail)
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
	pipe := NewLogPipeWithClose(func() { cancel(); <-done })
	go func() {
		defer finish()
		defer pipe.Finish(nil)
		selection, observed, e := cap.OpenFilesystemStream(c, id, store.FilesystemStreamOpenOptions{})
		if e != nil {
			if IsPermanentError(e) || c.Err() == nil {
				pipe.fail(e)
			}
			return
		}
		if !observed {
			return
		}
		if selection == nil {
			pipe.fail(store.ErrRunDataCapability)
			return
		}
		defer selection.Close()
		cursor := newFileLogDrain(selection, pipe, from)
		defer cursor.Close()
		// Absence is valid only after the retained observation was checked. A
		// joined identity/missing error must never be an empty successful log.
		file, e := selection.OpenLog(c)
		if IsPermanentError(e) {
			pipe.fail(e)
			return
		}
		if isFileMissing(e) {
			return
		}
		if e != nil {
			pipe.fail(e)
			return
		}
		if file == nil {
			pipe.fail(store.ErrRunDataCapability)
			return
		}
		file.Close()
		f.runSelectedFile(c, selection, true, cursor.Drain, pipe.Warn, pipe.fail)
	}()
	return pipe, nil
}

type fileRound struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	terminal bool
	err      error
}

func newFileRound(ctx context.Context, s store.FilesystemStreamSelection) *fileRound {
	c, cancel := context.WithCancel(ctx)
	r := &fileRound{ctx: c, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-c.Done():
				return
			case <-timer.C:
				terminal, e := s.Terminal(c)
				if e != nil || terminal {
					r.terminal = terminal
					r.err = e
					cancel()
					return
				}
				timer.Reset(fileTerminalPollInterval)
			}
		}
	}()
	return r
}
func (r *fileRound) finish(parent context.Context, e error) (bool, error) {
	r.cancel()
	<-r.done
	if mongoContextErrorOnly(e) && errors.Is(e, context.Canceled) && parent.Err() == nil {
		e = nil
	}
	pollErr := r.err
	if mongoContextErrorOnly(pollErr) && errors.Is(pollErr, context.Canceled) && parent.Err() == nil {
		pollErr = nil
	}
	return r.terminal, errors.Join(e, pollErr)
}
func (f *FileSource) runSelectedFile(ctx context.Context, s store.FilesystemStreamSelection, logs bool, drain func(context.Context) error, warn, fail func(error)) {
	eventDir, logDir := s.WatchHints()
	dir, name := eventDir, "events.jsonl"
	if logs {
		dir, name = logDir, "run.log"
	}
	delay := 250 * time.Millisecond
	for {
		round := newFileRound(ctx, s)
		e := watchFileDrains(round.ctx, dir, name, drain, f.logger)
		terminal, e := round.finish(ctx, e)
		if IsPermanentError(e) {
			fail(e)
			return
		}
		if ctx.Err() != nil || errors.Is(e, errEventDeliveryStopped) {
			return
		}
		if terminal && e == nil {
			e = drain(ctx)
			if IsPermanentError(e) {
				fail(e)
				return
			}
			if e == nil || errors.Is(e, errEventDeliveryStopped) {
				return
			}
		}
		if e == nil {
			e = fmt.Errorf("runstream: filesystem watch ended")
		}
		if !mongoRetry(ctx, e, delay, warn) {
			return
		}
		delay = nextMongoDelay(delay)
	}
}
