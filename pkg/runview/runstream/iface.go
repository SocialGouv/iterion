// Package runstream is the store-agnostic run-streaming seam (ADR-053):
// one Source per store delivers BOTH the structured event timeline and
// the raw log bytes of any run — persisted replay first, then live tail,
// gap-free — so the run-console WS layer never branches on how a run was
// produced (in-process launch, detached subprocess, external daemon,
// cross-store, cloud runner pod).
//
// Implementations:
//
//   - MongoSource (this package) — cloud: change streams over the events
//     and run_logs collections of the Mongo store.
//   - FileSource (this package) — a foreign filesystem store root
//     (cross-store observation): fsnotify tailers + run.json terminal poll.
//   - runview.Service's own source — the primary local store, backed by
//     the in-process EventBroker / RunLogBuffer fan-out plus on-demand
//     fsnotify tailers for runs this process didn't launch.
//
// The package is a leaf: it depends on pkg/store (types + read APIs) and
// infrastructure only, never back on pkg/runview.
package runstream

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/SocialGouv/iterion/pkg/store"
)

// MaxEventsPerPage caps the number of events a single replay batch (and
// a single store LoadEventsRange page) carries. One WS envelope ships at
// most this many events. Aliased by pkg/runview.MaxEventsPerPage.
//
// Adjustable, so a test can prove the pagination BOUNDARY without paying for
// the page size: covering it honestly means seeding more events than fit in
// one page, and at 25000 that was a 33-second test whose replay window then
// had to be widened until it was really measuring throughput on a loaded
// runner. Shrink it with SetMaxEventsPerPageForTest.
//
// Atomic because replay goroutines read it while a test's cleanup restores it.
var maxEventsPerPage atomic.Int64

func init() { maxEventsPerPage.Store(25000) }

// MaxEventsPerPage returns the current page cap.
func MaxEventsPerPage() int { return int(maxEventsPerPage.Load()) }

// SetMaxEventsPerPageForTest lowers the page cap for the duration of a test
// and returns a restore func. Test-only: production has no reason to move it.
func SetMaxEventsPerPageForTest(n int) func() {
	prev := maxEventsPerPage.Swap(int64(n))
	return func() { maxEventsPerPage.Store(prev) }
}

// ErrLogsUnsupported is returned by SubscribeLogs on a source that was
// constructed without a log backend (a MongoSource with no run_logs
// collection — a wiring error, not a runtime condition).
var ErrLogsUnsupported = errors.New("runstream: this source does not stream logs")

// Source is the long-lived streaming gateway for one store. Open one per
// store; spawn a subscription per WS client. Subscriptions deliver the
// persisted backlog from the requested anchor and then the live tail,
// with no gap between the two phases.
type Source interface {
	// SubscribeEvents delivers every event with Seq >= fromSeq
	// at-least-once and in order, batched: replay pages carry up to
	// MaxEventsPerPage events, live deliveries carry one. Unpersisted
	// Seq==0 events (store.EventAlert) may additionally be delivered
	// out-of-band on sources fed by the in-process broker.
	SubscribeEvents(ctx context.Context, runID string, fromSeq int64) (EventSubscription, error)
	// SubscribeLogs delivers log bytes from fromOffset, offset-tagged.
	// Consumers dedup/slice by offset (chunks may overlap on reconnect,
	// and a truncated/rotated producer file re-anchors at a lower
	// offset).
	SubscribeLogs(ctx context.Context, runID string, fromOffset int64) (LogSubscription, error)
	// Close releases pooled resources. Safe to call multiple times.
	// Open subscriptions are cancelled.
	Close() error
}

// EventSubscription is one client's view of a run's event stream.
// Events() closing means the stream ended: the run reached a terminal
// status on sources that watch for it, or a fatal error occurred — in
// the latter case the error is delivered on Errors() before the close.
type EventSubscription interface {
	Events() <-chan []*store.Event
	// Errors carries non-fatal warnings (e.g. a change-stream reconnect
	// notice). A fatal error is sent here and then both channels close.
	Errors() <-chan error
	// Close ends the subscription and releases any shared/refcounted
	// backend resources. Idempotent.
	Close() error
}

// LogChunk is one delivered span of the run's log byte stream. Offset
// is the absolute byte position of Data[0] in the run's log; the
// producer's running total at emit time is Offset + len(Data).
type LogChunk struct {
	Offset int64
	Data   []byte
}

// LogSubscription is one client's view of a run's log stream. Chunks()
// closing means the log stream ended (run terminal, or no log will ever
// exist for this run).
type LogSubscription interface {
	Chunks() <-chan LogChunk
	Errors() <-chan error
	Close() error
}
