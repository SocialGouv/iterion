package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
// writes atomically. Concurrent writes from parallel branches are
// serialised by FilesystemRunStore.mu — held by the caller (Engine
// keeps turn writes single-threaded per node).
func (s *FilesystemRunStore) refreshTurnIndex(runID, nodeID string, loopIter, turn int, writtenAt time.Time) error {
	path := s.turnIndexPath(runID, nodeID)
	var idx turnNodeIndex
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &idx)
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

// walkTurnDirs is a small helper used by `iterion fork --strict-hash`
// audits and by per-run GC (Phase 2). Walks the runs/<id>/turns tree
// and emits one path per turn directory. Caller filters.
func (s *FilesystemRunStore) walkTurnDirs(runID string, visit func(nodeID string, loopIter int) error) error {
	root := s.turnsDir(runID)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 {
			return nil
		}
		iter, parseErr := strconv.Atoi(parts[1])
		if parseErr != nil {
			return nil
		}
		return visit(parts[0], iter)
	})
}
