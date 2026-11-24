package store

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// AppendEvent appends an event to the run's events.jsonl.
// Seq and Timestamp are set automatically.
// The entire operation is serialized under mu and the stable run write lock
// to prevent interleaved writes across branches, handles and processes. The sequence counter is only incremented after
// a successful write to avoid gaps in the event stream.
func (s *FilesystemRunStore) AppendEvent(ctx context.Context, runID string, evt Event) (*Event, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	evt.RunID = runID
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireRunWriteLock(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	format, parent, err := s.valueRecordFormatLocked(ctx, runID, true)
	if err != nil {
		return nil, err
	}
	original, err := ObserveEventParent(ctx, runID, format, parent)
	if err != nil {
		return nil, err
	}
	p, err := s.openRunPayloadFile(ctx, runID, "events.jsonl", true)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	infoOnDisk, statErr := p.Stat()
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, statErr
	}
	cached := s.seqFile[p.key]
	if cached == nil || infoOnDisk == nil || !os.SameFile(cached, infoOnDisk) || cached.Size() != infoOnDisk.Size() || !cached.ModTime().Equal(infoOnDisk.ModTime()) {
		delete(s.seqSeed, p.key)
	}
	if s.seqFormat[p.key] != format {
		delete(s.seqSeed, p.key)
		s.seqFormat[p.key] = format
	}

	// On first append, or when another handle changed this file, seed the in-memory
	// sequence counter from any existing events.jsonl. Without this, a fresh
	// process opening a pre-existing run (typical for `iterion resume`) would
	// restart Seq at 0 and produce duplicate sequence numbers in the
	// append-only event stream — breaking the documented monotonic ordering
	// and any downstream consumer that dedups by Seq.
	if !s.seqSeed[p.key] {
		// scanMaxSeqPathLocked returns the best-effort max+1 even on
		// partial scan failures (a scanner error past N readable lines
		// returns N+1 rather than 0), so we can trust `next` regardless
		// of err. Restarting at 0 on a partial scan would collide with
		// the readable-but-skipped tail and break the monotonic Seq
		// invariant downstream consumers rely on for dedup. The error
		// remains worth logging so an operator can investigate the
		// corruption — but we don't gate on it.
		next, err := s.scanMaxSeqPathLocked(p)
		s.seq[p.key] = next
		if err != nil && s.logger != nil {
			s.logger.Warn("store: partial seq seed for run %s: %v (resuming from %d — best-effort)", runID, err, next)
		}
		s.seqSeed[p.key] = true
	}

	// Assign seq but don't increment the counter yet — only advance on
	// successful write to prevent gaps from failed marshals or I/O.
	evt.Seq = s.seq[p.key]

	// Stamp the current log-buffer byte position when the runview
	// Service has wired a callback; lets the studio's time-travel
	// scrubber slice "log up to event seq N" without parsing log
	// line timestamps. Only overwrites when the caller didn't set
	// LogOffset explicitly (Mongo-mode replays / synthetic test
	// events can pre-fill).
	projected, err := ApplyEventProjection(ctx, original, &evt)
	if err != nil {
		return nil, err
	}
	if !projected && evt.LogOffset == 0 {
		s.logPositionMu.RLock()
		fn := s.logPositionFn
		s.logPositionMu.RUnlock()
		if fn != nil {
			evt.LogOffset = fn(runID)
		}
	}

	// Stamp the run's monotonic active duration (engine SharedBudget
	// elapsed) so the studio can display suspend-excluded active time
	// instead of re-deriving it from wall-clock event windows. Only
	// when the caller didn't pre-fill it (Mongo replays / test events).
	if !projected && evt.ActiveMs == 0 {
		s.logPositionMu.RLock()
		afn := s.activeDurationFn
		s.logPositionMu.RUnlock()
		if afn != nil {
			evt.ActiveMs = afn(runID)
		}
	}

	line, err := EncodeValueRecordJSON(ctx, format, ExactEvent, &evt)
	if err != nil {
		return nil, fmt.Errorf("store: marshal event: %w", err)
	}
	line = append(line, '\n')

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// O_RDWR (not O_WRONLY): the torn-tail repair below ReadAt's the final
	// byte to detect a partial last line left by a prior crash, and ReadAt on
	// a write-only descriptor returns EBADF. O_APPEND still forces every Write
	// to EOF regardless of the read offset, so append semantics are unchanged.
	f, err := p.OpenFile(os.O_CREATE|os.O_RDWR|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("store: open events: %w", err)
	}
	defer f.Close()
	// Tighten permissions on legacy files that may have been created before
	// store-wide filePerm was enforced. events.jsonl carries prompts, model
	// outputs, and tool data.
	if err := f.Chmod(filePerm); err != nil {
		return nil, fmt.Errorf("store: chmod events: %w", err)
	}

	// Capture the pre-write size so a short write (typically ENOSPC
	// mid-line) can be rolled back. Without this, a partial JSON line
	// is left in events.jsonl and the next append would silently
	// concatenate a valid line onto the truncated one — every
	// downstream scanner (LoadEvents, ScanEvents, replay) then trips
	// on a corrupted record forever after.
	info, statErr := f.Stat()
	var preSize int64
	if statErr == nil {
		preSize = info.Size()
	}
	if statErr == nil && preSize > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, preSize-1); err != nil {
			return nil, fmt.Errorf("store: inspect events tail: %w", err)
		}
		if last[0] != '\n' {
			// A previous process crashed after writing only part of the final
			// JSONL record. Separate the torn bytes from the next valid event so
			// scanners skip exactly the corrupt tail line instead of losing the
			// first post-crash event to concatenation. This runs under s.mu, so
			// sequence seeding and repair are atomic within this process.
			if _, err := f.Write([]byte("\n")); err != nil {
				return nil, fmt.Errorf("store: separate torn event tail: %w", err)
			}
			preSize++
		}
	}

	n, writeErr := f.Write(line)
	if writeErr != nil || n != len(line) {
		// Best-effort truncate to the captured pre-write size so the
		// file stays JSONL-clean. We only have a captured size when
		// Stat succeeded above; if it didn't, leaving the partial line
		// in place is the lesser evil (truncating to a guessed offset
		// could discard prior good lines).
		if statErr == nil {
			_ = f.Truncate(preSize)
		}
		if writeErr != nil {
			return nil, fmt.Errorf("store: write event: %w", writeErr)
		}
		return nil, fmt.Errorf("store: short write on event (wrote %d of %d bytes)", n, len(line))
	}

	// Best-effort fsync. The bytes are already in the file as far as
	// future appends are concerned (O_APPEND is atomic per syscall);
	// fsync only adds the durability guarantee. Treating a transient
	// fsync failure as fatal used to leave the in-memory seq counter
	// pinned at evt.Seq, so the next AppendEvent would assign the same
	// Seq to a different event and produce duplicate sequence numbers
	// in the file. Log instead and advance seq; the next run.json write always
	// re-syncs this events file first (writeRun's ordering barrier).
	// The checkpoint must never be durably ahead
	// of its event log.
	if err := f.Sync(); err != nil {
		if s.logger != nil {
			s.logger.Warn("store: fsync event for run %s seq %d: %v — line written but not durable", runID, evt.Seq, err)
		}
	}

	// Always advance once the bytes are in the file. fsync confirms
	// durability, not presence.
	s.seq[p.key] = evt.Seq + 1
	if info, err := f.Stat(); err == nil {
		s.seqFile[p.key] = info
	} else {
		delete(s.seqSeed, p.key)
		delete(s.seqFile, p.key)
	}

	// Private provenance is returned only after successful publication.
	evt, err = WithEventObservation(evt, original)
	if err != nil {
		return nil, err
	}
	return &evt, nil
}

