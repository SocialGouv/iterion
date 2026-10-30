package store

import (
	"bytes"
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

// ErrTurnNotFound is returned by TurnStore implementations when the
// requested turn doesn't exist on disk. Callers should use errors.Is.
var ErrTurnNotFound = errors.New("store: turn not found")

// turnsDir returns runs/<id>/turns. Sanitised by the caller.
func (s *FilesystemRunStore) turnsDir(runID string) string {
	return filepath.Join(s.runDir(runID), "turns")
}

// turnNodeDir returns runs/<id>/turns/<node>.
func (s *FilesystemRunStore) turnNodeDir(runID, nodeID string) string {
	return filepath.Join(s.turnsDir(runID), nodeID)
}

// turnIterDir returns runs/<id>/turns/<node>/<iter>.
func (s *FilesystemRunStore) turnIterDir(runID, nodeID string, loopIter int) string {
	return filepath.Join(s.turnNodeDir(runID, nodeID), strconv.Itoa(loopIter))
}

// turnJSONPath returns runs/<id>/turns/<node>/<iter>/<turn>.json.
func (s *FilesystemRunStore) turnJSONPath(runID, nodeID string, loopIter, turn int) string {
	return filepath.Join(s.turnIterDir(runID, nodeID, loopIter), strconv.Itoa(turn)+".json")
}

// turnAttemptJSONPath returns the path a SUPERSEDED attempt is preserved
// at: runs/<id>/turns/<node>/<iter>/<turn>.attempt-<n>.json. The
// coordinate's canonical file (<turn>.json) always holds the LATEST
// attempt; attempt 1 is the oldest superseded record.
func (s *FilesystemRunStore) turnAttemptJSONPath(runID, nodeID string, loopIter, turn, attempt int) string {
	return filepath.Join(s.turnIterDir(runID, nodeID, loopIter),
		strconv.Itoa(turn)+".attempt-"+strconv.Itoa(attempt)+".json")
}

// turnMessagesPath returns the sibling messages.json blob for a claw turn.
func (s *FilesystemRunStore) turnMessagesPath(runID, nodeID string, loopIter, turn int) string {
	return filepath.Join(s.turnIterDir(runID, nodeID, loopIter), strconv.Itoa(turn)+".messages.json")
}

// turnAttemptMessagesPath is the sidecar twin of turnAttemptJSONPath.
func (s *FilesystemRunStore) turnAttemptMessagesPath(runID, nodeID string, loopIter, turn, attempt int) string {
	return filepath.Join(s.turnIterDir(runID, nodeID, loopIter),
		strconv.Itoa(turn)+".attempt-"+strconv.Itoa(attempt)+".messages.json")
}

// turnIndexPath returns runs/<id>/turns/<node>/index.json — a tiny
// cache of "what's the latest turn for this node?" so LatestTurn
// doesn't need to walk the full directory tree on every lookup.
func (s *FilesystemRunStore) turnIndexPath(runID, nodeID string) string {
	return filepath.Join(s.turnNodeDir(runID, nodeID), "index.json")
}

// turnNodeIndex is the per-node aggregator stored at index.json.
// It maps loopIter → highest TurnIndex seen plus the latest write
// time, so LatestTurn picks (highest LoopIter, highest TurnIndex
// within that iter) in O(1).
type turnNodeIndex struct {
	Iterations map[string]TurnIndexEntry `json:"iterations"`
	// LatestLoopIter is the highest LoopIter currently present
	// (mirrored as a top-level field for cheap lookup; recomputed
	// on every WriteTurn).
	LatestLoopIter int       `json:"latest_loop_iter"`
	LastWritten    time.Time `json:"last_written"`
}

// WriteTurn satisfies TurnStore. Atomically writes the per-turn JSON
// (and the messages sibling when t.Messages is non-nil) and refreshes
// the per-node index.
//
// A write to a coordinate that already holds a DIFFERENT checkpoint is a
// new attempt of the node, not an edit: the derivation (model/hooks.go)
// knows the LLM step, not the execution attempt, so a retried or resumed
// node re-derives the coordinate its previous attempt wrote (#2806). The
// previous record is then preserved at <turn>.attempt-<n>.json (stamped
// Superseded, its messages sidecar moved along), the new write becomes
// the coordinate's canonical record with Attempt = previous+1, and
// ListTurns enumerates every attempt — the earlier attempt's usage stays
// in the baseline the bench reconciliation sums. A byte-identical
// redelivery (WrittenAt/Attempt/Superseded aside) remains the idempotent
// overwrite the contract has always promised and numbers nothing new.
func (s *FilesystemRunStore) WriteTurn(_ context.Context, t *TurnCheckpoint) error {
	if t == nil {
		return fmt.Errorf("store: WriteTurn: nil turn")
	}
	if err := sanitizePathComponent("run ID", t.RunID); err != nil {
		return err
	}
	if err := sanitizePathComponent("node ID", t.NodeID); err != nil {
		return err
	}
	if t.LoopIter < 0 || t.TurnIndex < 0 {
		return fmt.Errorf("store: WriteTurn: negative iter/turn (%d/%d)", t.LoopIter, t.TurnIndex)
	}
	dir := s.turnIterDir(t.RunID, t.NodeID, t.LoopIter)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir turn dir: %w", err)
	}
	if t.WrittenAt.IsZero() {
		t.WrittenAt = time.Now().UTC()
	}
	// The coordinate's read-modify-write (load the incumbent, maybe
	// supersede it, then the index merge below) is one critical section:
	// WriteTurn is driven by each parallel branch's goroutine and by a
	// resumed process that shares nothing with this one but the files, so
	// the files decide. refreshTurnIndexLocked is the index half, under
	// the same lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	incumbent, err := s.readTurnFile(s.turnJSONPath(t.RunID, t.NodeID, t.LoopIter, t.TurnIndex))
	if err != nil {
		return err
	}
	switch {
	case incumbent == nil:
		if t.Attempt < 1 {
			t.Attempt = 1
		}
		t.Superseded = false
	case turnsSameAttempt(incumbent, t):
		// Same attempt written twice: keep the incumbent's numbering.
		t.Attempt = incumbentAttempt(incumbent)
		t.Superseded = false
	default:
		if err := s.supersedeTurn(incumbent); err != nil {
			return err
		}
		t.Attempt = incumbentAttempt(incumbent) + 1
		t.Superseded = false
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal turn: %w", err)
	}
	if err := writeFileAtomic(s.turnJSONPath(t.RunID, t.NodeID, t.LoopIter, t.TurnIndex), data, filePerm); err != nil {
		return fmt.Errorf("store: write turn: %w", err)
	}
	if len(t.Messages) > 0 {
		if err := writeFileAtomic(s.turnMessagesPath(t.RunID, t.NodeID, t.LoopIter, t.TurnIndex), t.Messages, filePerm); err != nil {
			return fmt.Errorf("store: write turn messages: %w", err)
		}
	}
	return s.refreshTurnIndexLocked(t.RunID, t.NodeID, t.LoopIter, t.TurnIndex, t.WrittenAt)
}

