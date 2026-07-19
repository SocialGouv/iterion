package native

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// ScanEvents streams events from events.jsonl through visit, in file
// order. Returning false from visit stops the scan. Safe to call
// concurrently with writes — the file is append-only.
func (s *Store) ScanEvents(visit func(*Event) bool) error {
	p := filepath.Join(s.root, eventsFile)
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<16), 10*1024*1024)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if !visit(&e) {
			return nil
		}
	}
	return scanner.Err()
}

// ---------------------------------------------------------------------------
// internals — must be called with s.mu held
// ---------------------------------------------------------------------------

// writeEventLineLocked formats an event and appends a single line to
// events.jsonl with fsync. Increments s.seq on success.
func (s *Store) writeEventLineLocked(evt Event) error {
	evt.Seq = s.seq
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	line, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("native store: marshal event: %w", err)
	}
	line = append(line, '\n')
	p := filepath.Join(s.root, eventsFile)
	// O_RDWR (not O_WRONLY) so the torn-tail repair below can ReadAt the
	// final byte (ReadAt on a write-only fd returns EBADF). O_APPEND
	// still forces every write to EOF, so append semantics are unchanged.
	// Mirrors the runs-store hardening (a79ffa76).
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("native store: open events: %w", err)
	}
	defer f.Close()

	// Repair a torn final line left by a prior crash (a partial JSONL
	// record with no trailing newline). Without this the next append
	// concatenates onto the torn bytes, merging two records into one
	// corrupt line — so a tailer skips it and loses BOTH the torn tail
	// AND this event. Runs under s.mu, so seq seeding + repair are atomic
	// within this process.
	info, statErr := f.Stat()
	var preSize int64
	if statErr == nil {
		preSize = info.Size()
	}
	if statErr == nil && preSize > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, preSize-1); err != nil {
			return fmt.Errorf("native store: inspect events tail: %w", err)
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte("\n")); err != nil {
				return fmt.Errorf("native store: separate torn event tail: %w", err)
			}
			preSize++
		}
	}

	n, writeErr := f.Write(line)
	if writeErr != nil || n != len(line) {
		// Roll back a short write (typically ENOSPC mid-line) to the
		// captured size so the file stays JSONL-clean. Only safe when
		// Stat succeeded; otherwise leaving the partial line is the
		// lesser evil (a guessed offset could drop prior good lines).
		if statErr == nil {
			_ = f.Truncate(preSize)
		}
		if writeErr != nil {
			return fmt.Errorf("native store: write event: %w", writeErr)
		}
		return fmt.Errorf("native store: short write on event (wrote %d of %d bytes)", n, len(line))
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("native store: sync event: %w", err)
	}
	s.seq++
	return nil
}

// appendEventLocked drains any previously-buffered events whose append
// failed before writing the new one. A transient fsync hiccup that
// previously left a gap in events.jsonl now self-heals on the next
// successful operation — external tailers see every transition in
// the correct seq order, just delayed.
func (s *Store) appendEventLocked(evt Event) error {
	if len(s.pendingEvents) > 0 {
		drained := s.pendingEvents
		s.pendingEvents = nil
		for i, p := range drained {
			if err := s.writeEventLineLocked(p); err != nil {
				// Still flaky — re-buffer the failed entry, every
				// entry after it, and the new one. The caller can
				// retry; state on disk is consistent because the
				// issue file was already updated by the mutator.
				s.pendingEvents = append(s.pendingEvents, drained[i:]...)
				s.pendingEvents = append(s.pendingEvents, evt)
				return err
			}
		}
	}
	if err := s.writeEventLineLocked(evt); err != nil {
		s.pendingEvents = append(s.pendingEvents, evt)
		return err
	}
	return nil
}

// emitPostCommitEvent appends an event after a successful issue write.
// The issue file is the authoritative source for state recovery
// (populateIndex reads them at startup, not events.jsonl), so an event
// write failure here doesn't corrupt state. The buffered-replay path
// in appendEventLocked ensures external tailers still see every
// transition once the filesystem cooperates again.
func (s *Store) emitPostCommitEvent(evt Event) error {
	if err := s.appendEventLocked(evt); err != nil {
		// The issue file + in-memory index are already updated (the
		// authoritative state) and appendEventLocked buffered the event in
		// pendingEvents for replay. events.jsonl is non-authoritative, so we
		// must NOT propagate this as a mutation failure: a caller that maps it
		// to a 4xx/5xx for a write that actually succeeded would, on retry,
		// create a duplicate issue (Create generates a fresh UUID) or re-emit
		// the mutation. Warn and swallow. (Always returns nil; the error
		// return is kept only for the call-site signatures.)
		//
		// s.logger read directly, not via getLogger(): every caller holds
		// s.mu (post-commit internals), and getLogger would re-lock it.
		s.logger.Warn("native store: event log fsync failed; buffered for replay on next operation: %v", err)
	}
	return nil
}
