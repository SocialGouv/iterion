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
	// SkillRefs is the list of bundle skill names attached to this
	// queued message. Before the engine injects the message into the
	// agent's conversation, each referenced SKILL.md is mirrored into
	// the workspace's .claude/skills/ directory so the LLM can read
	// it on the next turn. Sticky: skills stay mirrored for the rest
	// of the run (the LLM may reference them on later turns); the
	// operator removes a skill via a follow-up message with a
	// different SkillRefs list. Empty for plain-text messages.
	SkillRefs []string `json:"skill_refs,omitempty" bson:"skill_refs,omitempty"`
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
func (s *FilesystemRunStore) AppendQueuedMessage(ctx context.Context, runID string, msg QueuedUserMessage) error {
	if err := NormalizeQueuedForAppend(&msg, runID); err != nil {
		return err
	}
	path, err := s.userMessagesPath(runID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("store: mkdir user_messages: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := appendJSONL(path, msg); err != nil {
		return err
	}
	s.bumpInboxVersion(runID)
	return nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	latest, err := loadLatestQueuedMessages(path)
	if err != nil {
		return err
	}
	cur, ok := latest[msgID]
	if !ok {
		return ErrQueuedMessageNotFound
	}
	if !statusMatches(cur.Status, expectedFrom) {
		return fmt.Errorf("%w: status=%s", ErrQueuedMessageStatusConflict, cur.Status)
	}
	StampQueuedTransition(&cur, status, time.Now().UTC())
	if err := appendJSONL(path, cur); err != nil {
		return err
	}
	s.bumpInboxVersion(runID)
	return nil
}

func statusMatches(cur QueuedMessageStatus, expectedFrom []QueuedMessageStatus) bool {
	if len(expectedFrom) == 0 {
		return true
	}
	for _, want := range expectedFrom {
		if cur == want {
			return true
		}
	}
	return false
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
// by the studio for initial inbox hydration alongside the snapshot.
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
	if err := f.Sync(); err != nil {
		return fmt.Errorf("store: fsync user_messages: %w", err)
	}
	return nil
}

// NormalizeQueuedForAppend mutates msg into the canonical shape for
// AppendQueuedMessage. Validates the required fields, stamps RunID +
// Status=queued, fills QueuedAt when zero, and clears all transition
// timestamps. Shared between FilesystemRunStore and the Mongo store
// so the two paths agree on what "a freshly-queued row" looks like.
func NormalizeQueuedForAppend(msg *QueuedUserMessage, runID string) error {
	if msg.ID == "" {
		return fmt.Errorf("store: queued message ID required")
	}
	if msg.Text == "" {
		return fmt.Errorf("store: queued message text required")
	}
	msg.RunID = runID
	msg.Status = QueuedMessageStatusQueued
	if msg.QueuedAt.IsZero() {
		msg.QueuedAt = time.Now().UTC()
	}
	msg.DeliveredAt = nil
	msg.ConsumedAt = nil
	msg.CancelledAt = nil
	return nil
}

// StampQueuedTransition flips msg.Status and the matching transition
// timestamp. Shared by both stores so the timestamp/field mapping
// stays in lockstep with the status enum.
func StampQueuedTransition(msg *QueuedUserMessage, status QueuedMessageStatus, now time.Time) {
	msg.Status = status
	switch status {
	case QueuedMessageStatusDelivered:
		msg.DeliveredAt = &now
	case QueuedMessageStatusConsumed:
		msg.ConsumedAt = &now
	case QueuedMessageStatusCancelled:
		msg.CancelledAt = &now
	}
}

// QueuedInboxVersioner is implemented by stores that can report a
// monotonically-increasing "something has changed" counter for the
// run's user-message inbox. Hot-path consumers (the agent loop's
// inbox drainer) read the version once per iteration and only re-
// load when it advanced — turning the 50-iteration tool-loop on a
// quiet run from 50 full-file scans into 50 cheap atomic reads.
//
// Stores that don't implement it force a load every iteration, which
// is correct but slower. Cloud (Mongo) stores rely on change-streams
// for liveness and need not implement this.
type QueuedInboxVersioner interface {
	QueuedInboxVersion(runID string) uint64
}

// InboxEventFor builds the canonical store.Event payload for one
// inbox status transition. Used by all three event-emission sites
// (the agent-loop drainer, the runtime pauseAtHuman drainer, the
// runview Service API handlers) so the wire shape stays in lockstep.
func InboxEventFor(typ EventType, runID string, msg QueuedUserMessage) Event {
	return Event{
		Type:  typ,
		RunID: runID,
		Data: map[string]interface{}{
			"id":           msg.ID,
			"text":         msg.Text,
			"status":       string(msg.Status),
			"queued_at":    msg.QueuedAt,
			"delivered_at": msg.DeliveredAt,
			"consumed_at":  msg.ConsumedAt,
			"cancelled_at": msg.CancelledAt,
		},
	}
}

// PublishInboxEvent appends an inbox status-change event to the run
// log and fans it out to any live subscriber via publish (the
// broker.Publish callback in local mode, nil in cloud mode where
// the change stream surfaces transitions).
func PublishInboxEvent(ctx context.Context, s RunStore, publish func(Event), typ EventType, runID string, msg QueuedUserMessage) {
	evt := InboxEventFor(typ, runID, msg)
	persisted, err := s.AppendEvent(ctx, runID, evt)
	if err != nil || persisted == nil {
		return
	}
	if publish != nil {
		publish(*persisted)
	}
}

// DrainPending moves every "queued" row in runID's inbox to
// "delivered", appending a transition row per message and emitting
// one user_message_delivered event per transition through publish.
// Returns the texts and IDs in FIFO order so a caller can both
// surface them to the LLM (texts) and later mark them consumed (ids).
// Errors loading the inbox are reported; per-message transition
// errors are skipped silently — a concurrent cancellation winning
// the race is acceptable, the row simply won't be delivered.
func DrainPending(ctx context.Context, s RunStore, publish func(Event), runID string) (texts []string, ids []string, err error) {
	msgs, _, err := DrainPendingMessages(ctx, s, publish, runID)
	if err != nil || len(msgs) == 0 {
		return nil, nil, err
	}
	texts = make([]string, 0, len(msgs))
	ids = make([]string, 0, len(msgs))
	for _, m := range msgs {
		texts = append(texts, m.Text)
		ids = append(ids, m.ID)
	}
	return texts, ids, nil
}

// DrainPendingMessages is the richer sibling of DrainPending that
// returns the full QueuedUserMessage records — including the
// SkillRefs slice — so the runtime can mirror attached skills before
// injecting the message text into the agent's conversation. The
// second return value is the (ordered) IDs in the same order as
// the first slice; callers that don't need it can ignore it.
//
// All transitions to "delivered" are atomic per-row; concurrent
// cancellation winning the race is silently skipped (the row simply
// won't be delivered). FIFO by QueuedAt.
func DrainPendingMessages(ctx context.Context, s RunStore, publish func(Event), runID string) ([]QueuedUserMessage, []string, error) {
	pending, err := s.LoadPendingQueuedMessages(ctx, runID)
	if err != nil || len(pending) == 0 {
		return nil, nil, err
	}
	msgs := make([]QueuedUserMessage, 0, len(pending))
	ids := make([]string, 0, len(pending))
	for _, m := range pending {
		if err := s.UpdateQueuedMessageStatus(ctx, runID, m.ID, QueuedMessageStatusDelivered, QueuedMessageStatusQueued); err != nil {
			continue
		}
		StampQueuedTransition(&m, QueuedMessageStatusDelivered, time.Now().UTC())
		msgs = append(msgs, m)
		ids = append(ids, m.ID)
		PublishInboxEvent(ctx, s, publish, EventUserMessageDelivered, runID, m)
	}
	return msgs, ids, nil
}

// MarkConsumed transitions every id in ids from "delivered" to
// "consumed" and emits a user_message_consumed event per transition.
// Errors are skipped silently; nothing actionable a caller could do
// with a partial failure.
func MarkConsumed(ctx context.Context, s RunStore, publish func(Event), runID string, ids []string) {
	now := time.Now().UTC()
	for _, id := range ids {
		if err := s.UpdateQueuedMessageStatus(ctx, runID, id, QueuedMessageStatusConsumed); err != nil {
			continue
		}
		msg := QueuedUserMessage{ID: id}
		StampQueuedTransition(&msg, QueuedMessageStatusConsumed, now)
		PublishInboxEvent(ctx, s, publish, EventUserMessageConsumed, runID, msg)
	}
}

// QueuedInboxVersion returns the run's inbox revision counter. The
// counter advances whenever a queued-message row is appended (the
// caller serialises writes under s.mu, so the increment is safe).
// Returning 0 for an unseen run is correct: any future write will
// increment past 0.
func (s *FilesystemRunStore) QueuedInboxVersion(runID string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inboxVersion[runID]
}

// bumpInboxVersion is called by the write paths while holding s.mu.
func (s *FilesystemRunStore) bumpInboxVersion(runID string) {
	s.inboxVersion[runID]++
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
