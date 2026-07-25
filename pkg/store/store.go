package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// maxEventLineSize is the maximum size of a single event JSON line.
// Events with large LLM outputs can exceed the default 64KB scanner buffer.
const maxEventLineSize = 10 * 1024 * 1024 // 10 MB

// Event-stream corruption thresholds. A skipped line is one that failed
// json.Unmarshal — typically a torn write at process kill. A single
// skipped line at EOF is benign; massive skipping means the audit
// trail is unreliable and callers should surface that rather than
// silently serving a near-empty event log as if it were complete.
const (
	eventsCorruptionAbsThreshold   = 100
	eventsCorruptionRatioThreshold = 2 // skipped > valid/2 i.e. > ~33% corruption
)

// ErrEventsCorrupted signals that an events.jsonl file had so many
// unparseable lines (above eventsCorruptionAbsThreshold absolute or
// eventsCorruptionRatioThreshold ratio) that the returned data should
// not be treated as a complete audit trail. The error wraps with the
// counts so callers can errors.As() it or display a banner.
var ErrEventsCorrupted = fmt.Errorf("store: events.jsonl is severely corrupted")

// eventsCorruptionExceeded returns true when skipped lines exceed the
// safety threshold. Single trailing skip lines (e.g. a torn write at
// the very end) are tolerated; mass corruption is not.
func eventsCorruptionExceeded(skipped, valid int) bool {
	if skipped <= 1 {
		return false
	}
	if skipped > eventsCorruptionAbsThreshold {
		return true
	}
	return valid > 0 && skipped*eventsCorruptionRatioThreshold > valid
}

// File and directory permissions for store data.
// Restrictive by default — artifacts and interactions may contain sensitive data.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// WriteFileAtomic writes data to path atomically by first writing to a sibling
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
//
// Exported so other on-disk subsystems (e.g. the privacy vault) can reuse
// the same write semantics without duplicating the algorithm.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	// Use os.CreateTemp for a unique temp name. The previous fixed
	// `path+".tmp"` collided when two concurrent writers (e.g. an
	// in-process write racing an external process touching the same
	// file) both staged through the same temp path, producing torn
	// renames. CreateTemp + chmod gives us per-call isolation.
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".atomic-*")
	if err != nil {
		return fmt.Errorf("store: open temp file: %w", err)
	}
	tmp := f.Name()
	// Apply the requested mode now; CreateTemp uses 0600.
	if err := os.Chmod(tmp, perm); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("store: chmod temp file: %w", err)
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
	// fsync the parent directory so the rename itself is durably on
	// disk. Without this, ext4 (and other journalling filesystems) can
	// commit the file data and the rename out-of-order on crash, leaving
	// the prior name pointing nowhere — the run.json we just "atomically"
	// replaced can vanish after a power loss. fsync-on-dir is the
	// well-known second half of a durable atomic write; missing it was
	// the long-standing footgun.
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("store: sync dir: %w", err)
	}
	return nil
}

// WriteFileAtomicNew writes data to path durably but refuses to clobber an
// existing destination. It stages through a sibling temp file, fsyncs the
// bytes, then atomically links the temp inode into place. The hard-link step is
// an exclusive create: if path already exists (or a racing creator wins), the
// destination is left untouched and fs.ErrExist is returned in the error chain.
func WriteFileAtomicNew(path string, data []byte, perm os.FileMode) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".create-*")
	if err != nil {
		return fmt.Errorf("store: open temp file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Chmod(tmp, perm); err != nil {
		f.Close()
		return fmt.Errorf("store: chmod temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("store: write temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("store: sync temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close temp file: %w", err)
	}
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("store: create %s: %w", path, fs.ErrExist)
		}
		return fmt.Errorf("store: link temp file: %w", err)
	}
	// Drop the temp directory entry now that path links to the fsynced inode,
	// then sync the directory so both the new run.json name and temp cleanup are
	// durable. The deferred Remove is harmless if this succeeds.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store: remove temp file: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("store: sync dir: %w", err)
	}
	return nil
}

// fsyncDir opens dir and calls Sync on the descriptor so the directory
// entry update from the most recent rename/create reaches the platter.
// On platforms where opening a directory for sync isn't supported the
// fallback is a best-effort no-op (see openat_other.go's companions).
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		// On some platforms (notably Windows) Sync on a directory
		// handle returns EBADF/ENOTSUP. Swallow those — the atomic
		// rename semantics differ on those filesystems anyway.
		if errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP) {
			return closeErr
		}
		return syncErr
	}
	return closeErr
}

// SanitizePathComponent validates that a path component (RunID, NodeID,
// InteractionID) does not contain path traversal sequences, separators,
// null bytes, or control characters. Used at every store/runview/blob
// entry point that path-joins user-derived IDs into the run directory,
// and also at the HMAC-signing entry points where control characters in
// a component could collide with a separator in the MAC plaintext (see
// PresignAttachment) and make one signature accidentally valid for a
// different (runID, name) pair.
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
	for _, r := range component {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("store: %s %q contains control character", name, component)
		}
	}
	return nil
}

// sanitizePathComponent is kept as a private alias so internal call
// sites within pkg/store don't need to be touched. Prefer the
// exported name for new code.
var sanitizePathComponent = SanitizePathComponent

