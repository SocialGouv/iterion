package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// Read synchronization is either owned, quiescent but non-owned, busy, or
// failed. In particular a missing/dead PID entry is never an acquired lock.
type streamReadLockOutcome uint8

const (
	streamReadLockFailure streamReadLockOutcome = iota
	streamReadLockOwned
	streamReadLockQuiescent
	streamReadLockBusy
)

func acquireStreamMetadataReadAt(ctx context.Context, id, root string) (RunLock, streamReadLockOutcome, error) {
	if ctx == nil {
		return nil, streamReadLockFailure, ErrInvalidRunDataIdentity
	}
	if e := sanitizePathComponent("run ID", id); e != nil {
		return nil, streamReadLockFailure, e
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		if e := ctx.Err(); e != nil {
			return nil, streamReadLockFailure, e
		}
		lock, state, e := tryStreamReadLock(filepath.Join(root, ".run-write-locks", id), fmt.Sprintf("run %s document mutation", id))
		switch state {
		case streamReadLockOwned:
			if ce := ctx.Err(); ce != nil {
				return nil, streamReadLockFailure, errors.Join(ce, lock.Unlock())
			}
			return lock, state, e
		case streamReadLockQuiescent:
			if ce := ctx.Err(); ce != nil {
				return nil, streamReadLockFailure, ce
			}
			return nil, state, e
		case streamReadLockFailure:
			return nil, state, e
		case streamReadLockBusy:
			timer := time.NewTimer(5 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, streamReadLockFailure, ctx.Err()
			case <-deadline.C:
				timer.Stop()
				return nil, streamReadLockFailure, fmt.Errorf("store: run %s document mutation lock: %w", id, e)
			case <-timer.C:
			}
		}
	}
}
