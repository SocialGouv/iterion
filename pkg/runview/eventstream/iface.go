// Package eventstream abstracts the live + historical event source
// the run console subscribes to. Two implementations:
//
//   - FilesystemSource (T-20) — wraps the existing file watcher
//     (events.jsonl tailing) used by local-mode iterion.
//   - MongoSource (T-22) — opens a Mongo change stream filtered to
//     a single run, plus a backfill range for events that were
//     persisted before the WS connected.
//
// Pulling the abstraction lets the runview HTTP layer pick the right
// pipeline based on store.Capabilities().LiveStream without caring
// about the on-disk vs Mongo implementation detail.
//
// Plan §F (T-20, T-22).
package eventstream

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Source is the long-lived event-stream gateway. Open one Source per
// store at server boot; spawn a Subscription per WS connection.
type Source interface {
	// Subscribe returns a stream of events for the given run, starting
	// at fromSeq. Pass fromSeq=0 to receive every persisted event;
	// pass a higher value when the client has already seen events up
	// to fromSeq-1 (catch-up resume after a WS reconnect).
	Subscribe(ctx context.Context, runID string, fromSeq int64) (Subscription, error)
	// Capabilities advertises what the source can do so the HTTP
	// layer can fall back gracefully.
	Capabilities() SourceCapabilities
	// Close releases any pooled resources (Mongo change-stream
	// cursors, fsnotify watchers). Safe to call multiple times.
	Close() error
}

// Subscription is one WS-client view of the stream. Events arrive
// in seq order. The Errors channel receives non-fatal warnings; on
// fatal failure the channel is closed and the caller must
// re-subscribe (or fall back to REST polling).
type Subscription interface {
	// Events returns the receive-only channel of events. Closed when
	// the subscription is finished (Close called or fatal error).
	Events() <-chan *store.Event
	// Errors returns non-fatal errors observed during streaming.
	// Drained on Close.
	Errors() <-chan error
	// Close ends the subscription. Idempotent.
	Close() error
}

// SourceCapabilities describes what a Source impl can do. The HTTP
// layer reads this once at boot and routes WS requests accordingly:
// when LiveTail is false, the layer falls back to client-driven
// polling instead of a long-held connection.
type SourceCapabilities struct {
	// LiveTail is true when the source can push events as soon as
	// they are persisted (filesystem fsnotify, Mongo change stream).
	LiveTail bool
	// HistoricalRange is true when the source can serve a
	// fromSeq → lastSeq backfill before transitioning to live
	// (Mongo cursor + change stream resume token).
	HistoricalRange bool
}
