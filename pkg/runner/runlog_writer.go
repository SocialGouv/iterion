package runner

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// runLogWriter tees the per-run logger's bytes into the store's
// RunLogStore (the run_logs chunk collection in cloud mode) so the
// server pod can stream and replay the run's log without a shared
// filesystem (ADR-053). Writes append to an in-memory batch under a
// mutex — never blocking the engine on store I/O — and a background
// flusher persists the batch when it reaches runLogFlushBytes or every
// runLogFlushInterval, whichever comes first.
//
// Offsets are absolute stream positions seeded from RunLogSize at run
// claim, so a resumed/redelivered run appends after the persisted tail
// instead of overlapping it. The writer is the single producer per run
// (queue MaxAckPending=1 + run lock); the store's unique
// (run_id, offset) index is the safety net.
//
// Failure policy: a flush retries a few times with a short backoff and
// then DROPS the batch loudly (ERROR log + dropped-bytes counter) —
// the log stream is a derived observability view of the run, and
// killing or stalling a paid LLM run because the store hiccuped on a
// log chunk would be disproportionate. The degradation is explicit,
// never silent; the offset still advances so later chunks keep their
// true positions (a hole, not corruption).
type runLogWriter struct {
	ctx    context.Context // background ctx carrying the run's tenant identity — NOT the run ctx (a cancelled run must still flush its tail)
	store  store.RunLogStore
	runID  string
	logger *iterlog.Logger

	mu     sync.Mutex
	buf    []byte
	offset int64 // absolute offset of buf[0]
	total  int64 // running bytes-written counter (flushed or pending)
	closed bool

	dropped atomic.Int64

	flushCh chan struct{}
	stopCh  chan struct{}
	doneCh  chan struct{}
}

const (
	runLogFlushBytes    = 32 * 1024
	runLogFlushInterval = 500 * time.Millisecond
	runLogFlushRetries  = 3
)

// registerLogWriter exposes the run's writer to the store's
// LogPositionFn hook (logWriterTotal) so AppendEvent can stamp
// Event.LogOffset. The runner is sequential (MaxAckPending=1) but the
// map keeps the wiring correct regardless.
func (r *Runner) registerLogWriter(runID string, w *runLogWriter) {
	r.logWritersMu.Lock()
	if r.logWriters == nil {
		r.logWriters = make(map[string]*runLogWriter)
	}
	r.logWriters[runID] = w
	r.logWritersMu.Unlock()
}

func (r *Runner) unregisterLogWriter(runID string) {
	r.logWritersMu.Lock()
	delete(r.logWriters, runID)
	r.logWritersMu.Unlock()
}

// logWriterTotal is the store.LogPositionFn the runner installs on
// stores that support SetLogPositionFn: the run's current log byte
// total, 0 when no writer is active for that run.
func (r *Runner) logWriterTotal(runID string) int64 {
	r.logWritersMu.Lock()
	w := r.logWriters[runID]
	r.logWritersMu.Unlock()
	if w == nil {
		return 0
	}
	return w.Total()
}

// logPositionSetter matches the SetLogPositionFn hook both store
// backends expose (filesystem + mongo) without importing the concrete
// types.
type logPositionSetter interface {
	SetLogPositionFn(store.LogPositionFn)
}

// newRunLogWriter starts the background flusher. seed is the absolute
// offset of the first byte this writer will produce (RunLogSize at
// claim time). Callers must Close to flush the tail.
func newRunLogWriter(ctx context.Context, ls store.RunLogStore, runID string, seed int64, logger *iterlog.Logger) *runLogWriter {
	w := &runLogWriter{
		ctx:     ctx,
		store:   ls,
		runID:   runID,
		logger:  logger,
		offset:  seed,
		total:   seed,
		flushCh: make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	go w.flusher()
	return w
}

// Write implements io.Writer: append to the pending batch and nudge the
// flusher past the size threshold. Never blocks on store I/O.
func (w *runLogWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	w.total += int64(len(p))
	full := len(w.buf) >= runLogFlushBytes
	w.mu.Unlock()
	if full {
		select {
		case w.flushCh <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

// Total returns the running bytes-written counter (flushed or pending)
// — the value AppendEvent stamps as Event.LogOffset so the studio's
// per-node log slicing lines up with the byte stream.
func (w *runLogWriter) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// Close stops the flusher after a final flush of the pending tail.
// Idempotent.
func (w *runLogWriter) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	close(w.stopCh)
	<-w.doneCh
	return nil
}

func (w *runLogWriter) flusher() {
	defer close(w.doneCh)
	t := time.NewTicker(runLogFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stopCh:
			w.flush()
			return
		case <-w.flushCh:
			w.flush()
		case <-t.C:
			w.flush()
		}
	}
}

// flush persists the pending batch, applying the retry-then-drop-loudly
// policy documented on the type.
func (w *runLogWriter) flush() {
	w.mu.Lock()
	if len(w.buf) == 0 {
		w.mu.Unlock()
		return
	}
	data := w.buf
	off := w.offset
	w.buf = nil
	w.offset = off + int64(len(data))
	w.mu.Unlock()

	var err error
	for attempt := 0; attempt < runLogFlushRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
			case <-w.ctx.Done():
				// Identity ctx cancelled (process shutdown) — one last
				// immediate attempt below, then drop.
			}
		}
		if err = w.store.AppendRunLog(w.ctx, w.runID, off, data); err == nil {
			return
		}
	}
	total := w.dropped.Add(int64(len(data)))
	w.logger.Error("runner: run %s: DROPPING %d log bytes at offset %d after %d attempts: %v (total dropped this run: %d)",
		w.runID, len(data), off, runLogFlushRetries, err, total)
}