// ScanEvents streams events for a run through visit, in file order, and
// stops as soon as visit returns false. It allocates one *Event per
// scanned line (decoded into a fresh struct) so the caller can retain
// references freely, but it never materialises the full events.jsonl
// slice — callers searching for a single match (e.g. node-touched
// filter) or paginating a window can short-circuit without paying the
// O(file) memory of LoadEvents.
//
// Errors decoding a single line are skipped (consistent with
// LoadEvents). The returned error reflects file-open / scanner-buffer
// failures, not per-line parse errors.
//
// runID is sanitised before path-joining (see LoadRun for rationale).
func (s *FilesystemRunStore) ScanEvents(ctx context.Context, runID string, visit func(*Event) bool) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	f, format, err := s.openValueEvents(ctx, runID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store: open events: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLineSize)
	var skipped, valid int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		evt := &Event{}
		if err := DecodeValueRecordJSON(ctx, format, ExactEvent, line, evt); err != nil {
			if format == ExactValueFormat || ctx.Err() != nil {
				return fmt.Errorf("store: decode exact event: %w", err)
			}
			skipped++
			continue
		}
		valid++
		if !visit(evt) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("store: scan events: %w", err)
	}
	if skipped > 0 && s.logger != nil {
		s.logger.Warn("skipped %d corrupt event line(s) in run %s (valid=%d)", skipped, runID, valid)
	}
	if eventsCorruptionExceeded(skipped, valid) {
		return fmt.Errorf("%w: run %s, skipped=%d valid=%d", ErrEventsCorrupted, runID, skipped, valid)
	}
	return nil
}