// writeFileAtomic is the legacy private alias; new code should call
// the exported WriteFileAtomic directly.
var writeFileAtomic = WriteFileAtomic

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

	mu         sync.Mutex
	seq        map[string]int64 // run_id → next event sequence number
	seqSeed    map[string]bool  // run_id → seq has been seeded from disk
	signingKey []byte           // HMAC key for presigned attachment URLs (lazy)

	// inboxVersion is incremented on every user-message write so a
	// hot-path consumer (the agent-loop inbox drainer) can cheap-skip
	// loading the JSONL when nothing changed. Read+write under `mu`.
	inboxVersion map[string]uint64

	// eventsUnsynced marks runs whose events.jsonl has appended bytes
	// that a failed fsync left without a durability guarantee.
	// AppendEvent treats an fsync failure as non-fatal (advancing Seq
	// keeps the stream monotonic), but run.json — written atomically
	// WITH fsync — must not durably reference state whose events never
	// reached disk: a power loss would then recover a checkpoint ahead
	// of its event log. writeRun re-syncs a flagged run's events file
	// first (the write-ahead ordering barrier) and only proceeds when
	// that succeeds. Guarded by `mu`.
	eventsUnsynced map[string]bool

	// logPositionFn returns the current per-run log buffer byte total
	// for stamping Event.LogOffset at AppendEvent time. nil disables
	// stamping (LogOffset stays 0). Wired post-construction by the
	// runview Service, which owns the buffer registry; concrete-type
	// setter rather than constructor option because the buffer
	// lifecycle outlives any single store option pass.
	logPositionMu sync.RWMutex
	logPositionFn LogPositionFn
	// activeDurationFn is stamped onto Event.ActiveMs at AppendEvent
	// time; guarded by logPositionMu (same lifecycle — both are wired
	// once by the runview Service after the engine is ready). See
	// SetActiveDurationFn.
	activeDurationFn ActiveDurationFn
}

// LogPositionFn is the callback signature the store uses to stamp
// Event.LogOffset. Returns the byte position in the run's log
// buffer at the moment of invocation; 0 when no buffer exists yet
// for runID (early bootstrap events before the buffer is created).
type LogPositionFn func(runID string) int64

// ActiveDurationFn is the callback signature the store uses to stamp
// Event.ActiveMs. Returns the run's monotonic active duration in
// milliseconds (engine SharedBudget elapsed), or 0 when unknown (run
// not held by this process, or the workflow declares no budget). The
// value MUST be monotonic-derived (CLOCK_MONOTONIC, suspend-excluded),
// never recomputed from wall-clock timestamps.
type ActiveDurationFn func(runID string) int64

// StoreOption configures a FilesystemRunStore.
type StoreOption func(*FilesystemRunStore)

// WithLogger sets a leveled logger on the store.
func WithLogger(l *iterlog.Logger) StoreOption {
	return func(s *FilesystemRunStore) { s.logger = l }
}

// SetLogPositionFn installs (or replaces) the callback used by
// AppendEvent to stamp Event.LogOffset. Pass nil to disable stamping.
// Setter rather than constructor option because the per-run log
// buffer that backs the callback is created on demand by the runview
// Service AFTER the store is wired; the same Service instance
// installs the callback once it's ready.
func (s *FilesystemRunStore) SetLogPositionFn(fn LogPositionFn) {
	s.logPositionMu.Lock()
	s.logPositionFn = fn
	s.logPositionMu.Unlock()
}

