package store

import "context"

// WatcherCursorStore is the granular persistence capability for supervisor
// cursors. A watcher must not whole-document replace a run while the engine or
// an operator is changing status/checkpoint fields.
type WatcherCursorStore interface {
	SetWatcherCursor(ctx context.Context, runID, watcherID string, cursor WatcherCursor) error
}

func AsWatcherCursorStore(s RunStore) WatcherCursorStore {
	if s == nil {
		return nil
	}
	c, _ := s.(WatcherCursorStore)
	return c
}