// readTurnFile loads the checkpoint at path, nil (no error) when absent.
func (s *FilesystemRunStore) readTurnFile(path string) (*TurnCheckpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: read turn: %w", err)
	}
	var t TurnCheckpoint
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("store: unmarshal turn: %w", err)
	}
	return &t, nil
}

// incumbentAttempt reads a persisted record's attempt number; a record
// written before attempts were kept carries none and is attempt 1.
func incumbentAttempt(t *TurnCheckpoint) int {
	if t.Attempt < 1 {
		return 1
	}
	return t.Attempt
}

// turnsSameAttempt reports whether two checkpoints are one attempt
// written twice: every field equal, the bookkeeping ones (WrittenAt,
// Attempt, Superseded) aside. Messages carries no JSON form (the blob
// lands in the sidecar), so a same-checkpoint redelivery with a
// re-marshalled conversation still reads as one attempt.
func turnsSameAttempt(a, b *TurnCheckpoint) bool {
	ca, cb := *a, *b
	ca.WrittenAt, cb.WrittenAt = time.Time{}, time.Time{}
	ca.Attempt, cb.Attempt = 0, 0
	ca.Superseded, cb.Superseded = false, false
	ba, errA := json.Marshal(ca)
	bb, errB := json.Marshal(cb)
	return errA == nil && errB == nil && bytes.Equal(ba, bb)
}

