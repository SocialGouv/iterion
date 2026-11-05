package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RunNote is one freeform operator note attached to a run — the durable
// "flaky, re-ran" / "root cause was X" / "do not merge until Y"
// annotations a team leaves on a run so the next person reading it has
// the context. Notes are per-run, chronological, and immutable once
// created (no edit/delete in this first cut). Seq is a per-run monotonic
// index assigned at append time; it gives a stable chronological order
// even when two notes share a wall-clock timestamp.
type RunNote struct {
	Seq       int       `json:"seq"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	Timestamp time.Time `json:"ts"`
}

// RunNoteStore is an optional interface implemented by stores that
// persist a run's operator notes (see RunNote). The filesystem store
// backs it with runs/<id>/notes/<NNNN>.json (zero-padded sequence); the
// Mongo (cloud) store backs it with the run_notes collection (one doc
// per note, keyed run_id + seq). It mirrors the PlanStore seam so the
// note surface is backend-agnostic — the same HTTP handlers serve local
// and cloud runs. Callers MUST nil-check via AsRunNoteStore.
type RunNoteStore interface {
	// AppendRunNote persists note as the next chronological note for the
	// run, assigning note.Seq itself (callers leave it zero) and
	// stamping note.Timestamp with the current time when zero. Returns
	// the persisted note with Seq + Timestamp populated.
	AppendRunNote(ctx context.Context, runID string, note RunNote) (RunNote, error)
	// ListRunNotes returns every persisted note for the run in
	// chronological (ascending Seq) order. A run with no notes yields
	// (nil, nil).
	ListRunNotes(ctx context.Context, runID string) ([]RunNote, error)
	// ReattributeRunNotes rewrites the Author of every note, in every run
	// and tenant whatever the context, whose author is fromAuthor (case
	// and surrounding blanks ignored) to toAuthor, and answers how many it
	// rewrote. An account's erasure calls it with the address a note
	// defaults its author to and the account's id, so the note keeps a
	// pseudonym and loses the address. An empty fromAuthor is refused.
	ReattributeRunNotes(ctx context.Context, fromAuthor, toAuthor string) (int64, error)
}

// NoteAuthorMatches reports whether a note's author is from, read the way
// ReattributeRunNotes reads it: case and surrounding blanks ignored.
func NoteAuthorMatches(author, from string) bool {
	return strings.EqualFold(strings.TrimSpace(author), strings.TrimSpace(from))
}

// AsRunNoteStore returns s as RunNoteStore when the backend persists run
// notes, or nil otherwise. Both the filesystem store and the Mongo
// (cloud) store satisfy it — the seam mirrors PlanStore / RunGitMetaStore,
// so the HTTP surface is backend-agnostic. Callers MUST nil-check (a
// store without the seam, or a nil store).
func AsRunNoteStore(s RunStore) RunNoteStore {
	return capability[RunNoteStore](s)
}

// notesDir returns <root>/runs/<runID>/notes after validating runID.
func (s *FilesystemRunStore) notesDir(runID string) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "runs", runID, "notes"), nil
}

// AppendRunNote implements RunNoteStore over runs/<id>/notes/<NNNN>.json.
// Holds s.mu across the read-max-seq + write so concurrent note appends
// can't collide on a sequence number.
func (s *FilesystemRunStore) AppendRunNote(_ context.Context, runID string, note RunNote) (RunNote, error) {
	dir, err := s.notesDir(runID)
	if err != nil {
		return RunNote{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	seqs, err := noteSeqs(dir)
	if err != nil {
		return RunNote{}, err
	}
	nextSeq := 0
	if len(seqs) > 0 {
		nextSeq = seqs[len(seqs)-1] + 1
	}

	note.Seq = nextSeq
	if note.Timestamp.IsZero() {
		note.Timestamp = time.Now().UTC()
	}
	data, err := json.MarshalIndent(note, "", "  ")
	if err != nil {
		return RunNote{}, fmt.Errorf("store: marshal run note: %w", err)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return RunNote{}, fmt.Errorf("store: mkdir notes: %w", err)
	}
	p := filepath.Join(dir, fmt.Sprintf("%04d.json", nextSeq))
	if err := writeFileAtomic(p, data, filePerm); err != nil {
		return RunNote{}, err
	}
	return note, nil
}

// ListRunNotes implements RunNoteStore: every note under
// runs/<id>/notes in ascending Seq (chronological) order.
func (s *FilesystemRunStore) ListRunNotes(_ context.Context, runID string) ([]RunNote, error) {
	dir, err := s.notesDir(runID)
	if err != nil {
		return nil, err
	}
	seqs, err := noteSeqs(dir)
	if err != nil {
		return nil, err
	}
	if len(seqs) == 0 {
		return nil, nil
	}
	out := make([]RunNote, 0, len(seqs))
	for _, seq := range seqs {
		note, rerr := readNoteFile(dir, seq)
		if rerr != nil {
			// Skip an unreadable/corrupt note rather than failing the whole
			// listing — a partial note history is more useful than none,
			// matching the store's skip-corrupt event/plan semantics.
			continue
		}
		out = append(out, note)
	}
	return out, nil
}

// ReattributeRunNotes implements RunNoteStore over every runs/<id>/notes
// directory, rewriting each matching note file atomically under s.mu. A
// note file it cannot read is left as is and named in the error, so the
// erasure that calls it is answered as incomplete rather than reported done.
func (s *FilesystemRunStore) ReattributeRunNotes(_ context.Context, fromAuthor, toAuthor string) (int64, error) {
	if strings.TrimSpace(fromAuthor) == "" {
		return 0, fmt.Errorf("store: reattribute run notes: no author to match")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	runs, err := os.ReadDir(filepath.Join(s.root, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("store: reattribute run notes: read runs dir: %w", err)
	}
	var n int64
	var errs []error
	for _, run := range runs {
		if !run.IsDir() {
			continue
		}
		dir := filepath.Join(s.root, "runs", run.Name(), "notes")
		seqs, err := noteSeqs(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, seq := range seqs {
			note, err := readNoteFile(dir, seq)
			if err != nil {
				errs = append(errs, fmt.Errorf("run %s note %d: %w", run.Name(), seq, err))
				continue
			}
			if !NoteAuthorMatches(note.Author, fromAuthor) {
				continue
			}
			note.Author = toAuthor
			data, err := json.MarshalIndent(note, "", "  ")
			if err != nil {
				return n, fmt.Errorf("store: marshal run note: %w", err)
			}
			if err := writeFileAtomic(filepath.Join(dir, fmt.Sprintf("%04d.json", seq)), data, filePerm); err != nil {
				errs = append(errs, fmt.Errorf("run %s note %d: %w", run.Name(), seq, err))
				continue
			}
			n++
		}
	}
	if len(errs) > 0 {
		return n, fmt.Errorf("store: reattribute run notes: %w", errors.Join(errs...))
	}
	return n, nil
}

// noteSeqs returns the sorted sequence numbers of the <NNNN>.json note
// files in dir. A missing dir is (nil, nil).
func noteSeqs(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read notes dir: %w", err)
	}
	seqs := make([]int, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		n, perr := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if perr != nil {
			continue
		}
		seqs = append(seqs, n)
	}
	sort.Ints(seqs)
	return seqs, nil
}

// readNoteFile reads and decodes one run note by sequence number.
func readNoteFile(dir string, seq int) (RunNote, error) {
	p := filepath.Join(dir, fmt.Sprintf("%04d.json", seq))
	data, err := os.ReadFile(p)
	if err != nil {
		return RunNote{}, fmt.Errorf("store: read run note: %w", err)
	}
	var note RunNote
	if err := json.Unmarshal(data, &note); err != nil {
		return RunNote{}, fmt.Errorf("store: decode run note: %w", err)
	}
	return note, nil
}
