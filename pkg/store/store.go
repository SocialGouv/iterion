package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

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
	// Best-effort: drop a self-ignoring .gitignore so the store dir is
	// never accidentally committed.
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
