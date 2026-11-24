package store

import (
	"context"
	"fmt"
	"io"
)

// ---------------------------------------------------------------------------
// Tool blobs (per-tool-call I/O bodies, sidecar to events.jsonl)
// ---------------------------------------------------------------------------

// validateToolBlobKind rejects anything other than "input" or "output"
// so the path component is always a known literal — no traversal risk
// from a network-sourced kind value.
func validateToolBlobKind(kind string) error {
	if kind != "input" && kind != "output" {
		return fmt.Errorf("store: tool blob kind must be input|output, got %q", kind)
	}
	return nil
}

// WriteToolBlob satisfies ToolBlobStore. Writes atomically; returns the
// total byte size persisted. Idempotent — re-writing the same key
// replaces the prior bytes.
func (s *FilesystemRunStore) WriteToolBlob(ctx context.Context, runID, toolUseID, kind string, body []byte) (int64, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return 0, err
	}
	if err := sanitizePathComponent("tool_use_id", toolUseID); err != nil {
		return 0, err
	}
	if err := validateToolBlobKind(kind); err != nil {
		return 0, err
	}
	_, unlock, err := s.lockRunMetadataMutation(ctx, runID)
	if err != nil {
		return 0, err
	}
	defer unlock()
	p, err := s.openRunPayloadPath(ctx, runID, true, "tools", toolUseID, kind)
	if err != nil {
		return 0, err
	}
	defer p.Close()
	if err := p.WriteAtomic(ctx, body, filePerm); err != nil {
		return 0, fmt.Errorf("store: write tool blob: %w", err)
	}
	return int64(len(body)), nil
}

// ReadToolBlob satisfies ToolBlobStore. limit == 0 means "all from
// offset". Returns the bytes read, the full blob size, and eof when
// offset+len(data) == total. Missing blob → wrapped os.ErrNotExist.
func (s *FilesystemRunStore) ReadToolBlob(ctx context.Context, runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, 0, false, err
	}
	if err := sanitizePathComponent("tool_use_id", toolUseID); err != nil {
		return nil, 0, false, err
	}
	if err := validateToolBlobKind(kind); err != nil {
		return nil, 0, false, err
	}
	if offset < 0 {
		offset = 0
	}
	p, err := s.openRunPayloadPath(ctx, runID, false, "tools", toolUseID, kind)
	if err != nil {
		return nil, 0, false, err
	}
	defer p.Close()
	f, err := p.Open()
	if err != nil {
		return nil, 0, false, fmt.Errorf("store: open tool blob: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, false, fmt.Errorf("store: stat tool blob: %w", err)
	}
	total := st.Size()
	if offset >= total {
		return nil, total, true, nil
	}
	remaining := total - offset
	readLen := remaining
	if limit > 0 && limit < readLen {
		readLen = limit
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, total, false, fmt.Errorf("store: seek tool blob: %w", err)
	}
	buf := make([]byte, readLen)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, total, false, fmt.Errorf("store: read tool blob: %w", err)
	}
	eof := offset+readLen >= total
	return buf, total, eof, nil
}
