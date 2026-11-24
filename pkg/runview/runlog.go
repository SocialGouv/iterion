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

	// birth is the birth of the run this buffer was created for, recorded
	// once (SetBirth): the buffer is keyed by run ID alone, and a run
	// deleted and recreated under the ID may still find its predecessor's
	// buffer registered. birthKnown is false until it is recorded.
	birth      string
	birthKnown bool

	// lane is the log registry epoch this buffer's producer publishes to:
	// a payload-free wake after each byte reaches the file, and the epoch's
	// one end when the buffer's last holder releases it. Nil for a legacy
	// tailer buffer and for stores that persist no run logs.
	lane *logLane
	// onTeeFailure types a failed file write (the lane's single typing
	// site): the stream must fail, never wake on bytes the store lacks.
	onTeeFailure func(error)
	// end is the value setLogEnd recorded; releaseLogLane publishes it,
	// defaulting to logEndTerminal (a closed buffer's run has finished).
	end logEnd
	// fileErr is the first failed file write. Once set, no further wake is
	// published: the store is missing bytes and a wake would lie.
	fileErr error
	// laneHandled records that the lane registration was attempted, so a
	// second ownership pass does not recapture and re-adopt.
	laneHandled bool
	laneOnce    sync.Once
}

// markLaneHandled records that a lane registration was attempted for the
// buffer, whatever its outcome.
func (b *RunLogBuffer) markLaneHandled() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.laneHandled = true
}

// laneRegistered reports whether a lane registration was already attempted.
func (b *RunLogBuffer) laneRegistered() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.laneHandled
}

// attachLogLane binds the buffer to its producer lane and its failure hook.
func (b *RunLogBuffer) attachLogLane(l *logLane, onTeeFailure func(error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lane = l
	b.onTeeFailure = onTeeFailure
}

// setLogEnd records the value the lane publishes at release. The first value
// wins: a producer's fin is sticky.
func (b *RunLogBuffer) setLogEnd(e logEnd) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.end == logEndNone {
		b.end = e
	}
}

// fileTee reports whether writes reach the buffer's file right now.
func (b *RunLogBuffer) fileTee() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.file != nil && b.fileErr == nil
}

// persistFailure returns the first failed file write, nil when none.
func (b *RunLogBuffer) persistFailure() error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.fileErr
}

// releaseLogLane returns the producer's bail exactly once: the LAST bail
// publishes the epoch's end (the recorded setLogEnd, else logEndTerminal) and
// the topic is retained for a subscriber that parks across the close.
func (b *RunLogBuffer) releaseLogLane() {
	if b == nil {
		return
	}
	b.laneOnce.Do(func() {
		b.mu.RLock()
		l, end := b.lane, b.end
		b.mu.RUnlock()
		if l == nil {
			return
		}
		if end == logEndNone {
			end = logEndTerminal
		}
		l.Close(end)
	})
}

// SetBirth records the birth of the run the buffer was created for.
func (b *RunLogBuffer) SetBirth(birth string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.birth, b.birthKnown = birth, true
}

// BornOf reports whether the buffer was created for the run of this birth:
// a buffer whose birth was never recorded belongs to no known run.
func (b *RunLogBuffer) BornOf(birth string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.birthKnown && b.birth == birth
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
	// Seed the ring's "logical stream offset" from any existing on-disk
	// content. A resumed run reopens an existing run.log via O_APPEND;
	// without this seed, the buffer's start/written would be 0 and
	// Snapshot(from=0) would return offset=0 — the studio's HTTP /log
	// gap-fill (handleGetRunLog) and the WS subscribe_logs replayer
	// both gate on `offset > from`, so neither would ever read the
	// pre-restart history off disk. Symptom: per-node Logs tab empty
	// in the studio for any node that ran before the daemon restart.
	// Seeding start = written = file-size leaves the ring empty for now
	// (subsequent Writes append normally) but tells consumers the gap
	// `[0, file-size)` lives on disk.
	if stat, err := f.Stat(); err == nil {
		size := stat.Size()
		b.start = size
		b.written = size
	}
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
	// in-memory ring's logical order. A failed write is kept — the first one
	// is what the stream fails on — and is reported after the unlock.
	var firstTeeErr error
	if b.file != nil {
		if _, err := b.file.Write(p); err != nil && b.fileErr == nil {
			b.fileErr, firstTeeErr = err, err
		}
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
	// The wake follows the file write (flush-before-publish): a wake implies
	// the store sees the bytes. A failed tee never wakes — the stream fails
	// through onTeeFailure instead.
	var (
		wake         bool
		lane         *logLane
		onTeeFailure func(error)
	)
	if b.lane != nil && b.fileErr == nil {
		wake, lane = true, b.lane
	}
	if firstTeeErr != nil {
		onTeeFailure = b.onTeeFailure
	}
	b.mu.Unlock()

	if firstTeeErr != nil && onTeeFailure != nil {
		onTeeFailure(firstTeeErr)
	}
	if wake {
		lane.Wake()
	}
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
// snapshot tail. A subscription to a closed buffer is closed already: no
// Close will ever end it.
func (b *RunLogBuffer) Subscribe() *RunLogSubscription {
	sub := &runLogSub{ch: make(chan runLogChunk, runLogSubBufferSize)}

	b.mu.Lock()
	if b.closed {
		sub.closed = true
		close(sub.ch)
	} else {
		b.subs = append(b.subs, sub)
	}
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
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	subs := b.subs
	b.subs = nil
	if b.file != nil {
		_ = b.file.Close()
		b.file = nil
	}

	// Close under the lock: Subscribe's cancel closure also writes
	// sub.closed / closes sub.ch under b.mu. Doing it after Unlock left
	// no happens-before with a concurrent Cancel for the same sub — a
	// data race on sub.closed and, worse, a double close(sub.ch) panic.
	// Same fix as EventBroker.CloseRun; close() is cheap and Write's
	// sends are non-blocking, so holding the lock is safe.
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
