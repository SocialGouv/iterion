package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Interactions (human input/output)
// ---------------------------------------------------------------------------

// WriteInteraction persists a human interaction.
func (s *FilesystemRunStore) WriteInteraction(_ context.Context, i *Interaction) error {
	if err := sanitizePathComponent("run ID", i.RunID); err != nil {
		return err
	}
	if err := sanitizePathComponent("interaction ID", i.ID); err != nil {
		return err
	}
	if err := s.guardNotDeleted(i.RunID); err != nil {
		return err
	}
	dir := filepath.Join(s.root, "runs", i.RunID, "interactions")
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir interaction: %w", err)
	}
	p := filepath.Join(dir, i.ID+".json")
	data, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal interaction: %w", err)
	}
	return writeFileAtomic(p, data, filePerm)
}

// LoadInteraction reads a specific interaction by ID.
func (s *FilesystemRunStore) LoadInteraction(_ context.Context, runID, interactionID string) (*Interaction, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("interaction ID", interactionID); err != nil {
		return nil, err
	}
	p := filepath.Join(s.root, "runs", runID, "interactions", interactionID+".json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("store: load interaction: %w", err)
	}
	var i Interaction
	if err := json.Unmarshal(data, &i); err != nil {
		return nil, fmt.Errorf("store: decode interaction: %w", err)
	}
	return &i, nil
}

// AnswerInteractionCAS implements InteractionAnswerCAS: the load →
// check-unanswered → write sequence runs under an exclusive per-run
// interaction lock (flock on Unix, PID-lock on Windows), so of two
// concurrent answerers — REST racing CLI, a double-submit — exactly one
// wins and the other gets ErrInteractionAlreadyAnswered. The critical
// section is a few ms; contention waits (bounded) instead of erroring.
func (s *FilesystemRunStore) AnswerInteractionCAS(ctx context.Context, runID, interactionID string, answers map[string]any) (*Interaction, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	lock, err := acquireFileLockRetry(
		filepath.Join(s.root, "runs", runID, "interactions", ".answer.lock"),
		fmt.Sprintf("interactions of run %s", runID),
		2*time.Second,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Unlock() }()
	return answerInteractionUnlocked(ctx, s, runID, interactionID, answers)
}

// ListInteractions returns all interaction IDs for a run.
//
// runID is sanitised before path-joining (see LoadRun for rationale).
func (s *FilesystemRunStore) ListInteractions(_ context.Context, runID string) ([]string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, "runs", runID, "interactions")
	entries, err := os.ReadDir(dir)
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