// SetActiveDurationFn installs (or replaces) the callback AppendEvent
// uses to stamp Event.ActiveMs with the run's monotonic active duration
// (engine SharedBudget elapsed). Same lifecycle rationale as
// SetLogPositionFn — wired by the runview Service once the per-run
// engine is registered. Pass nil to disable stamping.
func (s *FilesystemRunStore) SetActiveDurationFn(fn ActiveDurationFn) {
	s.logPositionMu.Lock()
	s.activeDurationFn = fn
	s.logPositionMu.Unlock()
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
		root:           root,
		seq:            make(map[string]int64),
		seqSeed:        make(map[string]bool),
		inboxVersion:   make(map[string]uint64),
		eventsUnsynced: make(map[string]bool),
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

// CreateRun persists a new run with status "running". Captures the
// iterion-relevant launch env (model/effort/provider knobs) and
// iterion build version so the run record is reproducible later —
// without these, "why did the same recipe + same inputs produce
// different outputs across two daemon builds" is unanswerable.
//
// CreateRun is intentionally no-clobber: reusing a run ID must fail
// instead of resetting an existing run's metadata/checkpoint. Resume and
// crash-recovery code relies on run.json being the authoritative identity
// and checkpoint record for a run.
func (s *FilesystemRunStore) CreateRun(_ context.Context, id, workflowName string, inputs map[string]any) (*Run, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	r := &Run{
		FormatVersion:  RunFormatVersion,
		ID:             id,
		WorkflowName:   workflowName,
		Status:         RunStatusRunning,
		Inputs:         inputs,
		CreatedAt:      now,
		UpdatedAt:      now,
		LaunchEnv:      CaptureLaunchEnv(),
		IterionVersion: appinfo.FullVersion(),
	}
	if err := s.writeRunNew(r); err != nil {
		return nil, err
	}
	return r, nil
}

// CreateQueuedRun persists a new run with status "queued" — the state
// the pipeline board renders in its TODO lane while the run waits for a
// local concurrency slot. It mirrors CreateRun's exclusive-create
// contract (a reused run ID fails) but additionally stamps the file path
// and bot id so the local scheduler can compile + start the run later,
// and the board can render a meaningful card, without an in-memory
// launch spec. The engine's runResolveDoc transitions the doc
// queued→running on pickup, so no engine change is needed to start it.
//
// Implements store.QueuedRunCreator (filesystem-only; cloud stores
// already create runs queued via CreateRun).
func (s *FilesystemRunStore) CreateQueuedRun(_ context.Context, id, workflowName, filePath, botID string, inputs map[string]any) (*Run, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	r := &Run{
		FormatVersion:  RunFormatVersion,
		ID:             id,
		WorkflowName:   workflowName,
		FilePath:       filePath,
		BotID:          botID,
		Status:         RunStatusQueued,
		Inputs:         inputs,
		CreatedAt:      now,
		UpdatedAt:      now,
		QueuedAt:       &now,
		LaunchEnv:      CaptureLaunchEnv(),
		IterionVersion: appinfo.FullVersion(),
	}
	if err := s.writeRunNew(r); err != nil {
		return nil, err
	}
	return r, nil
}

// SaveRun persists the run metadata to disk. Protected by mu so it
// cannot race against UpdateRunStatus / SaveCheckpoint / WriteArtifact
// — two runners reconciling the same orphan via RecoverFinalize, or a
// finalize path concurrent with an engine status update, would
// otherwise read-modify-write through each other and lose fields.
func (s *FilesystemRunStore) SaveRun(_ context.Context, r *Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeRun(r)
}

// loadRunRaw is the pure-read variant of LoadRun: it parses run.json
// and returns the Run without firing the name backfill or
// finished_at heal. Used by every method that holds s.mu around its
// own read-modify-write — the public LoadRun's healing side-effects
// would otherwise sneak a second writeRun into a critical section
// the caller didn't account for (its own follow-up writeRun would
// then race the persisted state against its own in-memory copy).
func (s *FilesystemRunStore) loadRunRaw(id string) (*Run, error) {
	if err := sanitizePathComponent("run ID", id); err != nil {
		return nil, err
	}
	p := s.runJSONPath(id)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			// Wrap the shared sentinel (alongside the underlying
			// os.ErrNotExist the ReadFile error already carries) so callers
			// can distinguish genuine absence from a transient read failure
			// via errors.Is(err, ErrRunNotFound) — see ErrRunNotFound.
			return nil, fmt.Errorf("store: load run %s: %w: %w", id, ErrRunNotFound, err)
		}
		return nil, fmt.Errorf("store: load run %s: %w", id, err)
	}
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("store: decode run %s: %w", id, err)
	}
	return &r, nil
}

// healRun applies the on-read fixups (legacy-name backfill,
// finished_at sanity check) and returns true if a write is needed.
// Pure data manipulation; the caller decides when to persist.
func healRun(r *Run) bool {
	changed := false
	if r.Name == "" {
		r.Name = GenerateRunName(r.FilePath + ":" + r.ID)
		changed = true
	}
	if r.Status == RunStatusRunning && r.FinishedAt != nil {
		r.FinishedAt = nil
		changed = true
	}
	return changed
}

// loadRunHealBeforeLockHook is a test hook used to deterministically
// exercise the stale-read window before LoadRun's heal persistence enters the
// run-mutation critical section. Nil in production.
var loadRunHealBeforeLockHook func()

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
//
// Callers that already hold s.mu and intend to write the run
// themselves should use loadRunRaw to avoid the embedded writeRun
// from the heal path interleaving with their own write.
func (s *FilesystemRunStore) LoadRun(_ context.Context, id string) (*Run, error) {
	r, err := s.loadRunRaw(id)
	if err != nil {
		return nil, err
	}
	if healRun(r) {
		if loadRunHealBeforeLockHook != nil {
			loadRunHealBeforeLockHook()
		}
		// Persist heal-on-read under the same mutex as every other run.json
		// read-modify-write. The first load above may have observed a legacy
		// stale copy; before writing the heal, re-read the current on-disk run
		// while holding s.mu so a concurrent SaveCheckpoint/UpdateRunStatus/
		// SaveRun cannot have its authoritative fields clobbered by this
		// best-effort migration write.
		s.mu.Lock()
		fresh, reloadErr := s.loadRunRaw(id)
		if reloadErr == nil {
			if healRun(fresh) {
				if writeErr := s.writeRun(fresh); writeErr != nil && s.logger != nil {
					s.logger.Warn("store: heal-on-read for run %s failed: %v", id, writeErr)
				}
			}
			s.mu.Unlock()
			return fresh, nil
		}
		s.mu.Unlock()

		// Best-effort persist; a write/reload failure (read-only fs, racing
		// process, deleted run) leaves the in-memory heal applied and lets the
		// next successful write fix it up. Never fail LoadRun on this path.
		if s.logger != nil {
			s.logger.Warn("store: reload for heal-on-read for run %s failed: %v", id, reloadErr)
		}
	}
	return r, nil
}

