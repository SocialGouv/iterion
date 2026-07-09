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

// turnMessagesPath returns the sibling messages.json blob for a claw turn.
func (s *FilesystemRunStore) turnMessagesPath(runID, nodeID string, loopIter, turn int) string {
	return filepath.Join(s.turnIterDir(runID, nodeID, loopIter), strconv.Itoa(turn)+".messages.json")
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
	return s.refreshTurnIndex(t.RunID, t.NodeID, t.LoopIter, t.TurnIndex, t.WrittenAt)
}

// refreshTurnIndex merges the (LoopIter, TurnIndex, WrittenAt) tuple
// into the per-node index. Reads the current index, updates the slot,
// writes atomically. The read-modify-write of index.json is guarded by
// s.mu: WriteTurn is driven by the per-turn backend hook on EACH
// parallel branch's goroutine (model/hooks.go), not under s.mu, so two
// turns of the same node racing here would otherwise lose index entries
// (last-writer-wins on the whole file). No caller holds s.mu when
// reaching WriteTurn, so taking it here cannot deadlock.
func (s *FilesystemRunStore) refreshTurnIndex(runID, nodeID string, loopIter, turn int, writtenAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// ListTurns satisfies TurnStore. Returns turns in ascending TurnIndex
// order. The sibling messages.json blob is NOT inlined — callers that
// need it should follow up with LoadTurnMessages.
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
	type indexed struct {
		idx int
		t   *TurnCheckpoint
	}
	var rows []indexed
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".messages.json") {
			continue
		}
		idxStr := strings.TrimSuffix(name, ".json")
		idx, parseErr := strconv.Atoi(idxStr)
		if parseErr != nil {
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
		rows = append(rows, indexed{idx, &t})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].idx < rows[j].idx })
	out := make([]*TurnCheckpoint, len(rows))
	for i, r := range rows {
		out[i] = r.t
	}
	return out, nil
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
