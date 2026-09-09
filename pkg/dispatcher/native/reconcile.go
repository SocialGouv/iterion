package native

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// defaultRescanInterval is how often a Store whose watch the host refused
// re-reads issues/ from disk. Measured: a full scan costs ~4 ms for 200
// cards and ~19 ms for 2000 (≈10 µs a card), and it runs OUTSIDE the store
// mutex, so at this cadence it is ~1% of one core and stalls no reader.
const defaultRescanInterval = 2 * time.Second

// rescanIntervalOverride lets a test pin the interval; production resolves
// it from the environment when the net starts.
var rescanIntervalOverride *time.Duration

// rescanInterval resolves ITERION_NATIVE_INDEX_RESCAN when the net starts —
// not at package init, which would read the environment before a test's
// t.Setenv or an embedding process had set it. A Go duration; "off" or "0"
// disables the net and restores the historical blind-until-restart
// behaviour; an unparsable value falls back to the default.
func rescanInterval() time.Duration {
	if rescanIntervalOverride != nil {
		return *rescanIntervalOverride
	}
	raw := os.Getenv("ITERION_NATIVE_INDEX_RESCAN")
	if raw == "" {
		return defaultRescanInterval
	}
	if raw == "off" || raw == "0" {
		return 0
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return defaultRescanInterval
}

// reconcileScanning and reconcileScanned are test seams, nil in
// production: the first is called at the start of every disk scan — an
// in-process write must be able to land while it runs, which it could not
// if the scan held the store mutex; the second is called by Reconcile
// after its scan and before the swap — a write that lands there is one
// the scan did not see, and a swap must not revert it.
var (
	reconcileScanning func()
	reconcileScanned  func()
)

// reconcileRetries is how many times Reconcile re-scans when an in-process
// write landed during the scan before it falls back to a locked rebuild.
const reconcileRetries = 3

// Reconcile rebuilds the index from the authoritative on-disk state.
//
// It is the reconciliation net behind the fsnotify fast path. inotify is
// a lossy carrier by construction — a host at fs.inotify.max_user_watches
// refuses the watch outright (ENOSPC), a host at max_user_instances
// refuses the inotify fd (EMFILE), a full kernel queue drops events
// (ErrEventOverflow), a watch can go away mid-life — and none of those is
// recoverable by waiting for an event that will never be sent. Without a
// net the index is then frozen for the life of the process: every
// `iterion __mcp-board` write is invisible to /board and to the
// dispatcher until the daemon restarts.
//
// The scan — one ReadDir plus a read and a parse per card — runs WITHOUT
// the store mutex, so a board read never waits behind disk I/O; only the
// swap takes the lock. An in-process write that lands during the scan
// (Create, a state move, a Delete) is detected by the write counter and
// the stale scan is discarded rather than allowed to revert it. A card
// whose file is present but could not be read keeps the entry the index
// already holds — a stale-but-readable card beats a forced 404, the policy
// applyEvent has — and only a file that is gone leaves the index.
//
// Concurrent callers (the ticker, an overflow) are serialised by their own
// mutex, so two overlapping scans cannot swap in the older one last.
func (s *Store) Reconcile() error {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	for attempt := 0; attempt < reconcileRetries; attempt++ {
		s.mu.Lock()
		before := s.writes
		s.mu.Unlock()

		fresh, unreadable, err := s.scanIssues()
		if err != nil {
			return err
		}
		if reconcileScanned != nil {
			reconcileScanned()
		}

		s.mu.Lock()
		if s.writes == before {
			s.swapIndexLocked(fresh, unreadable)
			logger := s.logger
			s.mu.Unlock()
			warnUnreadable(logger, unreadable)
			return nil
		}
		s.mu.Unlock()
	}
	// A write raced every scan: rebuild under the lock, the one window no
	// write can enter.
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcileLocked()
}

// reconcileLocked is Reconcile with the store mutex already held (the
// panic-recovery path, and Reconcile's own fallback). On error the previous
// index is preserved: a stale-but-readable board beats an empty one.
func (s *Store) reconcileLocked() error {
	fresh, unreadable, err := s.scanIssues()
	if err != nil {
		return err
	}
	s.swapIndexLocked(fresh, unreadable)
	warnUnreadable(s.logger, unreadable)
	return nil
}

// swapIndexLocked replaces the index with a scan, keeping the entry it
// already holds for every card the scan could not read. Caller holds mu.
func (s *Store) swapIndexLocked(fresh map[string]*Issue, unreadable map[string]error) {
	for id := range unreadable {
		if prev, ok := s.index[id]; ok {
			fresh[id] = prev
		}
	}
	s.index = fresh
}

// warnUnreadable says, once per scan, how many cards were kept from the
// previous index because their file could not be read, and why for one.
func warnUnreadable(logger *iterlog.Logger, unreadable map[string]error) {
	if len(unreadable) == 0 || logger == nil {
		return
	}
	for id, err := range unreadable {
		logger.Warn("native index rescan: %d card(s) could not be read and were kept as last seen (e.g. %s: %v)", len(unreadable), id, err)
		return
	}
}

// scanIssues reads every committed issue file under issues/ into a new
// map. It touches no store state, so it is safe to call with or without
// the mutex. A missing issues/ directory is an empty board, not an error.
// A file that vanished between the listing and the read is gone; a file
// that is present but could not be read or parsed is returned apart, with
// its error, for the caller to decide — the rebuild keeps the last value,
// NewStore skips it.
func (s *Store) scanIssues() (fresh map[string]*Issue, unreadable map[string]error, err error) {
	if reconcileScanning != nil {
		reconcileScanning()
	}
	fresh = map[string]*Issue{}
	entries, err := os.ReadDir(filepath.Join(s.root, issuesDir))
	if errors.Is(err, fs.ErrNotExist) {
		return fresh, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("native store: scan issues: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		id := decodeID(strings.TrimSuffix(e.Name(), ".json"))
		iss, err := s.readIssueFromDisk(id)
		switch {
		case err == nil:
			fresh[id] = iss
		case errors.Is(err, tracker.ErrNotFound):
			// Removed between the listing and the read: gone.
		default:
			if unreadable == nil {
				unreadable = map[string]error{}
			}
			unreadable[id] = err
		}
	}
	return fresh, unreadable, nil
}

// startFallbackRescan runs Reconcile on a ticker for a Store that has no
// watcher. It exists so the store's promise — "a write another process
// makes under issues/ becomes visible through List/Get" — holds on a host
// that cannot give us a watch, instead of degrading silently until
// restart. Returns nil when the net is disabled.
func startFallbackRescan(s *Store) *indexRescanner {
	interval := rescanInterval()
	if interval <= 0 {
		return nil
	}
	r := &indexRescanner{interval: interval, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				if err := s.Reconcile(); err != nil {
					s.getLogger().Warn("native index rescan: %v", err)
				}
			}
		}
	}()
	return r
}

// indexRescanner is the fallback net's goroutine handle.
type indexRescanner struct {
	interval  time.Duration
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func (r *indexRescanner) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		close(r.stop)
		<-r.done
	})
	return nil
}
