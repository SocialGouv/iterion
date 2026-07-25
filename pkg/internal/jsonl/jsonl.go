// Package jsonl provides a crash-safe append-only JSONL file writer.
//
// It reuses the torn-tail-repair discipline of the run store's
// events.jsonl writer (pkg/store): before each append the last byte of
// the file is inspected, and a missing trailing newline — left by a
// process that crashed mid-write — is repaired so the torn bytes stay
// confined to their own line; a short write is rolled back by
// truncating to the pre-write size.
//
// Cross-process exclusion is provided by an advisory flock held for the
// duration of each append, so concurrent writers (e.g. two cron ticks
// firing at the same minute) never interleave bytes.
package jsonl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	dirPerm  = 0o755
	filePerm = 0o600
)

// AppendJSON marshals v and appends it as one line to path, creating
// parent directories and the file as needed. Safe across processes
// (flock) and across crashes (torn-tail repair + short-write rollback).
func AppendJSON(path string, v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("jsonl: marshal: %w", err)
	}
	line = append(line, '\n')

	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("jsonl: mkdir %s: %w", filepath.Dir(path), err)
	}

	// O_RDWR (not O_WRONLY): the torn-tail repair ReadAt's the final byte,
	// and ReadAt on a write-only descriptor returns EBADF. O_APPEND still
	// forces every write to EOF, so append semantics are unchanged.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("jsonl: open %s: %w", path, err)
	}
	defer f.Close()

	if err := lockFile(f); err != nil {
		return fmt.Errorf("jsonl: lock %s: %w", path, err)
	}
	defer unlockFile(f)

	info, statErr := f.Stat()
	var preSize int64
	if statErr == nil {
		preSize = info.Size()
	}
	if statErr == nil && preSize > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, preSize-1); err != nil {
			return fmt.Errorf("jsonl: inspect tail of %s: %w", path, err)
		}
		if last[0] != '\n' {
			// A previous process crashed after writing part of the final
			// record. Terminate the torn line so scanners skip exactly the
			// corrupt tail instead of losing the next record to
			// concatenation.
			if _, err := f.Write([]byte("\n")); err != nil {
				return fmt.Errorf("jsonl: separate torn tail of %s: %w", path, err)
			}
			preSize++
		}
	}

	n, writeErr := f.Write(line)
	if writeErr != nil || n != len(line) {
		// Roll back a partial line (typically ENOSPC mid-write) so the
		// file stays JSONL-clean. Only when Stat succeeded — truncating
		// to a guessed offset could discard prior good lines.
		if statErr == nil {
			_ = f.Truncate(preSize)
		}
		if writeErr != nil {
			return fmt.Errorf("jsonl: write %s: %w", path, writeErr)
		}
		return fmt.Errorf("jsonl: short write on %s (wrote %d of %d bytes)", path, n, len(line))
	}
	return nil
}

// ReadLines decodes every well-formed line of path into T, skipping
// blank and corrupt lines (a torn tail from a crash is expected, not an
// error). A missing file yields an empty slice.
func ReadLines[T any](path string) ([]T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("jsonl: read %s: %w", path, err)
	}
	var out []T
	start := 0
	for i := 0; i <= len(data); i++ {
		if i != len(data) && data[i] != '\n' {
			continue
		}
		lineBytes := data[start:i]
		start = i + 1
		if len(lineBytes) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(lineBytes, &v); err != nil {
			continue // torn or foreign line — skip, by design
		}
		out = append(out, v)
	}
	return out, nil
}
