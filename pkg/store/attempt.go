package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// RunAttempt is the small, append-only index of an execution's provenance.
// ID is the digest of the immutable snapshot, not the mutable workflow hash.
type RunAttempt struct {
	ID           string     `json:"id" bson:"id"`
	StartedAt    time.Time  `json:"started_at" bson:"started_at"`
	WorkflowHash string     `json:"workflow_hash,omitempty" bson:"workflow_hash,omitempty"`
	Budget       *RunBudget `json:"budget,omitempty" bson:"budget,omitempty"`
}

// AttemptSnapshot is authoritative even when Source is absent (a programmatic
// IR has no authoring source). Absence never licenses reading a later file.
type AttemptSnapshot struct {
	StartedAt    time.Time              `json:"started_at"`
	WorkflowHash string                 `json:"workflow_hash,omitempty"`
	Source       *workflowfile.Snapshot `json:"source,omitempty"`
	Budget       *RunBudget             `json:"budget,omitempty"`
}

// AttemptStore persists content-addressed snapshots before indexing them on
// the run. Filesystem and Mongo/S3 implement the same contract. There is no
// replace operation: a different body always has a different ID.
var ErrAttemptNotFound = errors.New("attempt snapshot not found")

type AttemptStore interface {
	RecordAttempt(context.Context, string, *AttemptSnapshot) (RunAttempt, error)
	LoadAttempt(context.Context, string, string) (*AttemptSnapshot, error)
}

func AsAttemptStore(s RunStore) AttemptStore { return capability[AttemptStore](s) }

func EncodeAttempt(a *AttemptSnapshot) (RunAttempt, []byte, error) {
	body, err := json.Marshal(a)
	if err != nil {
		return RunAttempt{}, nil, err
	}
	digest := sha256.Sum256(body)
	return RunAttempt{ID: hex.EncodeToString(digest[:]), StartedAt: a.StartedAt, WorkflowHash: a.WorkflowHash, Budget: a.Budget}, body, nil
}

func DecodeAttempt(id string, body []byte) (*AttemptSnapshot, error) {
	digest := sha256.Sum256(body)
	if id != hex.EncodeToString(digest[:]) {
		return nil, fmt.Errorf("attempt snapshot %s: digest mismatch", id)
	}
	var a AttemptSnapshot
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func ValidAttemptID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == sha256.Size
}

func (s *FilesystemRunStore) RecordAttempt(_ context.Context, runID string, a *AttemptSnapshot) (RunAttempt, error) {
	meta, body, err := EncodeAttempt(a)
	if err != nil {
		return meta, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.loadRunRaw(runID)
	if err != nil {
		return meta, err
	}
	dir := filepath.Join(s.root, "runs", runID, "attempts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return meta, err
	}
	// A partial write is never indexed. Retrying the same content is safe.
	if err := WriteFileAtomic(filepath.Join(dir, meta.ID+".json"), body, 0o600); err != nil {
		return meta, err
	}
	for _, previous := range r.Attempts {
		if previous.ID == meta.ID {
			return meta, nil
		}
	}
	r.Attempts = append(r.Attempts, meta)
	r.UpdatedAt = time.Now().UTC()
	return meta, s.writeRun(r)
}

func (s *FilesystemRunStore) LoadAttempt(ctx context.Context, runID, id string) (*AttemptSnapshot, error) {
	if !ValidAttemptID(id) {
		return nil, fmt.Errorf("invalid attempt id")
	}
	if _, err := s.LoadRun(ctx, runID); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(s.root, "runs", runID, "attempts", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAttemptNotFound
	}
	if err != nil {
		return nil, err
	}
	return DecodeAttempt(id, body)
}