// supersedeTurn preserves the coordinate's incumbent as
// <turn>.attempt-<n>.json, stamped Superseded with its attempt number,
// its messages sidecar moved to the attempt path beside it (a fork
// resolves the canonical record, so the incumbent's conversation must
// not keep answering for the new attempt under the canonical sidecar
// name). The caller holds s.mu.
func (s *FilesystemRunStore) supersedeTurn(incumbent *TurnCheckpoint) error {
	n := incumbentAttempt(incumbent)
	incumbent.Attempt = n
	incumbent.Superseded = true
	sidecar := s.turnMessagesPath(incumbent.RunID, incumbent.NodeID, incumbent.LoopIter, incumbent.TurnIndex)
	if blob, err := os.ReadFile(sidecar); err == nil {
		attemptSidecar := s.turnAttemptMessagesPath(incumbent.RunID, incumbent.NodeID, incumbent.LoopIter, incumbent.TurnIndex, n)
		if err := writeFileAtomic(attemptSidecar, blob, filePerm); err != nil {
			return fmt.Errorf("store: preserve turn messages: %w", err)
		}
		if err := os.Remove(sidecar); err != nil {
			return fmt.Errorf("store: move turn messages aside: %w", err)
		}
		if incumbent.MessagesRef != "" {
			incumbent.MessagesRef = incumbent.NodeID + "/" + strconv.Itoa(incumbent.LoopIter) + "/" +
				strconv.Itoa(incumbent.TurnIndex) + ".attempt-" + strconv.Itoa(n) + ".messages.json"
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("store: read turn messages: %w", err)
	}
	data, err := json.MarshalIndent(incumbent, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal superseded turn: %w", err)
	}
	if err := writeFileAtomic(s.turnAttemptJSONPath(incumbent.RunID, incumbent.NodeID, incumbent.LoopIter, incumbent.TurnIndex, n), data, filePerm); err != nil {
		return fmt.Errorf("store: preserve turn: %w", err)
	}
	return nil
}

// refreshTurnIndexLocked merges the (LoopIter, TurnIndex, WrittenAt)
// tuple into the per-node index. Reads the current index, updates the
// slot, writes atomically. The caller holds s.mu: WriteTurn's whole
// coordinate read-modify-write runs under it, so two turns of the same
// node racing here cannot lose index entries (last-writer-wins on the
// whole file) nor interleave a supersede with a fresh write.
func (s *FilesystemRunStore) refreshTurnIndexLocked(runID, nodeID string, loopIter, turn int, writtenAt time.Time) error {
	path := s.turnIndexPath(runID, nodeID)
	var idx turnNodeIndex
	if data, err := os.ReadFile(path); err == nil {
		// A corrupt index would otherwise be silently reset to zero-value and
		// overwritten below, permanently losing the recorded turn tuples.
		// Surface it; the rebuild from this single write is best-effort, but
		// the data loss must not be invisible.
		if uErr := json.Unmarshal(data, &idx); uErr != nil && len(data) > 0 && s.logger != nil {
			s.logger.Warn("store: turn index for run %s node %s is corrupt (%v) — rebuilding from this write; prior turn entries lost", runID, nodeID, uErr)
		}
	}
	if idx.Iterations == nil {
		idx.Iterations = make(map[string]TurnIndexEntry)
	}
	key := strconv.Itoa(loopIter)
	entry := idx.Iterations[key]
	if turn > entry.MaxTurn || entry.LastWritten.IsZero() {
		entry.MaxTurn = turn
	}
	entry.LoopIter = loopIter
	entry.LastWritten = writtenAt
	idx.Iterations[key] = entry
	if loopIter > idx.LatestLoopIter {
		idx.LatestLoopIter = loopIter
	}
	idx.LastWritten = writtenAt
	data, err := json.MarshalIndent(&idx, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal turn index: %w", err)
	}
	return writeFileAtomic(path, data, filePerm)
}

// LoadTurn satisfies TurnStore.
func (s *FilesystemRunStore) LoadTurn(_ context.Context, runID, nodeID string, loopIter, turn int) (*TurnCheckpoint, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.turnJSONPath(runID, nodeID, loopIter, turn))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: run=%s node=%s iter=%d turn=%d", ErrTurnNotFound, runID, nodeID, loopIter, turn)
		}
		return nil, fmt.Errorf("store: read turn: %w", err)
	}
	var t TurnCheckpoint
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("store: unmarshal turn: %w", err)
	}
	return &t, nil
}

