package store

import (
	"context"
	"os"
)

// FilesystemStreamStore acquires a read-only original filesystem observation.
// observed=false is only an unbound, initially absent run directory, without
// WaitForInitialDirectory. A retained caller never becomes unobserved again.
type FilesystemStreamStore interface {
	OpenFilesystemStream(context.Context, string, FilesystemStreamOpenOptions) (selection FilesystemStreamSelection, observed bool, err error)
}
type FilesystemStreamOpenOptions struct{ WaitForInitialDirectory bool }

// FilesystemStreamSelection retains an original directory/header observation.
// OpenEvents/OpenLog return caller-owned descriptors. A consumer validates the
// original observation before delivering bytes read from those descriptors;
// opening and validation are not an atomic transaction spanning external IO.
// Close releases the selection roots, not descriptors already returned.
type FilesystemStreamSelection interface {
	Validate(context.Context) error
	Terminal(context.Context) (bool, error)
	OpenEvents(context.Context) (*os.File, error)
	OpenLog(context.Context) (*os.File, error)
	DecodeEvent(context.Context, []byte) (*Event, error)
	WatchHints() (eventsParent, logsParent string)
	Close() error
}

func AsFilesystemStreamStore(s RunStore) FilesystemStreamStore {
	return capability[FilesystemStreamStore](s)
}

// CheckEventStreamCorruption shares the persisted reader's legacy corruption
// policy with streaming readers. Exact decode failures are refused separately.
func CheckEventStreamCorruption(skipped, valid int) error {
	if eventsCorruptionExceeded(skipped, valid) {
		return ErrEventsCorrupted
	}
	return nil
}
