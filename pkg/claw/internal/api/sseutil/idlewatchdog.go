package sseutil

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultStreamIdleTimeout is the gap between SSE lines tolerated before a
// stalled stream is aborted. Generous so long server-side reasoning (which
// still streams reasoning/keepalive lines) is never mistaken for a stall.
const DefaultStreamIdleTimeout = 5 * time.Minute

// StreamIdleTimeout resolves the idle timeout for streaming reads. It reads
// CLAW_STREAM_IDLE_TIMEOUT (a Go duration like "3m", or a bare number of
// seconds); 0 / negative disables the watchdog; an unset/invalid value falls
// back to DefaultStreamIdleTimeout. Shared by every SSE read loop so the knob
// is uniform across providers.
func StreamIdleTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("CLAW_STREAM_IDLE_TIMEOUT"))
	if v == "" {
		return DefaultStreamIdleTimeout
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return DefaultStreamIdleTimeout
}

// IdleWatchdog guards a streaming SSE read against a stalled connection that
// delivers no bytes for an extended period WITHOUT returning an error —
// observed with gpt-5 / ChatGPT-forfait Responses streams that hang
// mid-response. A blocking bufio.Scanner.Scan() (or any resp.Body read) then
// parks forever, so a single stalled call wedges the whole agent turn: the
// only thing that eventually unblocks it is an outer deadline (and in
// iterion's dispatched mode that is a 10-min stall timeout followed by a
// destructive `docker rm --force`, which SIGKILLs the runner and poisons the
// retry context).
//
// The watchdog closes the underlying stream when no Touch() has occurred
// within `idle`, which unblocks the parked read; the caller checks Fired() to
// surface a clear, retryable "stream stalled" error instead of a generic
// "read on closed body". Any received SSE line counts as activity (call
// Touch() once per line, including comments / keepalives), so only true
// silence trips it — long server-side reasoning that still streams deltas or
// keepalives keeps the connection considered live.
type IdleWatchdog struct {
	fired atomic.Bool
	last  atomic.Int64 // unixnano of the last Touch
	stop  chan struct{}
	once  sync.Once
}

// NewIdleWatchdog starts a watchdog that closes body when idle for `idle`, or
// stops cleanly on ctx cancellation / Stop(). An idle <= 0 disables it (the
// returned watchdog never fires and starts no goroutine) so callers can wire
// it unconditionally and gate behaviour purely on the configured duration.
func NewIdleWatchdog(ctx context.Context, body io.Closer, idle time.Duration) *IdleWatchdog {
	w := &IdleWatchdog{stop: make(chan struct{})}
	w.last.Store(time.Now().UnixNano())
	if idle <= 0 {
		return w
	}
	go func() {
		// Poll at a quarter of the idle window so worst-case overshoot past
		// the deadline is bounded, with a 1s floor.
		tick := idle / 4
		if tick < time.Second {
			tick = time.Second
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			case now := <-t.C:
				if now.UnixNano()-w.last.Load() >= int64(idle) {
					w.fired.Store(true)
					_ = body.Close() // unblocks the parked Scan()/Read()
					return
				}
			}
		}
	}()
	return w
}

// Touch records stream activity, resetting the idle timer. Cheap; call once
// per received line.
func (w *IdleWatchdog) Touch() { w.last.Store(time.Now().UnixNano()) }

// Stop ends the watchdog. Idempotent; safe to defer.
func (w *IdleWatchdog) Stop() { w.once.Do(func() { close(w.stop) }) }

// Fired reports whether the watchdog tripped (idle timeout → stream closed),
// so the caller can emit a clear stalled-stream error rather than the generic
// read error that the forced Close() produces.
func (w *IdleWatchdog) Fired() bool { return w.fired.Load() }