// DeleteRun permanently removes a run's entire directory (run.json,
// events.jsonl, artifacts, interactions, attachments — everything under
// runs/<id>/). Idempotent: a missing run dir is not an error.
func (s *FilesystemRunStore) DeleteRun(_ context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("store: DeleteRun requires a run id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(s.root, "runs", id)); err != nil {
		return fmt.Errorf("store: delete run %s: %w", id, err)
	}
	return nil
}

// UpdateRunStatus updates the status (and optional error) of a run.
// Protected by mu to prevent concurrent read-modify-write races.
func (s *FilesystemRunStore) UpdateRunStatus(ctx context.Context, id string, status RunStatus, runErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return err
	}
	return s.applyStatusTransition(r, status, runErr)
}

// UpdateRunStatusIf is a compare-and-set on the status field: the
// write only lands when the current status is in expectedFrom. Used
// by callers that need to avoid racing with a concurrent transition
// (e.g. a Cancel firing while a Resume is republishing). Returns
// changed=true on a successful write, false if the status had
// drifted since the caller's last read.
func (s *FilesystemRunStore) UpdateRunStatusIf(ctx context.Context, id string, status RunStatus, runErr string, expectedFrom []RunStatus) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
	if err != nil {
		return false, err
	}
	matched := false
	for _, want := range expectedFrom {
		if r.Status == want {
			matched = true
			break
		}
	}
	if !matched {
		return false, nil
	}
	if err := s.applyStatusTransition(r, status, runErr); err != nil {
		return false, err
	}
	return true, nil
}

