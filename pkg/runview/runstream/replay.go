package runstream

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// This file holds the small building blocks every backend shares:
// paginated event replay, the reconnect-with-backoff pump, and the
// offset-deduped log-chunk shipper.

// ReplayEvents pages the persisted backlog (seq >= fromSeq) into pipe
// in MaxEventsPerPage batches. Returns the highest shipped seq
// (fromSeq-1 when nothing shipped) and the first load error — the
// caller decides whether that error is fatal (store.ErrEventsCorrupted
// on the primary store) or just ends the replay early; pages loaded
// before the error are already shipped either way.
func ReplayEvents(ctx context.Context, st store.RunStore, runID string, fromSeq int64, pipe EventPipe) (int64, error) {
	maxReplayed := fromSeq - 1
	next := fromSeq
	for {
		page, err := st.LoadEventsRange(ctx, runID, next, 0, MaxEventsPerPage())
		if len(page) > 0 {
			if !pipe.Ship(ctx, page) {
				return maxReplayed, ctx.Err()
			}
			if last := page[len(page)-1].Seq; last > maxReplayed {
				maxReplayed = last
			}
		}
		if err != nil {
			return maxReplayed, err
		}
		if len(page) < MaxEventsPerPage() {
			return maxReplayed, nil
		}
		next = maxReplayed + 1
	}
}

// reconnectLoop drives attempt until it reports stop, succeeds after a
// cancellation (err == nil), or the ctx ends — retrying transient
// errors with exponential backoff (250ms → 30s ceiling). warn surfaces
// each retried error together with the chosen delay.
func reconnectLoop(ctx context.Context, warn func(delay time.Duration, err error), attempt func() (stop bool, err error)) {
	const baseBackoff = 250 * time.Millisecond
	const maxBackoff = 30 * time.Second
	backoff := baseBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		stop, err := attempt()
		if stop || err == nil {
			return
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		warn(backoff, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// ShipLogChunk delivers one log chunk deduped/sliced against the
// delivered high-water offset: fully-covered chunks are dropped,
// partially-covered ones are sliced at the boundary. data must be safe
// for the consumer to retain. Returns the new high-water offset and
// false when the subscription is over.
func ShipLogChunk(ctx context.Context, pipe LogPipe, off int64, data []byte, delivered int64) (int64, bool) {
	end := off + int64(len(data))
	if end <= delivered {
		return delivered, true
	}
	if off < delivered {
		data = data[delivered-off:]
		off = delivered
	}
	if !pipe.Ship(ctx, LogChunk{Offset: off, Data: data}) {
		return delivered, false
	}
	return end, true
}
