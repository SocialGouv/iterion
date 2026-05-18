package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// QueuedMessageStatus is the lifecycle state of a queued operator
// message. Persisted in the per-run user_messages.jsonl as a
// status-transition log; the live status of a message is the last
// status seen for its ID during reload.
type QueuedMessageStatus string

const (
	QueuedMessageStatusQueued    QueuedMessageStatus = "queued"
	QueuedMessageStatusDelivered QueuedMessageStatus = "delivered"
	QueuedMessageStatusConsumed  QueuedMessageStatus = "consumed"
	QueuedMessageStatusCancelled QueuedMessageStatus = "cancelled"
)

// QueuedUserMessage is one operator chat message queued against a
// running agent. The runtime drains pending messages between agent-
// loop iterations (claw) or at the next human pause (claude_code /
// codex). FIFO by QueuedAt.
type QueuedUserMessage struct {
	ID          string              `json:"id" bson:"id"`
	RunID       string              `json:"run_id" bson:"run_id"`
	Text        string              `json:"text" bson:"text"`
	QueuedAt    time.Time           `json:"queued_at" bson:"queued_at"`
	DeliveredAt *time.Time          `json:"delivered_at,omitempty" bson:"delivered_at,omitempty"`
	ConsumedAt  *time.Time          `json:"consumed_at,omitempty" bson:"consumed_at,omitempty"`
	CancelledAt *time.Time          `json:"cancelled_at,omitempty" bson:"cancelled_at,omitempty"`
	Status      QueuedMessageStatus `json:"status" bson:"status"`
	// TenantID mirrors Run.TenantID for cross-tenant access checks.
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
}

// ErrQueuedMessageNotFound is returned by UpdateQueuedMessageStatus
// when the target message ID does not exist for the given run.
var ErrQueuedMessageNotFound = errors.New("store: queued message not found")

// ErrQueuedMessageStatusConflict is returned by
// UpdateQueuedMessageStatus when the message's current status is not
// in the caller's expectedFrom whitelist (e.g. cancelling an already-
// delivered message).
var ErrQueuedMessageStatusConflict = errors.New("store: queued message status conflict")

// ---------------------------------------------------------------------------
// FilesystemRunStore impl
// ---------------------------------------------------------------------------

func (s *FilesystemRunStore) userMessagesPath(runID string) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "runs", runID, "user_messages.jsonl"), nil
}

// AppendQueuedMessage adds a new queued message in "queued" status.
// The ID, RunID, Text, and QueuedAt fields must be set by the caller.
// Status is forced to "queued"; all transition timestamps are nil.
// Concurrent appends are protected by a per-run flock via LockRun for
// cross-process safety.
func (s *FilesystemRunStore) AppendQueuedMessage(ctx context.Context, runID string, msg QueuedUserMessage) error {
	if msg.ID == "" {
		return fmt.Errorf("store: queued message ID required")
	}
	if msg.Text == "" {
		return fmt.Errorf("store: queued message text required")
	}
	path, err := s.userMessagesPath(runID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("store: mkdir user_messages: %w", err)
	}
	msg.RunID = runID
	msg.Status = QueuedMessageStatusQueued
	if msg.QueuedAt.IsZero() {
		msg.QueuedAt = time.Now().UTC()
	}
	msg.DeliveredAt = nil
	msg.ConsumedAt = nil
	msg.CancelledAt = nil
	return appendJSONL(path, msg)
}

// UpdateQueuedMessageStatus transitions the latest record for msgID
// to the new status. Writes a new line (the JSONL is a transition
// log; the latest line per ID wins). Returns
// ErrQueuedMessageNotFound when the ID does not exist, and
// ErrQueuedMessageStatusConflict when the current status is not one
// of expectedFrom (when expectedFrom is non-empty).
func (s *FilesystemRunStore) UpdateQueuedMessageStatus(ctx context.Context, runID, msgID string, status QueuedMessageStatus, expectedFrom ...QueuedMessageStatus) error {
	if msgID == "" {
		return fmt.Errorf("store: queued message ID required")
	}
	path, err := s.userMessagesPath(runID)
	if err != nil {
		return err
	}
	latest, err := loadLatestQueuedMessages(path)
	if err != nil {
		return err
	}
	cur, ok := latest[msgID]
	if !ok {
		return ErrQueuedMessageNotFound
	}
	if len(expectedFrom) > 0 {
		match := false
		for _, want := range expectedFrom {
			if cur.Status == want {
				match = true
				break
			}
		}
		if !match {
			return fmt.Errorf("%w: status=%s", ErrQueuedMessageStatusConflict, cur.Status)
		}
	}
	now := time.Now().UTC()
	cur.Status = status
	switch status {
	case QueuedMessageStatusDelivered:
		cur.DeliveredAt = &now
	case QueuedMessageStatusConsumed:
		cur.ConsumedAt = &now
	case QueuedMessageStatusCancelled:
		cur.CancelledAt = &now
	}
	return appendJSONL(path, cur)
}

// LoadPendingQueuedMessages returns the messages whose current status
// is "queued", ordered FIFO by QueuedAt. Returns an empty slice (no
// error) when the file does not exist (no messages have ever been
// queued for this run).
func (s *FilesystemRunStore) LoadPendingQueuedMessages(ctx context.Context, runID string) ([]QueuedUserMessage, error) {
	path, err := s.userMessagesPath(runID)
	if err != nil {
		return nil, err
	}
	latest, err := loadLatestQueuedMessages(path)
	if err != nil {
		return nil, err
	}
	out := make([]QueuedUserMessage, 0, len(latest))
	for _, m := range latest {
		if m.Status == QueuedMessageStatusQueued {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].QueuedAt.Before(out[j].QueuedAt)
	})
	return out, nil
}

// ListQueuedMessages returns every message recorded for the run, in
// FIFO order by QueuedAt, with their CURRENT (latest) status. Used
// by the editor for initial inbox hydration alongside the snapshot.
func (s *FilesystemRunStore) ListQueuedMessages(ctx context.Context, runID string) ([]QueuedUserMessage, error) {
	path, err := s.userMessagesPath(runID)
	if err != nil {
		return nil, err
	}
	latest, err := loadLatestQueuedMessages(path)
	if err != nil {
		return nil, err
	}
	out := make([]QueuedUserMessage, 0, len(latest))
	for _, m := range latest {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].QueuedAt.Before(out[j].QueuedAt)
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// JSONL helpers
// ---------------------------------------------------------------------------

func appendJSONL(path string, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: marshal user_message: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("store: open user_messages: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("store: write user_message: %w", err)
	}
	return nil
}

// loadLatestQueuedMessages folds the JSONL log into a "latest status
// per ID" map. Order within the file is the authoritative tie-breaker
// for same-ID rows.
func loadLatestQueuedMessages(path string) (map[string]QueuedUserMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]QueuedUserMessage{}, nil
		}
		return nil, fmt.Errorf("store: open user_messages: %w", err)
	}
	defer f.Close()
	out := map[string]QueuedUserMessage{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var m QueuedUserMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("store: decode user_message: %w", err)
		}
		out[m.ID] = m
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("store: scan user_messages: %w", err)
	}
	return out, nil
}