// applyStatusTransition is the shared tail of UpdateRunStatus and
// UpdateRunStatusIf: mutate r in-place (status, timestamps, terminal
// finished_at / resume FinishedAt clear, checkpoint clear when leaving
// paused state), then persist via writeRun. Caller must hold s.mu.
func (s *FilesystemRunStore) applyStatusTransition(r *Run, status RunStatus, runErr string) error {
	r.Status = status
	r.UpdatedAt = time.Now().UTC()
	r.Error = runErr
	switch status {
	case RunStatusFinished, RunStatusFailed, RunStatusFailedResumable, RunStatusCancelled:
		t := r.UpdatedAt
		r.FinishedAt = &t
	case RunStatusRunning, RunStatusPausedWaitingHuman:
		// Resume paths (failed_resumable/cancelled → running) must clear
		// FinishedAt — otherwise the studio's duration ticker uses the
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
func (s *FilesystemRunStore) SaveCheckpoint(ctx context.Context, id string, cp *Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
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
func (s *FilesystemRunStore) PauseRun(ctx context.Context, id string, cp *Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
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
func (s *FilesystemRunStore) FailRunResumable(ctx context.Context, id string, cp *Checkpoint, runErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(id)
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

// AddWatchedIssues merges issueIDs into the run's WatchedIssueIDs set
// (dedup, insertion order preserved) and returns the resulting set.
func (s *FilesystemRunStore) AddWatchedIssues(_ context.Context, runID string, issueIDs []string) ([]string, error) {
	return s.mutateWatched(runID, func(cur []string) []string { return mergeWatchedIssues(cur, issueIDs) })
}

// RemoveWatchedIssues drops issueIDs from the run's WatchedIssueIDs set
// and returns the resulting set.
func (s *FilesystemRunStore) RemoveWatchedIssues(_ context.Context, runID string, issueIDs []string) ([]string, error) {
	return s.mutateWatched(runID, func(cur []string) []string { return removeWatchedIssues(cur, issueIDs) })
}

// mutateWatched applies apply to the run's WatchedIssueIDs under mu —
// parallel branches' onNodeFinished hooks and the watch API can call
// this concurrently.
func (s *FilesystemRunStore) mutateWatched(runID string, apply func([]string) []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.loadRunRaw(runID)
	if err != nil {
		return nil, err
	}
	r.WatchedIssueIDs = apply(r.WatchedIssueIDs)
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRun(r); err != nil {
		return nil, err
	}
	return r.WatchedIssueIDs, nil
}

// ListRuns returns the IDs of all persisted runs.
func (s *FilesystemRunStore) ListRuns(_ context.Context) ([]string, error) {
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

// ListRunsBySourceIssue returns the ids of runs whose Source.IssueID
// equals issueID (the card←run reverse edge), sorted by created_at
// ascending. At local scale we scan ListRuns + LoadRun each and filter
// — mirroring how the other fs-side filters work (runview.List) — since
// the filesystem store has no secondary index. Runs that fail to load
// are skipped rather than failing the whole query.
func (s *FilesystemRunStore) ListRunsBySourceIssue(ctx context.Context, issueID string) ([]string, error) {
	if issueID == "" {
		return []string{}, nil
	}
	return s.filterRunsSorted(ctx, func(r *Run) bool {
		return r.Source != nil && r.Source.IssueID == issueID
	})
}

// ListChildRuns returns the ids of runs whose ParentRunID equals
// parentRunID (a run's shard/child subtree), sorted by created_at
// ascending. Same scan-and-filter strategy as ListRunsBySourceIssue.
func (s *FilesystemRunStore) ListChildRuns(ctx context.Context, parentRunID string) ([]string, error) {
	if parentRunID == "" {
		return []string{}, nil
	}
	return s.filterRunsSorted(ctx, func(r *Run) bool {
		return r.ParentRunID == parentRunID
	})
}

// filterRunsSorted scans every run, keeps those matching pred, and
// returns their ids sorted by CreatedAt ascending. Shared by the
// fs-side reverse-tree queries.
func (s *FilesystemRunStore) filterRunsSorted(ctx context.Context, pred func(*Run) bool) ([]string, error) {
	ids, err := s.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	type match struct {
		id string
		at time.Time
	}
	var matches []match
	for _, id := range ids {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("store: skip run %s during reverse-query scan: %v", id, err)
			}
			continue
		}
		if pred(r) {
			matches = append(matches, match{id: r.ID, at: r.CreatedAt})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].at.Before(matches[j].at)
	})
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.id)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// AppendEvent appends an event to the run's events.jsonl.
// Seq and Timestamp are set automatically.
// The entire operation is serialized under mu to prevent interleaved writes
// from concurrent branches. The sequence counter is only incremented after
// a successful write to avoid gaps in the event stream.
func (s *FilesystemRunStore) AppendEvent(_ context.Context, runID string, evt Event) (*Event, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
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
		if err != nil && s.logger != nil {
			s.logger.Warn("store: partial seq seed for run %s: %v (resuming from %d — best-effort)", runID, err, next)
		}
		s.seqSeed[runID] = true
	}

	// Assign seq but don't increment the counter yet — only advance on
	// successful write to prevent gaps from failed marshals or I/O.
	evt.Seq = s.seq[runID]

	// Stamp the current log-buffer byte position when the runview
	// Service has wired a callback; lets the studio's time-travel
	// scrubber slice "log up to event seq N" without parsing log
	// line timestamps. Only overwrites when the caller didn't set
	// LogOffset explicitly (Mongo-mode replays / synthetic test
	// events can pre-fill).
	if evt.LogOffset == 0 {
		s.logPositionMu.RLock()
		fn := s.logPositionFn
		s.logPositionMu.RUnlock()
		if fn != nil {
			evt.LogOffset = fn(runID)
		}
	}

	// Stamp the run's monotonic active duration (engine SharedBudget
	// elapsed) so the studio can display suspend-excluded active time
	// instead of re-deriving it from wall-clock event windows. Only
	// when the caller didn't pre-fill it (Mongo replays / test events).
	if evt.ActiveMs == 0 {
		s.logPositionMu.RLock()
		afn := s.activeDurationFn
		s.logPositionMu.RUnlock()
		if afn != nil {
			evt.ActiveMs = afn(runID)
		}
	}

	line, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("store: marshal event: %w", err)
	}
	line = append(line, '\n')

	p := s.eventsPath(runID)
	if err := os.MkdirAll(filepath.Dir(p), dirPerm); err != nil {
		return nil, fmt.Errorf("store: mkdir events: %w", err)
	}

	// O_RDWR (not O_WRONLY): the torn-tail repair below ReadAt's the final
	// byte to detect a partial last line left by a prior crash, and ReadAt on
	// a write-only descriptor returns EBADF. O_APPEND still forces every Write
	// to EOF regardless of the read offset, so append semantics are unchanged.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_APPEND, filePerm)
	if err != nil {
		return nil, fmt.Errorf("store: open events: %w", err)
	}
	defer f.Close()
	// Tighten permissions on legacy files that may have been created before
	// store-wide filePerm was enforced. events.jsonl carries prompts, model
	// outputs, and tool data.
	if err := f.Chmod(filePerm); err != nil {
		return nil, fmt.Errorf("store: chmod events: %w", err)
	}

	// Capture the pre-write size so a short write (typically ENOSPC
	// mid-line) can be rolled back. Without this, a partial JSON line
	// is left in events.jsonl and the next append would silently
	// concatenate a valid line onto the truncated one — every
	// downstream scanner (LoadEvents, ScanEvents, replay) then trips
	// on a corrupted record forever after.
	info, statErr := f.Stat()
	var preSize int64
	if statErr == nil {
		preSize = info.Size()
	}
	if statErr == nil && preSize > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, preSize-1); err != nil {
			return nil, fmt.Errorf("store: inspect events tail: %w", err)
		}
		if last[0] != '\n' {
			// A previous process crashed after writing only part of the final
			// JSONL record. Separate the torn bytes from the next valid event so
			// scanners skip exactly the corrupt tail line instead of losing the
			// first post-crash event to concatenation. This runs under s.mu, so
			// sequence seeding and repair are atomic within this process.
			if _, err := f.Write([]byte("\n")); err != nil {
				return nil, fmt.Errorf("store: separate torn event tail: %w", err)
			}
			preSize++
		}
	}

	n, writeErr := f.Write(line)
	if writeErr != nil || n != len(line) {
		// Best-effort truncate to the captured pre-write size so the
		// file stays JSONL-clean. We only have a captured size when
		// Stat succeeded above; if it didn't, leaving the partial line
		// in place is the lesser evil (truncating to a guessed offset
		// could discard prior good lines).
		if statErr == nil {
			_ = f.Truncate(preSize)
		}
		if writeErr != nil {
			return nil, fmt.Errorf("store: write event: %w", writeErr)
		}
		return nil, fmt.Errorf("store: short write on event (wrote %d of %d bytes)", n, len(line))
	}

	// Best-effort fsync. The bytes are already in the file as far as
	// future appends are concerned (O_APPEND is atomic per syscall);
	// fsync only adds the durability guarantee. Treating a transient
	// fsync failure as fatal used to leave the in-memory seq counter
	// pinned at evt.Seq, so the next AppendEvent would assign the same
	// Seq to a different event and produce duplicate sequence numbers
	// in the file. Log instead, advance seq, and flag the run so the
	// next run.json write re-syncs the events file first (writeRun's
	// ordering barrier) — the checkpoint must never be durably ahead
	// of its event log.
	if err := f.Sync(); err != nil {
		s.eventsUnsynced[runID] = true
		if s.logger != nil {
			s.logger.Warn("store: fsync event for run %s seq %d: %v — line written but not durable", runID, evt.Seq, err)
		}
	} else {
		delete(s.eventsUnsynced, runID)
	}

	// Always advance once the bytes are in the file. fsync confirms
	// durability, not presence.
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
func (s *FilesystemRunStore) ScanEvents(_ context.Context, runID string, visit func(*Event) bool) error {
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
	var skipped, valid int
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
		valid++
		if !visit(evt) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("store: scan events: %w", err)
	}
	if skipped > 0 && s.logger != nil {
		s.logger.Warn("skipped %d corrupt event line(s) in run %s (valid=%d)", skipped, runID, valid)
	}
	if eventsCorruptionExceeded(skipped, valid) {
		return fmt.Errorf("%w: run %s, skipped=%d valid=%d", ErrEventsCorrupted, runID, skipped, valid)
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
func (s *FilesystemRunStore) LoadEventsRange(ctx context.Context, runID string, from, to int64, limit int) ([]*Event, error) {
	var out []*Event
	if limit > 0 {
		out = make([]*Event, 0, limit)
	}
	err := s.ScanEvents(ctx, runID, func(e *Event) bool {
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
//
// Delegates to ScanEvents: identical open/scan/decode/skip-corrupt semantics
// (corruption threshold, partial-result return) are preserved because
// ScanEvents populates events via the visit callback before returning the
// ErrEventsCorrupted error.
func (s *FilesystemRunStore) LoadEvents(ctx context.Context, runID string) ([]*Event, error) {
	var events []*Event
	err := s.ScanEvents(ctx, runID, func(e *Event) bool {
		events = append(events, e)
		return true
	})
	return events, err
}

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------

// WriteArtifact persists an artifact for a node at the given version and
// updates the run's artifact index for O(1) latest-version lookups.
func (s *FilesystemRunStore) WriteArtifact(ctx context.Context, a *Artifact) error {
	if err := sanitizePathComponent("run ID", a.RunID); err != nil {
		return err
	}
	if err := sanitizePathComponent("node ID", a.NodeID); err != nil {
		return err
	}
	if a.WrittenAt.IsZero() {
		a.WrittenAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal artifact: %w", err)
	}
	dir := filepath.Join(s.root, "runs", a.RunID, "artifacts", a.NodeID)
	p := filepath.Join(dir, fmt.Sprintf("%d.json", a.Version))

	// Hold s.mu across the artifact file write AND the index update so
	// the on-disk file and the cached pointer in run.json land together.
	// Without this, a concurrent LoadRun/SaveRun could observe an index
	// that points to a version not yet on disk (or miss one already
	// written), and a crash between the two writes would leave the
	// artifact orphan to a directory-scan fallback every read.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir artifact: %w", err)
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
	r, err := s.loadRunRaw(a.RunID)
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
func (s *FilesystemRunStore) LoadArtifact(_ context.Context, runID, nodeID string, version int) (*Artifact, error) {
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
func (s *FilesystemRunStore) LoadLatestArtifact(ctx context.Context, runID, nodeID string) (*Artifact, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	if err := sanitizePathComponent("node ID", nodeID); err != nil {
		return nil, err
	}

	// Fast path: use artifact index if available.
	if r, err := s.LoadRun(ctx, runID); err == nil && r.ArtifactIndex != nil {
		if v, ok := r.ArtifactIndex[nodeID]; ok {
			return s.LoadArtifact(ctx, runID, nodeID, v)
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
	return s.LoadArtifact(ctx, runID, nodeID, maxVersion)
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
func (s *FilesystemRunStore) ListArtifactVersions(_ context.Context, runID, nodeID string) ([]ArtifactVersionInfo, error) {
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
func (s *FilesystemRunStore) WriteInteraction(_ context.Context, i *Interaction) error {
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

// ---------------------------------------------------------------------------
// Run files (tool-produced artifact files surfaced via the studio)
// ---------------------------------------------------------------------------

// runFilesDir returns the per-run scratch directory where tool-produced
// artifact files live. The path is bind-mounted into the sandbox at
// ITERION_ARTIFACT_FILES_DIR so in-container tools can write files
// without going through the worktree (and committing them into the
// bench repo).
func (s *FilesystemRunStore) runFilesDir(runID string) string {
	return filepath.Join(s.runDir(runID), "artifact_files")
}

// EnsureRunFilesDir satisfies RunFilesStore. Idempotent.
func (s *FilesystemRunStore) EnsureRunFilesDir(_ context.Context, runID string) (string, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return "", err
	}
	runDir := s.runDir(runID)
	if err := os.MkdirAll(runDir, dirPerm); err != nil {
		return "", fmt.Errorf("store: mkdir run dir: %w", err)
	}
	if err := os.Chmod(runDir, dirPerm); err != nil {
		return "", fmt.Errorf("store: chmod run dir: %w", err)
	}

	dir := s.runFilesDir(runID)
	if err := ensurePlainDirNoSymlink(dir, dirPerm); err != nil {
		return "", fmt.Errorf("store: mkdir run files dir: %w", err)
	}
	// Loosen perms so the in-sandbox container user (devbox, typically
	// uid 1000) can write here even when the host daemon owner is also
	// uid 1000 — explicit 0o775 + setting the dir as group-writable
	// covers the common case and keeps a future "container user is
	// 1001" deployment from silently failing the bind-mount writes.
	_ = os.Chmod(dir, 0o775)
	return dir, nil
}

// ensurePlainDirNoSymlink creates dir but refuses to treat an existing
// symlink as success. os.MkdirAll follows a final symlink to a directory,
// which would let a sandbox-controlled artifact_files entry redirect the
// bind-mount/output area outside the run; replace that stale symlink with a
// real directory instead.
func ensurePlainDirNoSymlink(dir string, perm os.FileMode) error {
	info, err := os.Lstat(dir)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(dir); err != nil {
				return err
			}
		} else {
			if !info.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", dir)
			}
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(dir, perm); err != nil {
		if os.IsExist(err) {
			info, lstatErr := os.Lstat(dir)
			if lstatErr != nil {
				return lstatErr
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s exists as symlink", dir)
			}
			if !info.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", dir)
			}
			return nil
		}
		return err
	}
	return nil
}

// cleanRunFilePath validates a user-supplied artifact file path before the
// openat walk. Reject escapes lexically instead of normalising them away:
// "a/../run.json" must be denied, not collapsed to "run.json".
func cleanRunFilePath(relPath string) ([]string, string, error) {
	slashPath := strings.ReplaceAll(relPath, "\\", "/")
	if slashPath == "" || path.IsAbs(slashPath) {
		return nil, "", fmt.Errorf("store: run file not found")
	}
	for _, part := range strings.Split(slashPath, "/") {
		if part == ".." {
			return nil, "", fmt.Errorf("store: run file not found")
		}
	}
	cleaned := path.Clean(slashPath)
	if cleaned == "." {
		return nil, "", fmt.Errorf("store: run file not found")
	}
	components := strings.Split(cleaned, "/")
	for _, part := range components {
		if part == "" || part == "." || part == ".." {
			return nil, "", fmt.Errorf("store: run file not found")
		}
	}
	return components, cleaned, nil
}

// ListRunFiles satisfies RunFilesStore. Returns a sorted slice (by path)
// for stable output; empty (no error) when no files exist.
func (s *FilesystemRunStore) ListRunFiles(_ context.Context, runID string) ([]RunFileInfo, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, err
	}
	root := s.runFilesDir(runID)
	var out []RunFileInfo
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return filepath.SkipAll
			}
			return walkErr
		}
		// artifact_files is writable by sandboxed tools. Never surface a
		// symlink as a produced file: d.Info may follow a link to a regular
		// file, while OpenRunFile correctly refuses that same path, yielding a
		// misleading manifest entry (and a dead review attachment).
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, RunFileInfo{
			Path:       filepath.ToSlash(rel),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list run files: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// OpenRunFile satisfies RunFilesStore. Implements path-traversal
// protection by walking from an already-open artifact_files directory fd.
// Absolute paths and any `..` component are rejected lexically, every
// intermediate component is opened with O_DIRECTORY|O_NOFOLLOW, and the
// final file is opened with O_NOFOLLOW before fstat. That openat-style walk
// avoids the EvalSymlinks-then-open TOCTOU gap where a sandbox-controlled
// artifact_files tree could swap an intermediate directory for a symlink
// after validation and trick the server into streaming an arbitrary host file.
func (s *FilesystemRunStore) OpenRunFile(_ context.Context, runID, relPath string) (io.ReadCloser, RunFileInfo, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, RunFileInfo{}, err
	}
	components, cleaned, err := cleanRunFilePath(relPath)
	if err != nil {
		return nil, RunFileInfo{}, err
	}
	root := s.runFilesDir(runID)
	f, info, err := openRunFileAt(root, components)
	if err != nil {
		return nil, RunFileInfo{}, fmt.Errorf("store: run file not found")
	}
	return f, RunFileInfo{
		Path:       filepath.ToSlash(cleaned),
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC(),
	}, nil
}

// ---------------------------------------------------------------------------
// Tool blobs (per-tool-call I/O bodies, sidecar to events.jsonl)
// ---------------------------------------------------------------------------

// toolBlobPath returns runs/<id>/tools/<toolUseID>/<kind>. Caller has
// already sanitised runID + toolUseID + kind ∈ {"input", "output"}.
func (s *FilesystemRunStore) toolBlobPath(runID, toolUseID, kind string) string {
	return filepath.Join(s.runDir(runID), "tools", toolUseID, kind)
}

// validateToolBlobKind rejects anything other than "input" or "output"
// so the path component is always a known literal — no traversal risk
// from a network-sourced kind value.
func validateToolBlobKind(kind string) error {
	if kind != "input" && kind != "output" {
		return fmt.Errorf("store: tool blob kind must be input|output, got %q", kind)
	}
	return nil
}

// WriteToolBlob satisfies ToolBlobStore. Writes atomically; returns the
// total byte size persisted. Idempotent — re-writing the same key
// replaces the prior bytes.
func (s *FilesystemRunStore) WriteToolBlob(_ context.Context, runID, toolUseID, kind string, body []byte) (int64, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return 0, err
	}
	if err := sanitizePathComponent("tool_use_id", toolUseID); err != nil {
		return 0, err
	}
	if err := validateToolBlobKind(kind); err != nil {
		return 0, err
	}
	dir := filepath.Dir(s.toolBlobPath(runID, toolUseID, kind))
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return 0, fmt.Errorf("store: mkdir tool blob dir: %w", err)
	}
	if err := writeFileAtomic(s.toolBlobPath(runID, toolUseID, kind), body, filePerm); err != nil {
		return 0, fmt.Errorf("store: write tool blob: %w", err)
	}
	return int64(len(body)), nil
}

// ReadToolBlob satisfies ToolBlobStore. limit == 0 means "all from
// offset". Returns the bytes read, the full blob size, and eof when
// offset+len(data) == total. Missing blob → wrapped os.ErrNotExist.
func (s *FilesystemRunStore) ReadToolBlob(_ context.Context, runID, toolUseID, kind string, offset, limit int64) ([]byte, int64, bool, error) {
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return nil, 0, false, err
	}
	if err := sanitizePathComponent("tool_use_id", toolUseID); err != nil {
		return nil, 0, false, err
	}
	if err := validateToolBlobKind(kind); err != nil {
		return nil, 0, false, err
	}
	if offset < 0 {
		offset = 0
	}
	p := s.toolBlobPath(runID, toolUseID, kind)
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, false, fmt.Errorf("store: open tool blob: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, false, fmt.Errorf("store: stat tool blob: %w", err)
	}
	total := st.Size()
	if offset >= total {
		return nil, total, true, nil
	}
	remaining := total - offset
	readLen := remaining
	if limit > 0 && limit < readLen {
		readLen = limit
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, total, false, fmt.Errorf("store: seek tool blob: %w", err)
	}
	buf := make([]byte, readLen)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, total, false, fmt.Errorf("store: read tool blob: %w", err)
	}
	eof := offset+readLen >= total
	return buf, total, eof, nil
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
	if err := sanitizePathComponent("run ID", runID); err != nil {
		return 0, err
	}
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

func (s *FilesystemRunStore) writeRunNew(r *Run) error {
	// Defence in depth for CreateRun's exclusive create path: sanitise here as
	// well as at the public entry point so future internal callers cannot path
	// join a tampered Run.ID outside the store root.
	if err := sanitizePathComponent("run ID", r.ID); err != nil {
		return err
	}
	dir := s.runDir(r.ID)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: mkdir run: %w", err)
	}
	// Tighten existing directories (MkdirAll is a no-op on them). A stale or
	// pre-created run directory must not remain world-readable.
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("store: chmod run: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal run: %w", err)
	}
	if err := WriteFileAtomicNew(s.runJSONPath(r.ID), data, filePerm); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("store: run %s already exists: %w", r.ID, fs.ErrExist)
		}
		return err
	}
	return nil
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
	if err := os.Chmod(dir, dirPerm); err != nil {
		return fmt.Errorf("store: chmod run: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal run: %w", err)
	}
	// Write-ahead ordering barrier: if an earlier AppendEvent fsync
	// failed, re-sync events.jsonl BEFORE the checkpoint write. run.json
	// is written atomically with fsync, so without the barrier a power
	// loss could recover a checkpoint that references events which never
	// reached disk. Failing here is consistent with writeFileAtomic's
	// own hard-fail on fsync: if the disk can't persist the log, it
	// can't persist the checkpoint either.
	if s.eventsUnsynced[r.ID] {
		if err := s.syncEventsLocked(r.ID); err != nil {
			return fmt.Errorf("store: write run %s blocked — events.jsonl re-sync after an earlier fsync failure: %w", r.ID, err)
		}
	}
	// Atomic write: run.json is the authoritative resume checkpoint
	// (per CLAUDE.md). A torn write would lose all prior checkpoint state.
	return writeFileAtomic(s.runJSONPath(r.ID), data, filePerm)
}

// syncEventsLocked fsyncs the run's events.jsonl and clears its
// unsynced flag. Called under s.mu. A missing events file counts as
// synced (nothing to persist — the failed append may have been the
// file's first line on a path that never materialised).
func (s *FilesystemRunStore) syncEventsLocked(runID string) error {
	f, err := os.OpenFile(s.eventsPath(runID), os.O_RDWR, filePerm)
	if err != nil {
		if os.IsNotExist(err) {
			delete(s.eventsUnsynced, runID)
			return nil
		}
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return err
	}
	delete(s.eventsUnsynced, runID)
	return nil
}
