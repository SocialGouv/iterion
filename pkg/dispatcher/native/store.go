// Package native implements iterion's first-class issue/kanban tracker.
// Issues live as one JSON file per issue under <root>/issues/, a board
// config sits at <root>/board.json, and every mutation appends a
// monotonically-sequenced record to <root>/events.jsonl. All writes are
// serialized through a single mutex; reads scan the filesystem.
package native

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

const (
	boardFile  = "board.json"
	issuesDir  = "issues"
	eventsFile = "events.jsonl"

	dirPerm  fs.FileMode = 0o755
	filePerm fs.FileMode = 0o644
)

// Store is the filesystem-backed native tracker store. Safe for
// concurrent use.
type Store struct {
	root string

	mu    sync.Mutex
	board *Board
	seq   int64

	// logger carries store diagnostics (watcher errors, …). Defaults to a
	// warn-level stderr logger — stderr is safe even in the `iterion
	// __mcp-board` stdio subprocess, whose protocol channel is stdout.
	// Replaceable via SetLogger.
	logger *iterlog.Logger

	// index is a hot in-memory mirror of issues/<id>.json. Filesystem
	// remains authoritative — index is populated at NewStore and kept
	// in sync on every write. List + Get walk the index instead of
	// hitting the filesystem, so a board with hundreds of issues
	// doesn't pay N file reads per query.
	index map[string]*Issue

	// watcher mirrors out-of-process writes (e.g. the `iterion
	// __mcp-board` stdio MCP subprocess) into the index. nil when
	// fsnotify isn't available on the host — the Store still works,
	// it just can't see writes by other processes, which is the
	// pre-watcher status quo.
	watcher *indexWatcher

	// pendingEvents buffers events whose appendEventLocked call
	// returned an error (transient fsync failure, NFS hiccup). Every
	// subsequent successful event flush drains the buffer first so a
	// downstream tailer eventually sees every state transition. State
	// recovery via populateIndex doesn't depend on events.jsonl, so
	// holding the buffer in memory is safe across the failure window.
	pendingEvents []Event

	// commentDispatcher, when set, lets a comment whose body leads with a
	// "/command" launch a bot — the native/local twin of the forge
	// issue-comment trigger. handleAddComment consults it for any comment the
	// request didn't already resolve (no explicit bot/bot_args). The resolver —
	// a server closure — does the command→bot lookup + the open_mr /
	// source_issue_ref stamp, keeping the store decoupled from the bot registry.
	commentDispatcher CommentDispatcher
}

// CommentDispatcher resolves a board-issue comment that leads with a "/command"
// into a bot launch: the bot to assign, the per-run bot_args (including the
// open_mr / source_issue_ref stamp for an opens-MR command), and the
// dispatch-eligible state to move the issue to. ok=false means "just record the
// comment, launch nothing". Installed by the server via SetCommentDispatcher;
// nil in a bare store (a plain `iterion dispatch` daemon or a unit test), where
// the comment is recorded with no dispatch — exactly the prior behaviour.
type CommentDispatcher func(iss Issue, commentBody string) (bot string, botArgs map[string]string, transitionTo string, ok bool)

// SetCommentDispatcher installs the slash-command resolver consulted by the
// POST /issues/{id}/comments handler. Called once at wiring time.
func (s *Store) SetCommentDispatcher(d CommentDispatcher) {
	s.mu.Lock()
	s.commentDispatcher = d
	s.mu.Unlock()
}

// getCommentDispatcher returns the installed resolver (nil if none) under the
// store lock, so a wiring-time SetCommentDispatcher races cleanly with serving.
func (s *Store) getCommentDispatcher() CommentDispatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commentDispatcher
}

