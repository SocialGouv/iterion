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

	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// writeFileAtomic writes data to path atomically by first writing to a sibling
// temp file (path+".tmp"), fsyncing, and then renaming over the destination.
// On POSIX, rename(2) is atomic for paths on the same filesystem, so a reader
// observes either the prior contents or the new contents — never a torn write.
//
// This matters for run.json (the authoritative resume checkpoint per CLAUDE.md):
// the prior code path used os.WriteFile, which truncates and then writes; a
// SIGKILL/OOM/power-loss between truncate and write produced an empty or
// partial JSON that LoadRun could no longer decode, making the run permanently
// unresumable.
//
// On error, the temp file is best-effort removed so we don't leak it.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("store: open temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("store: write temp file: %w", err)
	}
	// fsync the file contents before rename so the new bytes are durably on
	// disk; otherwise a crash after rename but before the data block flush
	// could still surface a zero-length file on recovery.
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("store: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: rename temp file: %w", err)
	}
	return nil
}

// SanitizePathComponent validates that a path component (RunID, NodeID,
// InteractionID) does not contain path traversal sequences, separators,
// or null bytes. Used at every store/runview/blob entry point that
// path-joins user-derived IDs into the run directory.
func SanitizePathComponent(name, component string) error {
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

// sanitizePathComponent is kept as a private alias so internal call
// sites within pkg/store don't need to be touched. Prefer the
// exported name for new code.
var sanitizePathComponent = SanitizePathComponent

// ---------------------------------------------------------------------------
// FilesystemRunStore — file-backed persistence for runs
// ---------------------------------------------------------------------------

// FilesystemRunStore manages the on-disk layout:
//
//	<root>/runs/<run_id>/run.json
//	<root>/runs/<run_id>/events.jsonl
//	<root>/runs/<run_id>/artifacts/<node>/<version>.json
//	<root>/runs/<run_id>/interactions/<interaction_id>.json
type FilesystemRunStore struct {
	root   string // base directory
	logger *iterlog.Logger

	mu      sync.Mutex
	seq     map[string]int64 // run_id → next event sequence number
	seqSeed map[string]bool  // run_id → seq has been seeded from disk
}

// StoreOption configures a FilesystemRunStore.
type StoreOption func(*FilesystemRunStore)

// WithLogger sets a leveled logger on the store.
func WithLogger(l *iterlog.Logger) StoreOption {
	return func(s *FilesystemRunStore) { s.logger = l }
}

// New creates a FilesystemRunStore rooted at the given directory.
// The directory is created if it does not exist.
func New(root string, opts ...StoreOption) (*FilesystemRunStore, error) {
	if err := os.MkdirAll(filepath.Join(root, "runs"), dirPerm); err != nil {
		return nil, fmt.Errorf("store: create root: %w", err)
	}
	// Best-effort: drop a self-ignoring .gitignore so the store dir is never
	// accidentally committed even if the user skipped `iterion init`.
	// Failures (read-only FS, permission, etc.) are non-fatal.
	_ = ensureGitignore(root)
	s := &FilesystemRunStore{
		root:    root,
		seq:     make(map[string]int64),
		seqSeed: make(map[string]bool),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// ensureGitignore writes a self-ignoring .gitignore at the store root if none
// exists. Existing files are left untouched so user customizations are kept.
func ensureGitignore(root string) error {
	path := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte("**\n"), filePerm)
}

// Root returns the store root directory.
func (s *FilesystemRunStore) Root() string { return s.root }

// ---------------------------------------------------------------------------
// Run lifecycle
// ---------------------------------------------------------------------------

// CreateRun persists a new run with status "running".
func (s *FilesystemRunStore) CreateRun(id, workflowName string, inputs map[string]interface{}) (*Run, error) {
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

// SaveRun persists the run metadata to disk.
func (s *FilesystemRunStore) SaveRun(r *Run) error {
	return s.writeRun(r)
}

// LoadRun reads run.json for the given run ID.
//
// The run ID is sanitised before path-joining so a hostile or
// network-sourced ID cannot escape the store root. The write side
// (CreateRun/WriteArtifact/WriteInteraction) already sanitises its inputs;
// the read paths must do the same so the defence is symmetric.
//
// As a one-shot migration step, a legacy run with empty Name gets a
// deterministic friendly label generated and persisted on read. After
// the first call the field is on disk; subsequent LoadRuns skip the
// fixup. The seed mirrors the CLI/launch path (file_path:run_id) so the
// backfill produces the exact name a new launch would have produced.
func (s *FilesystemRunStore) LoadRun(id string) (*Run, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	p := s.runJSONPath(id)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("store: load run %s: %w", id, err)
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("store: decode run %s: %w", id, err)
	}
	if r.Name == "" {
		r.Name = GenerateRunName(r.FilePath + ":" + r.ID)
		// Best-effort persist; a write failure (read-only fs, racing
		// process) leaves the in-memory name set and lets the next
		// successful write fix it up. Never fail LoadRun on this path.
		if writeErr := s.writeRun(&r); writeErr != nil && s.logger != nil {
			s.logger.Warn("store: backfill name for run %s failed: %v", id, writeErr)
		}
	}
	// Heal runs persisted by an older binary that left FinishedAt set
	// across a resume into Running. The duration ticker in the editor
	// keys on finished_at, so a stale value freezes the displayed run
	// time. Best-effort persist mirrors the name-backfill above.
	if r.Status == RunStatusRunning && r.FinishedAt != nil {
		r.FinishedAt = nil
		if writeErr := s.writeRun(&r); writeErr != nil && s.logger != nil {
			s.logger.Warn("store: heal stale finished_at on run %s failed: %v", id, writeErr)
		}
	}
	return &r, nil
}

// UpdateRunStatus updates the status (and optional error) of a run.
// Protected by mu to prevent concurrent read-modify-write races.
func (s *FilesystemRunStore) UpdateRunStatus(id string, status RunStatus, runErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.LoadRun(id)
	if err != nil {
		return err
	}
	r.Status = status
	r.UpdatedAt = time.Now().UTC()
	r.Error = runErr
	switch status {
	case RunStatusFinished, RunStatusFailed, RunStatusFailedResumable, RunStatusCancelled:
		t := r.UpdatedAt
		r.FinishedAt = &t
	case RunStatusRunning, RunStatusPausedWaitingHuman:
		// Resume paths (failed_resumable/cancelled → running) must clear
		// FinishedAt — otherwise the editor's duration ticker uses the
		// stale terminal timestamp and freezes mid-run.
		r.FinishedAt = nil
	}
	// Clear checkpoint when leaving paused state (preserved for failed_resumable and cancelled).
	if status == RunStatusRunning || status == RunStatusFinished || status == RunStatusFailed {
		r.Checkpoint = nil
	}
	return s.writeRun(r)
}

// SaveCheckpoint persists a checkpoint on a paused run.
// Protected by mu to prevent concurrent read-modify-write races.
func (s *FilesystemRunStore) SaveCheckpoint(id string, cp *Checkpoint) error {
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
func (s *FilesystemRunStore) PauseRun(id string, cp *Checkpoint) error {
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

// FailRunResumable atomically sets the checkpoint, error message, and status
// to failed_resumable in a single write, enabling resume from the last
// successfully completed node.
func (s *FilesystemRunStore) FailRunResumable(id string, cp *Checkpoint, runErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.LoadRun(id)
	if err != nil {
		return err
	}
	r.Checkpoint = cp
	r.Status = RunStatusFailedResumable
	r.Error = runErr
	t := time.Now().UTC()
	r.UpdatedAt = t
	r.FinishedAt = &t
	return s.writeRun(r)
}

// ListRuns returns the IDs of all persisted runs.
func (s *FilesystemRunStore) ListRuns() ([]string, error) {
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
func (s *FilesystemRunStore) AppendEvent(runID string, evt Event) (*Event, error) {
	evt.RunID = runID
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// On first append for this runID since process start, seed the in-memory
	// sequence counter from any existing events.jsonl. Without this, a fresh
	// process opening a pre-existing run (typical for `iterion resume`) would
	// restart Seq at 0 and produce duplicate sequence numbers in the
	// append-only event stream — breaking the documented monotonic ordering
	// and any downstream consumer that dedups by Seq.
	if !s.seqSeed[runID] {
		// scanMaxSeqLocked now returns the best-effort max+1 even on
		// partial scan failures (a scanner error past N readable lines
		// returns N+1 rather than 0), so we can trust `next` regardless
		// of err. Restarting at 0 on a partial scan would collide with
		// the readable-but-skipped tail and break the monotonic Seq
		// invariant downstream consumers rely on for dedup. The error
		// remains worth logging so an operator can investigate the
		// corruption — but we don't gate on it.
		next, err := s.scanMaxSeqLocked(runID)
		s.seq[runID] = next
		if err != nil {
			s.logger.Warn("store: partial seq seed for run %s: %v (resuming from %d — best-effort)", runID, err, next)
		}
		s.seqSeed[runID] = true
	}

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

	// Flush to disk before advancing the sequence counter to avoid
	// losing events on crash while the in-memory counter has advanced.
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("store: sync event: %w", err)
	}

	// Only increment after successful write — no sequence gaps on failure.
	s.seq[runID] = evt.Seq + 1

	return &evt, nil
}

// ScanEvents streams events for a run through visit, in file order, and
// stops as soon as visit returns false. It allocates one *Event per
// scanned line (decoded into a fresh struct) so the caller can retain
// references freely, but it never materialises the full events.jsonl
// slice — callers searching for a single match (e.g. node-touched
// filter) or paginating a window can short-circuit without paying the
// O(file) memory of LoadEvents.
//
// Errors decoding a single line are skipped (consistent with
// LoadEvents). The returned error reflects file-open / scanner-buffer
// failures, not per-line parse errors.
//
// runID is sanitised before path-joining (see LoadRun for rationale).
func (s *FilesystemRunStore) ScanEvents(runID string, visit func(*Event) bool) error {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return err
	}
	p := s.eventsPath(runID)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store: open events: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLineSize)
	var skipped int
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		evt := &Event{}
		if err := json.Unmarshal(line, evt); err != nil {
			skipped++
			continue
		}
		if !visit(evt) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("store: scan events: %w", err)
	}
	if skipped > 0 {
		s.logger.Warn("skipped %d corrupt event line(s) in run %s", skipped, runID)
	}
	return nil
}

// LoadEventsRange streams events with seq in [from, to) (to == 0 means
// "no upper bound") and caps the returned slice at limit (limit == 0
// means "no cap"). Designed for paginating long events.jsonl tails
// without allocating the whole file: a 200MB events.jsonl with limit=
// 5000 returns at most 5000 entries instead of materialising every
// event in memory just to slice the head.
//
// The caller can detect "more available" by passing limit and checking
// whether len(out) == limit; the next page starts at out[len(out)-1].Seq+1.
func (s *FilesystemRunStore) LoadEventsRange(runID string, from, to int64, limit int) ([]*Event, error) {
	var out []*Event
	if limit > 0 {
		out = make([]*Event, 0, limit)
	}
	err := s.ScanEvents(runID, func(e *Event) bool {
		if e.Seq < from {
			return true
		}
		if to > 0 && e.Seq >= to {
			return false // events.jsonl is monotonic in Seq → safe to stop
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LoadEvents reads all events for a run in sequence order.
//
// runID is sanitised before path-joining (see LoadRun for rationale).
func (s *FilesystemRunStore) LoadEvents(runID string) ([]*Event, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
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
	var skipped int
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var evt Event
		if err := json.Unmarshal(line, &evt); err != nil {
			// Skip corrupt lines rather than aborting — partial corruption
			// should not prevent reading subsequent valid events.
			skipped++
			continue
		}
		events = append(events, &evt)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("store: scan events: %w", err)
	}
	if skipped > 0 {
		s.logger.Warn("skipped %d corrupt event line(s) in run %s", skipped, runID)
	}
	return events, nil
}

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------

// WriteArtifact persists an artifact for a node at the given version and
// updates the run's artifact index for O(1) latest-version lookups.
func (s *FilesystemRunStore) WriteArtifact(a *Artifact) error {
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
	if err := writeFileAtomic(p, data, filePerm); err != nil {
		return err
	}

	// Update the artifact index in run.json. The index is a cache — if it's
	// stale, LoadLatestArtifact falls back to a directory scan — so a fresh
	// run with no run.json yet (LoadRun errors with NotExist) is not fatal.
	// But once run.json exists, a failure to update the index (e.g. ENOSPC,
	// permission denied, JSON encode error) IS surfaced to the caller: a
	// silently dropped index update can cause downstream nodes to read a
	// stale artifact version, which is a correctness bug, not a performance
	// degradation. Callers can decide to retry or fail the run.
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.LoadRun(a.RunID)
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
			// No run.json yet (e.g. early CreateRun race) — artifact written,
			// index will be populated by a later write or by directory scan.
			return nil
		}
		return fmt.Errorf("store: write artifact: load run for index update: %w", err)
	}
	if r.ArtifactIndex == nil {
		r.ArtifactIndex = make(map[string]int)
	}
	if cur, ok := r.ArtifactIndex[a.NodeID]; !ok || a.Version > cur {
		r.ArtifactIndex[a.NodeID] = a.Version
		if err := s.writeRun(r); err != nil {
			return fmt.Errorf("store: write artifact: update index: %w", err)
		}
	}
	return nil
}

// LoadArtifact reads a specific artifact version.
func (s *FilesystemRunStore) LoadArtifact(runID, nodeID string, version int) (*Artifact, error) {
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
// It first checks the run's artifact index for an O(1) lookup and falls back
// to a directory scan for backward compatibility with older run formats.
func (s *FilesystemRunStore) LoadLatestArtifact(runID, nodeID string) (*Artifact, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}

	// Fast path: use artifact index if available.
	if r, err := s.LoadRun(runID); err == nil && r.ArtifactIndex != nil {
		if v, ok := r.ArtifactIndex[nodeID]; ok {
			return s.LoadArtifact(runID, nodeID, v)
		}
	}

	// Fallback: directory scan (backward compat with old runs without index).
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

// ArtifactVersionInfo is the lightweight (version, mtime) pair returned by
// ListArtifactVersions — the directory enumeration without the full body
// decode that LoadArtifact incurs.
type ArtifactVersionInfo struct {
	Version   int
	WrittenAt time.Time
}

// ListArtifactVersions enumerates the persisted artifact versions for a
// node in ascending order, returning each version's mtime without
// decoding the body. Returns (nil, nil) when the node has no artifact
// directory (a node that hasn't published anything yet).
func (s *FilesystemRunStore) ListArtifactVersions(runID, nodeID string) ([]ArtifactVersionInfo, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, "runs", runID, "artifacts", nodeID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list artifact versions: %w", err)
	}
	out := make([]ArtifactVersionInfo, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, ArtifactVersionInfo{Version: v, WrittenAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// ---------------------------------------------------------------------------
// Interactions (human input/output)
// ---------------------------------------------------------------------------

// WriteInteraction persists a human interaction.
func (s *FilesystemRunStore) WriteInteraction(i *Interaction) error {
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
	return writeFileAtomic(p, data, filePerm)
}

// LoadInteraction reads a specific interaction by ID.
func (s *FilesystemRunStore) LoadInteraction(runID, interactionID string) (*Interaction, error) {
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
//
// runID is sanitised before path-joining (see LoadRun for rationale).
func (s *FilesystemRunStore) ListInteractions(runID string) ([]string, error) {
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

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (s *FilesystemRunStore) runDir(runID string) string {
	return filepath.Join(s.root, "runs", runID)
}

func (s *FilesystemRunStore) runJSONPath(runID string) string {
	return filepath.Join(s.runDir(runID), "run.json")
}

func (s *FilesystemRunStore) eventsPath(runID string) string {
	return filepath.Join(s.runDir(runID), "events.jsonl")
}

// scanMaxSeqLocked reads events.jsonl for runID and returns max(Seq)+1, the
// value that should be assigned to the next appended event. Returns 0 (with
// nil error) if the file does not exist (fresh run) or contains no decodable
// lines. Caller must hold s.mu.
//
// This intentionally does NOT use LoadEvents (which allocates the full slice
// of events) — we only need the max Seq, so we scan and discard.
func (s *FilesystemRunStore) scanMaxSeqLocked(runID string) (int64, error) {
	p := s.eventsPath(runID)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	var maxSeq int64 = -1
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLineSize)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Decode only the seq field — minimal struct keeps allocations low.
		var hdr struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &hdr); err != nil {
			// Skip corrupt lines rather than aborting (consistent with
			// LoadEvents' tolerant behaviour).
			continue
		}
		if hdr.Seq > maxSeq {
			maxSeq = hdr.Seq
		}
	}
	scanErr := scanner.Err()
	// Always return the best-effort max+1: when scanner.Err is non-nil
	// (oversized line, read failure mid-file) the readable prefix's
	// max is still trustworthy. Restarting from 0 on a partial scan
	// would collide with prior events and break the monotonic Seq
	// invariant. Caller logs scanErr; this function never withholds
	// the count it managed to compute.
	next := int64(0)
	if maxSeq >= 0 {
		next = maxSeq + 1
	}
	return next, scanErr
}

func (s *FilesystemRunStore) writeRun(r *Run) error {
	// Defence in depth: every public entry point that mutates a run
	// (SaveRun, UpdateRunStatus, SaveCheckpoint, PauseRun,
	// FailRunResumable) flows through here. Sanitise once, here, so
	// e.g. a Run loaded with a tampered ID can't be re-serialised to a
	// path outside the store root.
	if err := sanitizePathComponent("run ID", r.ID); err != nil {
		return err
	}
	dir := s.runDir(r.ID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir run: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal run: %w", err)
	}
	// Atomic write: run.json is the authoritative resume checkpoint
	// (per CLAUDE.md). A torn write would lose all prior checkpoint state.
	return writeFileAtomic(s.runJSONPath(r.ID), data, filePerm)
}
