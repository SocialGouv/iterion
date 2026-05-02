package runview

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// runLogRingCap caps the in-memory tail kept for late subscribers and
// REST `?from=` queries. A long-running workflow can produce megabytes
// of log output; once the cap is exceeded, the oldest bytes are
// evicted and the buffer's `start` offset advances. Subscribers asking
// for an offset older than `start` get the oldest available bytes
// instead — they can detect the truncation by comparing the requested
// offset against the response's start offset.
const runLogRingCap = 1 << 20 // 1 MiB

// runLogSubBufferSize matches the event broker pattern: lossy fan-out
// with re-anchor recovery. A slow log subscriber whose buffer fills
// gets dropped silently; the client can re-subscribe with from_offset
// to recover from the persisted file.
const runLogSubBufferSize = 256

// runLogChunk is the unit of fan-out. Offset is the byte position in
// the run's logical stream where Bytes begins; subscribers reconcile
// against catch-up data via (offset, len(bytes)).
type runLogChunk struct {
	Offset int64
	Bytes  []byte
}

type runLogSub struct {
	ch     chan runLogChunk
	closed bool
	drops  atomic.Int32
}

// RunLogSubscription mirrors EventSubscription: receive on C, call
// Cancel to unregister. Drops counts chunks lost to a full buffer;
// clients that see drops > 0 should re-anchor via the REST endpoint.
type RunLogSubscription struct {
	C       <-chan runLogChunk
	cancel  func()
	once    sync.Once
	dropsFn func() int
}

// Cancel unregisters the subscription. Idempotent.
func (s *RunLogSubscription) Cancel() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

func (s *RunLogSubscription) Drops() int {
	if s.dropsFn == nil {
		return 0
	}
	return s.dropsFn()
}

// RunLogBuffer captures per-run logger output: io.Writer for
// io.MultiWriter composition, a bounded ring for catch-up replay,
// fan-out to live subscribers, and an optional file tee for
// post-mortem inspection. Thread-safe.
type RunLogBuffer struct {
	mu       sync.RWMutex
	ring     []byte // ring of size runLogRingCap; only filled up to len(ring)
	start    int64  // byte offset of ring[0] in the logical stream
	written  int64  // total bytes ever written (== start + len(ring) when full, else == len(ring))
	closed   bool
	file     *os.File // optional persisted tee; nil when persistence disabled
	subs     []*runLogSub
	dropsLog atomic.Int64 // best-effort counter of total dropped chunks across subscribers
}

// NewRunLogBuffer creates an empty buffer. Pass filePath to also
// persist writes to disk; pass "" to keep the buffer purely in-memory.
// The returned buffer is always usable; fileErr is non-nil when the
// optional disk persistence couldn't be set up — callers should log a
// warning and proceed (the in-memory ring is the source of truth for
// WS subscribers; the file is best-effort post-mortem).
//
// O_APPEND so a resumed run extends the existing file rather than
// truncating it.
func NewRunLogBuffer(filePath string) (*RunLogBuffer, error) {
	b := &RunLogBuffer{ring: make([]byte, 0, runLogRingCap)}
	if filePath == "" {
		return b, nil
	}
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return b, err
	}
	b.file = f
	return b, nil
}

// Write implements io.Writer.
//
// io.Writer's contract permits the caller to reuse p after Write
// returns, so we copy bytes before retaining them — the ring (via
// append) and the subscriber channel each get their own copy.
func (b *RunLogBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return len(p), nil
	}

	// File tee under the mutex so the file's byte order matches the
	// in-memory ring's logical order.
	if b.file != nil {
		_, _ = b.file.Write(p)
	}

	chunkOffset := b.written
	b.appendLocked(p)

	chunkCopy := make([]byte, len(p))
	copy(chunkCopy, p)
	subs := b.subs
	chunk := runLogChunk{Offset: chunkOffset, Bytes: chunkCopy}
	for _, sub := range subs {
		if sub.closed {
			continue
		}
		select {
		case sub.ch <- chunk:
		default:
			sub.drops.Add(1)
			b.dropsLog.Add(1)
		}
	}
	b.mu.Unlock()
	return len(p), nil
}

// appendLocked appends p to the ring, evicting the oldest bytes when
// the total would exceed runLogRingCap. Caller holds b.mu.
func (b *RunLogBuffer) appendLocked(p []byte) {
	if len(b.ring)+len(p) <= runLogRingCap {
		b.ring = append(b.ring, p...)
		b.written += int64(len(p))
		return
	}

	if len(p) >= runLogRingCap {
		b.ring = append(b.ring[:0], p[len(p)-runLogRingCap:]...)
		b.start = b.written + int64(len(p)) - runLogRingCap
		b.written += int64(len(p))
		return
	}

	drop := len(b.ring) + len(p) - runLogRingCap
	copy(b.ring, b.ring[drop:])
	b.ring = b.ring[:len(b.ring)-drop]
	b.ring = append(b.ring, p...)
	b.start += int64(drop)
	b.written += int64(len(p))
}

// Snapshot returns the bytes available with offset >= from. The first
// returned int is the actual offset of the returned bytes (== from
// when from is in-range, == b.start when from is older than the
// retained tail). The second return is total bytes ever written, so
// callers can detect whether more will arrive.
//
// Pass from=0 for "everything we still have".
func (b *RunLogBuffer) Snapshot(from int64) (offset int64, data []byte, total int64) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	total = b.written
	if from >= b.written {
		return b.written, nil, total
	}
	if from < b.start {
		from = b.start
	}
	rel := int(from - b.start)
	if rel < 0 || rel > len(b.ring) {
		return b.start, nil, total
	}
	out := make([]byte, len(b.ring)-rel)
	copy(out, b.ring[rel:])
	return from, out, total
}

// Subscribe registers a live consumer. Callers should Snapshot(from)
// historical bytes BEFORE Subscribe to avoid racing the next Write,
// then drain live chunks dropping any whose offset overlaps the
// snapshot tail.
func (b *RunLogBuffer) Subscribe() *RunLogSubscription {
	sub := &runLogSub{ch: make(chan runLogChunk, runLogSubBufferSize)}

	b.mu.Lock()
	b.subs = append(b.subs, sub)
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, s := range b.subs {
			if s == sub {
				b.subs = append(b.subs[:i], b.subs[i+1:]...)
				break
			}
		}
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	return &RunLogSubscription{
		C:       sub.ch,
		cancel:  cancel,
		dropsFn: func() int { return int(sub.drops.Load()) },
	}
}

// Close terminates all live subscriptions and closes the persisted
// file. Late writes from goroutines racing run completion are
// silently dropped to avoid surfacing benign races as errors.
func (b *RunLogBuffer) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	subs := b.subs
	b.subs = nil
	if b.file != nil {
		_ = b.file.Close()
		b.file = nil
	}
	b.mu.Unlock()

	for _, sub := range subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
}

// Total returns the total bytes ever written.
func (b *RunLogBuffer) Total() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.written
}

var _ io.Writer = (*RunLogBuffer)(nil)
