package store

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var ErrRunDataCapability = errors.New("store: run data capability unavailable")

// RunDataHeaderStore observes metadata without decoding execution payloads.
// Providing this capability does not advertise protocol-one data storage.
type RunDataHeaderStore interface {
	ObserveRunDataHeader(context.Context, string) (RunDataHeader, error)
}

func AsRunDataHeaderStore(s RunStore) RunDataHeaderStore {
	return capability[RunDataHeaderStore](s)
}

// ObserveRunDataHeader is for a deliberate initial observation. Retained
// contexts are compared, never replaced; there is no full-payload fallback.
func ObserveRunDataHeader(ctx context.Context, s RunStore, id string) (RunDataHeader, error) {
	cap := AsRunDataHeaderStore(s)
	if cap == nil {
		return RunDataHeader{}, ErrRunDataCapability
	}
	h, err := cap.ObserveRunDataHeader(ctx, id)
	if err != nil {
		return RunDataHeader{}, err
	}
	return ValidateRunDataObservation(ctx, id, h)
}

// ValidateRunDataObservation permits a fresh header-only observation while
// retaining any original binding already supplied by the caller. It grants
// neither payload access nor execution authority.
func ValidateRunDataObservation(ctx context.Context, id string, h RunDataHeader) (RunDataHeader, error) {
	if ctx == nil {
		return RunDataHeader{}, ErrInvalidRunDataIdentity
	}
	if err := ctx.Err(); err != nil {
		return RunDataHeader{}, err
	}
	h, err := h.validate()
	if err != nil {
		return RunDataHeader{}, err
	}
	if id == "" || h.RunID != id {
		return RunDataHeader{}, ErrRunDataIdentityChanged
	}
	if _, bound := RunDataBindingFrom(ctx); bound {
		if err := CheckRunDataBinding(ctx, h); err != nil {
			return RunDataHeader{}, err
		}
	}
	if _, bound := ctx.Value(runValueFormatKey{}).(RunValueFormat); bound {
		if _, err := CheckRunValueFormat(ctx, id, int(h.Format)); err != nil {
			return RunDataHeader{}, err
		}
	}
	return h, nil
}

func (s *FilesystemRunStore) ObserveRunDataHeader(ctx context.Context, id string) (RunDataHeader, error) {
	if ctx == nil {
		return RunDataHeader{}, ErrInvalidRunDataIdentity
	}
	if err := ctx.Err(); err != nil {
		return RunDataHeader{}, err
	}
	if err := sanitizePathComponent("run ID", id); err != nil {
		return RunDataHeader{}, err
	}
	lock, err := s.acquireRunWriteLock(id)
	if err != nil {
		return RunDataHeader{}, err
	}
	defer func() { _ = lock.Unlock() }()
	if err := ctx.Err(); err != nil {
		return RunDataHeader{}, err
	}
	raw, err := os.ReadFile(s.runJSONPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			if deleted := s.guardNotDeleted(id); deleted != nil {
				return RunDataHeader{}, deleted
			}
			return RunDataHeader{}, fmt.Errorf("store: observe run %s: %w: %w", id, ErrRunNotFound, err)
		}
		return RunDataHeader{}, err
	}
	h, err := RunDataHeaderFromJSON(ctx, raw)
	if err != nil {
		return RunDataHeader{}, err
	}
	h, err = ValidateRunDataObservation(ctx, id, h)
	if err != nil {
		return RunDataHeader{}, err
	}
	if err := s.guardNotDeleted(id); err != nil {
		return RunDataHeader{}, err
	}
	return h, nil
}

var _ RunDataHeaderStore = (*FilesystemRunStore)(nil)
