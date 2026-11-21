package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// valueRecordFormatLocked is called with the stable run lock held. Missing
// parents retain the historical legacy orphan APIs; exact writers need a birth.
func (s *FilesystemRunStore) valueRecordFormatLocked(ctx context.Context, id string, writing bool) (ValueFormat, *Run, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if err := s.guardNotDeleted(id); err != nil {
		return 0, nil, err
	}
	r, err := s.loadRunRawContext(ctx, id)
	raw := 0
	if err != nil && !errors.Is(err, ErrRunNotFound) {
		return 0, nil, err
	}
	if r == nil {
		if err := CheckMissingRunDataParent(ctx, id); err != nil {
			return 0, nil, err
		}
	}
	if r != nil {
		raw = r.FormatVersion
	}
	f, err := NormalizeValueFormat(raw)
	if err != nil {
		return 0, nil, err
	}
	_, bound := ctx.Value(runValueFormatKey{}).(RunValueFormat)
	if writing || bound {
		f, err = CheckRunValueFormat(ctx, id, raw)
	}
	if err != nil {
		return 0, nil, err
	}
	return f, r, nil
}

func marshalValueRecordIndent(ctx context.Context, f ValueFormat, kind ExactRecordKind, record any) ([]byte, error) {
	raw, err := EncodeValueRecordJSON(ctx, f, kind, record)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err = json.Indent(&out, raw, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), ctx.Err()
}

// Open under the same lock as the header. The descriptor pins that file even
// after deletion/replacement; visitors run without a lock and may call stores.
func (s *FilesystemRunStore) openValueEvents(ctx context.Context, id string) (*os.File, ValueFormat, error) {
	lock, err := s.acquireRunWriteLock(id)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = lock.Unlock() }()
	f, _, err := s.valueRecordFormatLocked(ctx, id, false)
	if err != nil {
		return nil, 0, err
	}
	path, err := s.openRunPayloadFile(ctx, id, "events.jsonl", false)
	if err != nil {
		return nil, f, err
	}
	defer path.Close()
	file, err := path.Open()
	if err != nil {
		return nil, f, err
	}
	if err = ctx.Err(); err != nil {
		_ = file.Close()
		return nil, f, fmt.Errorf("store: open events: %w", err)
	}
	return file, f, nil
}
