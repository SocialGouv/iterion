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
	"sort"
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
	// __mcp-board` stdio MCP subprocess) into the index. nil when the
	// host refused a watch — the rescanner below then carries the same
	// promise on a slower path.
	watcher *indexWatcher

	// watcherErr records why the watch could not be armed. From the
	// outside an absent watcher is a nil field either way, so without
	// this a host that refused the watch (ENOSPC at max_user_watches,
	// EMFILE at max_user_instances) and a regression that stops the
	// watcher starting are the same observation.
	watcherErr error

	// rescanner is the reconciliation net that replaces the watcher
	// when the host refused it. Without it the index is frozen at its
	// NewStore snapshot for the life of the process. nil when a watch
	// was armed (the fast path needs no net) or when the net is off.
	rescanner *indexRescanner

	// scanning is >0 while Reconcile's disk scan is in flight, and dirty
	// collects the ids the process wrote meanwhile (every file write
	// through writeIssueLocked, every Delete) — both under mu. The scan
	// runs WITHOUT the mutex, so a write that lands during it is one the
	// scan may or may not have seen; the swap takes the index's own value
	// for those ids instead of the scan's, and never reverts one.
	scanning int
	dirty    map[string]bool

	// unreadableFP fingerprints the set of cards the last scan could not
	// read, so the warning is written when the set CHANGES, not on every
	// tick of a net that runs every two seconds.
	unreadableFP string

	// closed is set by Close under mu; a watch lost after that arms no net.
	closed bool

	// rebuildMu guards the coalescing state for the rebuilds a
	// kernel-queue overflow asks for: one rebuild runs at a time, and
	// every request that arrives while it runs collapses into exactly
	// one FOLLOW-UP pass. rebuildAgain is what makes that a deferral
	// rather than a drop — the in-flight scan's ReadDir predates any
	// overflow that arrives after it, so it cannot cover one.
	rebuildMu      sync.Mutex
	rebuildRunning bool
	rebuildAgain   bool

	// reconcileMu serialises Reconcile callers (the rescan ticker, a
	// kernel-queue overflow, an explicit call) so two scans cannot
	// overlap and swap in the older one last. Never held with mu by the
	// same goroutine except in the order reconcileMu → mu.
	reconcileMu sync.Mutex

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
	// disk snapshot. A failure here is not fatal, but it is not
	// nothing either: a refused watch must not silently become "this
	// store is blind until the daemon restarts". Record why, say so,
	// and fall back to the reconciliation net so the store's promise
	// survives a host that has no watch descriptor to give.
	// startIndexWatcher publishes the watcher on the store itself, under
	// the lock and before its loop runs.
	if _, err := startIndexWatcher(s); err != nil {
		s.mu.Lock()
		s.watcherErr = err
		s.rescanner = startFallbackRescan(s)
		r := s.rescanner
		s.mu.Unlock()
		if r != nil {
			s.logger.Warn("native index watcher unavailable: %v — falling back to a %s disk rescan for out-of-process issue changes", err, r.interval)
		} else {
			s.logger.Warn("native index watcher unavailable: %v, and the rescan net is disabled (ITERION_NATIVE_INDEX_RESCAN=off) — out-of-process issue changes will not be seen until restart", err)
		}
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
	if s == nil {
		return nil
	}
	// Snapshot under the lock — a watch lost mid-life swaps these from the
	// watcher goroutine — and close outside it: closing the watcher waits
	// for its loop, which may itself be waiting for the store mutex. The
	// closed flag stops a loss detected after this point from arming a
	// net; one that was armed between the snapshot and the watcher's exit
	// is picked up by the second look.
	s.mu.Lock()
	s.closed = true
	rescanner, watcher := s.rescanner, s.watcher
	s.mu.Unlock()
	if rescanner != nil {
		_ = rescanner.Close()
	}
	var err error
	if watcher != nil {
		err = watcher.Close()
	}
	s.mu.Lock()
	late := s.rescanner
	s.mu.Unlock()
	if late != nil && late != rescanner {
		_ = late.Close()
	}
	return err
}

// populateIndex loads every committed issue file into the index at
// NewStore. It adds to the index rather than replacing it; the full
// rebuild that also drops vanished files is Reconcile. A file that cannot
// be read is skipped, and said so.
func (s *Store) populateIndex() error {
	fresh, unreadable, err := s.scanIssues()
	if err != nil {
		return err
	}
	for id, iss := range fresh {
		s.index[id] = iss
	}
	if len(unreadable) > 0 && s.logger != nil {
		// Name the FIRST id by sort order, not whichever one Go's
		// randomised map iteration handed us: two restarts over the same
		// broken tree must blame the same card, or the warning cannot be
		// grepped for or compared across a restart. swapIndexLocked
		// already reports its own example this way.
		ids := make([]string, 0, len(unreadable))
		for id := range unreadable {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		s.logger.Warn("native store: %d issue file(s) could not be read at startup and were skipped (e.g. %s: %v)", len(ids), ids[0], unreadable[ids[0]])
	}
	return nil
}

// errWatchLost is recorded on a store whose armed watch went away
// mid-life: the kernel dropped it (issues/ removed, renamed or unmounted
// — fsnotify forgets the watch and says nothing), or fsnotify closed its
// channels under the loop.
var errWatchLost = errors.New("inotify watch on issues/ lost mid-life")

// watchLost is called by the watcher loop once it has established that
// its watch is gone: the fast path is over, so the store arms the same
// net a watch refused at startup gets, with the reason recorded, instead
// of serving its last snapshot until restart. The fsnotify watcher is
// closed here — its inotify descriptor is the scarce thing on the host
// this happens on — and a store that is closing arms nothing.
func (s *Store) watchLost(iw *indexWatcher) {
	s.mu.Lock()
	if s.watcher != iw || s.closed {
		s.mu.Unlock()
		return
	}
	s.watcher = nil
	s.watcherErr = errWatchLost
	s.rescanner = startFallbackRescan(s)
	r, logger := s.rescanner, s.logger
	s.mu.Unlock()
	_ = iw.w.Close()
	switch {
	case logger == nil:
	case r != nil:
		logger.Warn("native index watcher: %v — falling back to a %s disk rescan for out-of-process issue changes", errWatchLost, r.interval)
	default:
		logger.Warn("native index watcher: %v, and the rescan net is disabled (ITERION_NATIVE_INDEX_RESCAN=off) — out-of-process issue changes will not be seen until restart", errWatchLost)
	}
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
	if reloadErr := s.reconcileLocked(); reloadErr != nil {
		*err = fmt.Errorf("native store: %s panicked (%v) and index reload failed (%v) — restart recommended", name, r, reloadErr)
		return
	}
	*err = fmt.Errorf("native store: %s panicked: %v", name, r)
}