// ListTurns satisfies TurnStore. Returns turns in ascending (TurnIndex,
// Attempt) order: every attempt of a coordinate is enumerated — the
// superseded records (<turn>.attempt-<n>.json) first, the canonical
// latest attempt last — so a reader summing the node's usage (bench
// discovery's reconciliation) counts every attempt's spend (#2806). The
// sibling messages.json blob is NOT inlined — callers that need it
// should follow up with LoadTurnMessages.
func (s *FilesystemRunStore) ListTurns(_ context.Context, runID, nodeID string, loopIter int) ([]*TurnCheckpoint, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	dir := s.turnIterDir(runID, nodeID, loopIter)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list turn dir: %w", err)
	}
	var rows []*TurnCheckpoint
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".messages.json") {
			continue
		}
		// <turn>.json is the coordinate's canonical record,
		// <turn>.attempt-<n>.json a preserved superseded one; anything
		// else is not a turn file.
		stem := strings.TrimSuffix(name, ".json")
		turnPart, _, _ := strings.Cut(stem, ".attempt-")
		if _, parseErr := strconv.Atoi(turnPart); parseErr != nil {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			return nil, fmt.Errorf("store: read turn %s: %w", name, readErr)
		}
		var t TurnCheckpoint
		if jerr := json.Unmarshal(data, &t); jerr != nil {
			return nil, fmt.Errorf("store: unmarshal turn %s: %w", name, jerr)
		}
		rows = append(rows, &t)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].TurnIndex != rows[j].TurnIndex {
			return rows[i].TurnIndex < rows[j].TurnIndex
		}
		// Within one coordinate the superseded records come first, the
		// canonical latest attempt last; a legacy record carrying no
		// attempt number reads as 1 and never shares its coordinate with
		// attempt files (those exist only once numbering did).
		return incumbentAttempt(rows[i]) < incumbentAttempt(rows[j])
	})
	return rows, nil
}

// LatestTurn satisfies TurnStore. Walks the per-node index.json once
// (O(iterations), tiny) and returns the highest (LoopIter, TurnIndex)
// turn. Falls back to a directory scan when index.json is missing
// (e.g. legacy run created before this feature shipped).
func (s *FilesystemRunStore) LatestTurn(ctx context.Context, runID, nodeID string) (*TurnCheckpoint, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	idxData, err := os.ReadFile(s.turnIndexPath(runID, nodeID))
	if err == nil {
		var idx turnNodeIndex
		if jerr := json.Unmarshal(idxData, &idx); jerr == nil && len(idx.Iterations) > 0 {
			entry, ok := idx.Iterations[strconv.Itoa(idx.LatestLoopIter)]
			if ok {
				return s.LoadTurn(ctx, runID, nodeID, entry.LoopIter, entry.MaxTurn)
			}
		}
	}
	// Fallback: scan node dir.
	dir := s.turnNodeDir(runID, nodeID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: run=%s node=%s", ErrTurnNotFound, runID, nodeID)
		}
		return nil, fmt.Errorf("store: scan turn node dir: %w", err)
	}
	maxIter := -1
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		iter, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue
		}
		if iter > maxIter {
			maxIter = iter
		}
	}
	if maxIter < 0 {
		return nil, fmt.Errorf("%w: run=%s node=%s", ErrTurnNotFound, runID, nodeID)
	}
	turns, err := s.ListTurns(ctx, runID, nodeID, maxIter)
	if err != nil {
		return nil, err
	}
	if len(turns) == 0 {
		return nil, fmt.Errorf("%w: run=%s node=%s iter=%d", ErrTurnNotFound, runID, nodeID, maxIter)
	}
	return turns[len(turns)-1], nil
}

// LoadTurnAtIndex returns the turn at TurnIndex on the highest loop
// iteration that has one, scanning all iterations (highest-first). Fork
// uses it for an explicit turn_index: a node inside a loop may only have a
// checkpoint at that turn on iteration > 0, so probing loop_iter=0 alone
// would spuriously fail with ErrTurnNotFound.
func (s *FilesystemRunStore) LoadTurnAtIndex(ctx context.Context, runID, nodeID string, turn int) (*TurnCheckpoint, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.turnNodeDir(runID, nodeID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: run=%s node=%s turn=%d", ErrTurnNotFound, runID, nodeID, turn)
		}
		return nil, fmt.Errorf("store: scan turn node dir: %w", err)
	}
	iters := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if iter, perr := strconv.Atoi(e.Name()); perr == nil {
			iters = append(iters, iter)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(iters)))
	for _, iter := range iters {
		t, err := s.LoadTurn(ctx, runID, nodeID, iter, turn)
		if err == nil {
			return t, nil
		}
		if !errors.Is(err, ErrTurnNotFound) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: run=%s node=%s turn=%d (any loop iter)", ErrTurnNotFound, runID, nodeID, turn)
}

// LoadTurnMessages satisfies TurnStore. Returns the sibling
// messages.json blob, or ErrTurnNotFound when missing.
func (s *FilesystemRunStore) LoadTurnMessages(_ context.Context, runID, nodeID string, loopIter, turn int) ([]byte, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.turnMessagesPath(runID, nodeID, loopIter, turn))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: run=%s node=%s iter=%d turn=%d messages", ErrTurnNotFound, runID, nodeID, loopIter, turn)
		}
		return nil, fmt.Errorf("store: read turn messages: %w", err)
	}
	return data, nil
}
