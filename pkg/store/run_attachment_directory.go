package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

func writeRecordDirectory(ctx context.Context, dir *runPayloadDirectory, data []byte) error {
	if !dir.parent.strict {
		return writeRecord(dir.parent.key, data, filePerm)
	}
	meta, err := dir.OpenDirectory(ctx, ".iterion", true)
	if err != nil {
		return err
	}
	defer meta.Close()
	return meta.WriteAtomic(ctx, "meta.json", data, filePerm)
}
func readRecordDirectory(ctx context.Context, dir *runPayloadDirectory) ([]byte, error) {
	if !dir.parent.strict {
		return readRecord(dir.parent.key)
	}
	meta, err := dir.OpenDirectory(ctx, ".iterion", false)
	if err != nil {
		return nil, err
	}
	defer meta.Close()
	data, err := meta.ReadFile("meta.json")
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return dir.ReadFile("meta.json")
}
func loadAttachmentMetaDirectory(ctx context.Context, dir *runPayloadDirectory) (AttachmentRecord, error) {
	data, err := readRecordDirectory(ctx, dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || (!dir.parent.strict && recordPathBlocked(dir.parent.key)) {
			return AttachmentRecord{}, ErrAttachmentNotFound
		}
		return AttachmentRecord{}, fmt.Errorf("store: read attachment meta: %w", err)
	}
	var rec AttachmentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return AttachmentRecord{}, fmt.Errorf("store: decode attachment meta: %w", err)
	}
	return rec, nil
}

// removeAll removes a payload entry through its retained parent; links are
// unlinked, never traversed. A retry synchronizes the parent even when absent.
func (p *runPayloadFile) removeAll(ctx context.Context) error {
	if ctx == nil {
		return ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.strict {
		return os.RemoveAll(p.key)
	}
	if p.root == nil {
		return nil
	}
	return errors.Join(p.root.RemoveAll(p.name), p.syncParent())
}