// NewStore opens (or initializes) the native tracker at root. If
// board.json is absent a default board is written.
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("native store: root path required")
	}
	if err := os.MkdirAll(filepath.Join(root, issuesDir), dirPerm); err != nil {
		return nil, fmt.Errorf("native store: mkdir: %w", err)
	}
	s := &Store{
		root:   root,
		index:  map[string]*Issue{},
		logger: iterlog.NewFallback(iterlog.LevelWarn, os.Stderr),
	}
	if err := s.loadOrInitBoard(); err != nil {
		return nil, err
	}
	// Seed the event sequence counter from any existing log so a
	// fresh process opening a pre-existing store doesn't restart Seq
	// at 0 and produce duplicate sequence numbers in events.jsonl.
	// Torn tails / malformed lines are skipped inside ScanEvents, so
	// an error here is a real I/O failure — abort rather than restart
	// Seq at 0 and emit duplicate sequence numbers.
	var maxSeq int64 = -1
	if err := s.ScanEvents(func(e *Event) bool {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
		return true
	}); err != nil {
		return nil, fmt.Errorf("native: recover max seq from events.jsonl: %w", err)
	}
	s.seq = maxSeq + 1

	// Populate the index from disk. Corrupt files are skipped (a
	// warning would be nice but the store doesn't carry a logger).
	if err := s.populateIndex(); err != nil {
		return nil, err
	}

	// Start the fsnotify watcher AFTER the initial index population
	// so the watcher can never overwrite a fresh load with a stale
	// disk snapshot. A failure here is non-fatal — the Store keeps
	// working, just blind to out-of-process writes (the historical
	// behaviour). We don't log because the package carries no logger
	// today; the missing-watcher symptom (stale board reads) is
	// already documented as a known mode in the cache-desync finding.
	if w, err := startIndexWatcher(s); err == nil {
		s.watcher = w
	}
	return s, nil
}

// SetLogger replaces the store's diagnostic logger (default: warn-level
// to stderr). Lets studio/dispatch plumb their configured logger in.
// Nil is ignored so callers never disable diagnostics by accident.
func (s *Store) SetLogger(l *iterlog.Logger) {
	if l == nil {
		return
	}
	s.mu.Lock()
	s.logger = l
	s.mu.Unlock()
}

// getLogger returns the current diagnostic logger under the store lock,
// so the watcher goroutine races cleanly with a wiring-time SetLogger.
func (s *Store) getLogger() *iterlog.Logger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logger
}

// Close releases store-owned resources (currently the fsnotify
// watcher goroutine). Safe to call multiple times; safe on a Store
// whose watcher never started.
func (s *Store) Close() error {
	if s == nil || s.watcher == nil {
		return nil
	}
	return s.watcher.Close()
}

func (s *Store) populateIndex() error {
	entries, err := os.ReadDir(filepath.Join(s.root, issuesDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("native store: scan issues: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		id := decodeID(strings.TrimSuffix(e.Name(), ".json"))
		iss, err := s.readIssueFromDisk(id)
		if err != nil {
			continue
		}
		s.index[id] = iss
	}
	return nil
}

// readIssueFromDisk bypasses the index — used only at NewStore to
// populate the cache from the authoritative on-disk files. Post-init
// reads should go through the index via readIssueLocked.
func (s *Store) readIssueFromDisk(id string) (*Issue, error) {
	data, err := os.ReadFile(s.issuePath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, tracker.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("native store: read issue: %w", err)
	}
	var iss Issue
	if err := json.Unmarshal(data, &iss); err != nil {
		return nil, fmt.Errorf("native store: parse issue %s: %w", id, err)
	}
	return &iss, nil
}

// Root returns the on-disk root directory.
func (s *Store) Root() string { return s.root }

// recoverMutator wraps a Store mutator in defer-recover. A panic
// during disk I/O, index mutation, or event emission would otherwise
// take down the dispatcher process; here we reload the index from disk
// so any partially-applied in-memory state is replaced with the
// canonical on-disk view, and surface the panic as a returned error
// so the caller (HTTP handler, MCP tool, etc.) reports it instead of
// crashing.
func (s *Store) recoverMutator(name string, err *error) {
	r := recover()
	if r == nil {
		return
	}
	// Best-effort: drop the in-memory index and rebuild from disk so
	// later reads don't see a half-mutated state. A reload failure
	// here is folded into the returned error so the caller knows the
	// store is in a degraded state and the process should probably
	// be restarted to recover.
	s.index = map[string]*Issue{}
	if reloadErr := s.populateIndex(); reloadErr != nil {
		*err = fmt.Errorf("native store: %s panicked (%v) and index reload failed (%v) — restart recommended", name, r, reloadErr)
		return
	}
	*err = fmt.Errorf("native store: %s panicked: %v", name, r)
}
