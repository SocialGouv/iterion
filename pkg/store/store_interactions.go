package store

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Interactions (human input/output)
// ---------------------------------------------------------------------------

// WriteInteraction persists a human interaction.
func (s *FilesystemRunStore) WriteInteraction(ctx context.Context, i *Interaction) error {
	if err := sanitizePathComponent("run ID", i.RunID); err != nil {
		return err
	}
	if err := sanitizePathComponent("interaction ID", i.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireRunWriteLock(i.RunID)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	f, _, err := s.valueRecordFormatLocked(ctx, i.RunID, true)
	if err != nil {
		return err
	}
	data, err := encodeInteractionValue(ctx, f, i)
	if err != nil {
		return err
	}
	dir, err := s.openRunPayloadDirectory(ctx, i.RunID, true, "interactions")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.WriteAtomic(ctx, i.ID+".json", data, filePerm)
}

func encodeInteractionValue(ctx context.Context, f ValueFormat, i *Interaction) ([]byte, error) {
	data, err := marshalValueRecordIndent(ctx, f, ExactInteraction, i)
	if err != nil {
		return nil, fmt.Errorf("store: marshal interaction: %w", err)
	}
	return data, nil
}

// LoadInteraction reads a specific interaction by ID.
func (s *FilesystemRunStore) LoadInteraction(ctx context.Context, runID, interactionID string) (*Interaction, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("interaction ID", interactionID); err != nil {
		return nil, err
	}
	lock, err := s.acquireRunWriteLock(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	f, _, err := s.valueRecordFormatLocked(ctx, runID, false)
	if err != nil {
		return nil, err
	}
	dir, err := s.openRunPayloadDirectory(ctx, runID, false, "interactions")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return loadInteractionFromDirectory(ctx, f, dir, interactionID)
}
func loadInteractionFromDirectory(ctx context.Context, f ValueFormat, dir *runPayloadDirectory, interactionID string) (*Interaction, error) {
	data, err := dir.ReadFile(interactionID + ".json")
	if err != nil {
		return nil, fmt.Errorf("store: load interaction: %w", err)
	}
	var i Interaction
	if err := DecodeValueRecordJSON(ctx, f, ExactInteraction, data, &i); err != nil {
		return nil, fmt.Errorf("store: decode interaction: %w", err)
	}
	return &i, nil
}

// AnswerInteractionCAS implements InteractionAnswerCAS: the load →
// check-unanswered → write sequence runs under an exclusive per-run
// stable run lock (flock on Unix, PID-lock on Windows), so of two
// concurrent answerers — REST racing CLI, a double-submit — exactly one
// wins and the other gets ErrInteractionAlreadyAnswered. The critical
// section is a few ms; contention waits (bounded) instead of erroring.
func (s *FilesystemRunStore) AnswerInteractionCAS(ctx context.Context, runID, interactionID string, answers map[string]any) (*Interaction, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("interaction ID", interactionID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := s.acquireRunWriteLock(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	f, _, err := s.valueRecordFormatLocked(ctx, runID, true)
	if err != nil {
		return nil, err
	}
	dir, err := s.openRunPayloadDirectory(ctx, runID, false, "interactions")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	in, err := loadInteractionFromDirectory(ctx, f, dir, interactionID)
	if err != nil {
		return nil, err
	}
	// Stored payload identity cannot redirect the answer to a different row.
	if in.RunID != runID || in.ID != interactionID {
		return nil, ErrRunDataIdentityChanged
	}
	if in.AnsweredAt != nil {
		return nil, fmt.Errorf("interaction %s/%s: %w", runID, interactionID, ErrInteractionAlreadyAnswered)
	}
	now := time.Now().UTC()
	in.Answers, in.AnsweredAt = answers, &now
	data, err := encodeInteractionValue(ctx, f, in)
	if err != nil {
		return nil, err
	}
	if err = dir.WriteAtomic(ctx, interactionID+".json", data, filePerm); err != nil {
		return nil, err
	}
	return in, nil
}

// ListInteractions returns all interaction IDs for a run.
//
// runID is sanitised before path-joining (see LoadRun for rationale).
func (s *FilesystemRunStore) ListInteractions(ctx context.Context, runID string) ([]string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	lock, err := s.acquireRunWriteLock(runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	if _, _, err := s.valueRecordFormatLocked(ctx, runID, false); err != nil {
		return nil, err
	}
	dir, err := s.openRunPayloadDirectory(ctx, runID, false, "interactions")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list interactions: %w", err)
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".json") {
			ids = append(ids, strings.TrimSuffix(name, ".json"))
		}
	}
	sort.Strings(ids)
	return ids, nil
}
