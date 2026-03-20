package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxEventLineSize is the maximum size of a single event JSON line.
// Events with large LLM outputs can exceed the default 64KB scanner buffer.
const maxEventLineSize = 10 * 1024 * 1024 // 10 MB

// File and directory permissions for store data.
// Restrictive by default — artifacts and interactions may contain sensitive data.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// sanitizePathComponent validates that a path component (RunID, NodeID,
// InteractionID) does not contain path traversal sequences or separators.
func sanitizePathComponent(name, component string) error {
	if component == "" {
		return fmt.Errorf("store: %s must not be empty", name)
	}
	if strings.Contains(component, "..") {
		return fmt.Errorf("store: %s %q contains path traversal", name, component)
	}
	if strings.ContainsAny(component, "/\\") {
		return fmt.Errorf("store: %s %q contains path separator", name, component)
	}
	if strings.ContainsRune(component, 0) {
		return fmt.Errorf("store: %s %q contains null byte", name, component)
	}
	return nil
}

// ---------------------------------------------------------------------------
// RunStore — file-backed persistence for runs
// ---------------------------------------------------------------------------

// RunStore manages the on-disk layout:
//
//	<root>/runs/<run_id>/run.json
//	<root>/runs/<run_id>/events.jsonl
//	<root>/runs/<run_id>/artifacts/<node>/<version>.json
//	<root>/runs/<run_id>/interactions/<interaction_id>.json
type RunStore struct {
	root string // base directory

	mu  sync.Mutex
	seq map[string]int64 // run_id → next event sequence number
}

// New creates a RunStore rooted at the given directory.
// The directory is created if it does not exist.
func New(root string) (*RunStore, error) {
	if err := os.MkdirAll(filepath.Join(root, "runs"), dirPerm); err != nil {
		return nil, fmt.Errorf("store: create root: %w", err)
	}
	return &RunStore{root: root, seq: make(map[string]int64)}, nil
}

// Root returns the store root directory.
func (s *RunStore) Root() string { return s.root }

// ---------------------------------------------------------------------------
// Run lifecycle
// ---------------------------------------------------------------------------

// CreateRun persists a new run with status "running".
func (s *RunStore) CreateRun(id, workflowName string, inputs map[string]interface{}) (*Run, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	r := &Run{
		FormatVersion: RunFormatVersion,
		ID:            id,
		WorkflowName:  workflowName,
		Status:        RunStatusRunning,
		Inputs:        inputs,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.writeRun(r); err != nil {
		return nil, err
	}
	return r, nil
}

// LoadRun reads run.json for the given run ID.
func (s *RunStore) LoadRun(id string) (*Run, error) {
	p := s.runJSONPath(id)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("store: load run %s: %w", id, err)
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("store: decode run %s: %w", id, err)
	}
	return &r, nil
}

// UpdateRunStatus updates the status (and optional error) of a run.
// Protected by mu to prevent concurrent read-modify-write races.
func (s *RunStore) UpdateRunStatus(id string, status RunStatus, runErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.LoadRun(id)
	if err != nil {
		return err
	}
	r.Status = status
	r.UpdatedAt = time.Now().UTC()
	r.Error = runErr
	if status == RunStatusFinished || status == RunStatusFailed || status == RunStatusCancelled {
		t := r.UpdatedAt
		r.FinishedAt = &t
	}
	// Clear checkpoint when leaving paused state.
	if status == RunStatusRunning || status == RunStatusFinished || status == RunStatusFailed || status == RunStatusCancelled {
		r.Checkpoint = nil
	}
	return s.writeRun(r)
}

// SaveCheckpoint persists a checkpoint on a paused run.
// Protected by mu to prevent concurrent read-modify-write races.
func (s *RunStore) SaveCheckpoint(id string, cp *Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.LoadRun(id)
	if err != nil {
		return err
	}
	r.Checkpoint = cp
	r.UpdatedAt = time.Now().UTC()
	return s.writeRun(r)
}

// PauseRun atomically sets the checkpoint and updates the status to paused
// in a single write, preventing inconsistency if one of two separate
// operations were to fail.
func (s *RunStore) PauseRun(id string, cp *Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.LoadRun(id)
	if err != nil {
		return err
	}
	r.Checkpoint = cp
	r.Status = RunStatusPausedWaitingHuman
	r.UpdatedAt = time.Now().UTC()
	return s.writeRun(r)
}

