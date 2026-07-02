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
//     (and, once wired, run_logs) collections of the Mongo store.
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

	"github.com/SocialGouv/iterion/pkg/store"
)

// MaxEventsPerPage caps the number of events a single replay batch (and
// a single store LoadEventsRange page) carries. One WS envelope ships at
// most this many events. Aliased by pkg/runview.MaxEventsPerPage.
const MaxEventsPerPage = 25000

// ErrLogsUnsupported is returned by SubscribeLogs on sources that cannot
// stream logs (Capabilities().Logs == false). Callers treat it as "no
// log stream will ever exist here", not as a transient failure.
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
	// offset). Returns ErrLogsUnsupported when Capabilities().Logs is
	// false.
	SubscribeLogs(ctx context.Context, runID string, fromOffset int64) (LogSubscription, error)
	// Capabilities advertises what this source can do so callers can
	// degrade gracefully.
	Capabilities() Capabilities
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

// LogChunk is one delivered span of the run's log byte stream.
type LogChunk struct {
	// Offset is the absolute byte position of Data[0] in the run's log.
	Offset int64
	Data   []byte
	// Total is the producer's running byte counter at emit time
	// (Offset + len(Data) for in-order delivery). Lets clients detect
	// drops and re-anchor.
	Total int64
}

// LogSubscription is one client's view of a run's log stream. Chunks()
// closing means the log stream ended (run terminal, or no log will ever
// exist for this run).
type LogSubscription interface {
	Chunks() <-chan LogChunk
	Errors() <-chan error
	Close() error
}

// Capabilities describes what a Source implementation can do.
type Capabilities struct {
	// LiveTail is true when the source pushes new data as soon as it is
	// persisted (fsnotify, Mongo change stream).
	LiveTail bool
	// HistoricalRange is true when the source can serve a from→end
	// backfill before transitioning to live.
	HistoricalRange bool
	// Logs is true when SubscribeLogs is functional. False routes
	// callers to their no-log fallback (migration gate while the cloud
	// log pipeline lands).
	Logs bool
}