// LoadEventsRange streams events with seq in [from, to) (to == 0 means
// "no upper bound") and caps the returned slice at limit (limit == 0
// means "no cap"). Designed for paginating long events.jsonl tails
// without allocating the whole file: a 200MB events.jsonl with limit=
// 5000 returns at most 5000 entries instead of materialising every
// event in memory just to slice the head.
//
// The caller can detect "more available" by passing limit and checking
// whether len(out) == limit; the next page starts at out[len(out)-1].Seq+1.
func (s *FilesystemRunStore) LoadEventsRange(ctx context.Context, runID string, from, to int64, limit int) ([]*Event, error) {
	var out []*Event
	if limit > 0 {
		out = make([]*Event, 0, limit)
	}
	err := s.ScanEvents(ctx, runID, func(e *Event) bool {
		if e.Seq < from {
			return true
		}
		if to > 0 && e.Seq >= to {
			return false // events.jsonl is monotonic in Seq → safe to stop
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LoadEvents reads all events for a run in sequence order.
//
// runID is sanitised before path-joining (see LoadRun for rationale).
//
// Delegates to ScanEvents: identical open/scan/decode/skip-corrupt semantics
// (corruption threshold, partial-result return) are preserved because
// ScanEvents populates events via the visit callback before returning the
// ErrEventsCorrupted error.
func (s *FilesystemRunStore) LoadEvents(ctx context.Context, runID string) ([]*Event, error) {
	var events []*Event
	err := s.ScanEvents(ctx, runID, func(e *Event) bool {
		events = append(events, e)
		return true
	})
	return events, err
}

// scanMaxSeqPathLocked reads the selected events file and returns max(Seq)+1, the
// value that should be assigned to the next appended event. Returns 0 (with
// nil error) if the file does not exist (fresh run) or contains no decodable
// lines. Caller must hold s.mu.
//
// This intentionally does NOT use LoadEvents (which allocates the full slice
// of events) — we only need the max Seq, so we scan and discard.
func (s *FilesystemRunStore) scanMaxSeqPathLocked(p *runPayloadFile) (int64, error) {
	f, err := p.Open()
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	var maxSeq int64 = -1
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Decode only the seq field — minimal struct keeps allocations low.
		var hdr struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &hdr); err != nil {
			// Skip corrupt lines rather than aborting (consistent with
			// LoadEvents' tolerant behaviour).
			continue
		}
		if hdr.Seq > maxSeq {
			maxSeq = hdr.Seq
		}
	}
	scanErr := scanner.Err()
	// Always return the best-effort max+1: when scanner.Err is non-nil
	// (oversized line, read failure mid-file) the readable prefix's
	// max is still trustworthy. Restarting from 0 on a partial scan
	// would collide with prior events and break the monotonic Seq
	// invariant. Caller logs scanErr; this function never withholds
	// the count it managed to compute.
	next := int64(0)
	if maxSeq >= 0 {
		next = maxSeq + 1
	}
	return next, scanErr
}

// syncEventsPathLocked fsyncs the selected events file before every checkpoint,
// including after another handle wrote it. Called under the stable run lock.
// A missing events file counts as
// synced (nothing to persist — the failed append may have been the
// file's first line on a path that never materialised).
func (s *FilesystemRunStore) syncEventsPathLocked(path *runPayloadFile) error {
	f, err := path.OpenFile(os.O_RDWR, filePerm)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return err
	}
	return path.syncParent()
}