// ListRuns returns the IDs of all persisted runs.
func (s *RunStore) ListRuns() ([]string, error) {
	runsDir := filepath.Join(s.root, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// AppendEvent appends an event to the run's events.jsonl.
// Seq and Timestamp are set automatically.
// The entire operation is serialized under mu to prevent interleaved writes
// from concurrent branches. The sequence counter is only incremented after
// a successful write to avoid gaps in the event stream.
func (s *RunStore) AppendEvent(runID string, evt Event) (*Event, error) {
	evt.RunID = runID
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Assign seq but don't increment the counter yet — only advance on
	// successful write to prevent gaps from failed marshals or I/O.
	evt.Seq = s.seq[runID]

	line, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("store: marshal event: %w", err)
	}
	line = append(line, '\n')

	p := s.eventsPath(runID)
	if err := os.MkdirAll(filepath.Dir(p), dirPerm); err != nil {
		return nil, fmt.Errorf("store: mkdir events: %w", err)
	}

	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("store: open events: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(line); err != nil {
		return nil, fmt.Errorf("store: write event: %w", err)
	}

	// Only increment after successful write — no sequence gaps on failure.
	s.seq[runID] = evt.Seq + 1

	return &evt, nil
}

// LoadEvents reads all events for a run in sequence order.
func (s *RunStore) LoadEvents(runID string) ([]*Event, error) {
	p := s.eventsPath(runID)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: open events: %w", err)
	}
	defer f.Close()

	var events []*Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt Event
		if err := json.Unmarshal(line, &evt); err != nil {
			return nil, fmt.Errorf("store: decode event line: %w", err)
		}
		events = append(events, &evt)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("store: scan events: %w", err)
	}
	return events, nil
}

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------

// WriteArtifact persists an artifact for a node at the given version.
func (s *RunStore) WriteArtifact(a *Artifact) error {
	if err := sanitizePathComponent("run ID", a.RunID); err != nil {
		return err
	}
	if err := sanitizePathComponent("node ID", a.NodeID); err != nil {
		return err
	}
	if a.WrittenAt.IsZero() {
		a.WrittenAt = time.Now().UTC()
	}
	dir := filepath.Join(s.root, "runs", a.RunID, "artifacts", a.NodeID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir artifact: %w", err)
	}
	p := filepath.Join(dir, fmt.Sprintf("%d.json", a.Version))
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal artifact: %w", err)
	}
	return os.WriteFile(p, data, filePerm)
}

// LoadArtifact reads a specific artifact version.
func (s *RunStore) LoadArtifact(runID, nodeID string, version int) (*Artifact, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	p := filepath.Join(s.root, "runs", runID, "artifacts", nodeID, fmt.Sprintf("%d.json", version))
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("store: load artifact: %w", err)
	}
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("store: decode artifact: %w", err)
	}
	return &a, nil
}

// LoadLatestArtifact returns the artifact with the highest version for a node.
func (s *RunStore) LoadLatestArtifact(runID, nodeID string) (*Artifact, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, "runs", runID, "artifacts", nodeID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts: %w", err)
	}
	maxVersion := -1
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		vStr := strings.TrimSuffix(name, ".json")
		v, err := strconv.Atoi(vStr)
		if err != nil {
			continue
		}
		if v > maxVersion {
			maxVersion = v
		}
	}
	if maxVersion < 0 {
		return nil, fmt.Errorf("store: no artifacts for node %s in run %s", nodeID, runID)
	}
	return s.LoadArtifact(runID, nodeID, maxVersion)
}

// ---------------------------------------------------------------------------
// Interactions (human input/output)
// ---------------------------------------------------------------------------

// WriteInteraction persists a human interaction.
func (s *RunStore) WriteInteraction(i *Interaction) error {
	if err := sanitizePathComponent("run ID", i.RunID); err != nil {
		return err
	}
	if err := sanitizePathComponent("interaction ID", i.ID); err != nil {
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
	return os.WriteFile(p, data, filePerm)
}

// LoadInteraction reads a specific interaction by ID.
func (s *RunStore) LoadInteraction(runID, interactionID string) (*Interaction, error) {
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

// ListInteractions returns all interaction IDs for a run.
func (s *RunStore) ListInteractions(runID string) ([]string, error) {
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

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (s *RunStore) runDir(runID string) string {
	return filepath.Join(s.root, "runs", runID)
}

func (s *RunStore) runJSONPath(runID string) string {
	return filepath.Join(s.runDir(runID), "run.json")
}

func (s *RunStore) eventsPath(runID string) string {
	return filepath.Join(s.runDir(runID), "events.jsonl")
}

func (s *RunStore) writeRun(r *Run) error {
	dir := s.runDir(r.ID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir run: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal run: %w", err)
	}
	return os.WriteFile(s.runJSONPath(r.ID), data, filePerm)
}
