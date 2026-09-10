package native

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// defaultRescanInterval is how often a Store whose watch the host refused
// re-reads issues/ from disk.
//
// The cost is one ReadDir plus an open+read+parse per card, so it is
// linear in the board and dominated by the filesystem — measured at ~10 µs
// a card on a dev box (4 ms for 200, 19 ms for 2 000) and ~45 µs a card in
// a container on overlayfs (8 ms for 200, 99 ms for 2 000), which is the
// shape a cloud dispatcher actually runs in. Read it as `cards ×
// per-card ÷ interval` rather than as one number: at this cadence that is
// well under 1% of a core for a few hundred cards and a few percent for a
// few thousand on the slower end.
//
// What does NOT vary is the property the cadence rests on: the scan runs
// OUTSIDE the store mutex, so however long it takes it stalls no reader.
const defaultRescanInterval = 2 * time.Second

// rescanIntervalOverride lets a test pin the interval; production resolves
// it from the environment when the net starts. Atomic like every seam in
// this package — see fireSeam.
var rescanIntervalOverride atomic.Pointer[time.Duration]

// rescanInterval resolves ITERION_NATIVE_INDEX_RESCAN when the net starts —
// not at package init, which would read the environment before a test's
// t.Setenv or an embedding process had set it. A Go duration or a bare
// number of seconds; "off", or a duration or number that is zero or
// negative ("0", "0s", "-1"), disables the net and restores the historical
// blind-until-restart behaviour — every DISABLING spelling is honoured,
// never quietly replaced by the default.
//
// Anything else — "OFF", "none", "2 s", a typo — is not a disable this can
// recognise, and it is deliberately NOT read as one: the fallback is the
// 2s default, because guessing "the operator meant off" would silently
// remove the correctness net on a host that has no watch. It is also not
// swallowed. The second return is that raw value, which the caller (which
// owns the logger this does not) names in the line it writes when the net
// starts — an operator who mistyped learns it from the log instead of from
// a board that stops updating.
func rescanInterval() (interval time.Duration, unread string) {
	if d := rescanIntervalOverride.Load(); d != nil {
		return *d, ""
	}
	raw := os.Getenv("ITERION_NATIVE_INDEX_RESCAN")
	if raw == "" {
		return defaultRescanInterval, ""
	}
	if raw == "off" {
		return 0, ""
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return 0, ""
		}
		return d, ""
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n <= 0 {
			return 0, ""
		}
		return time.Duration(n) * time.Second, ""
	}
	return defaultRescanInterval, raw
}

// reconcileScanning and reconcileScanned are test seams, unset in
// production: the first is called at the start of every disk scan — an
// in-process write must be able to land while it runs, which it could not
// if the scan held the store mutex; the second is called by Reconcile
// after its scan and before the swap — a write that lands there is one
// the scan did not see, and a swap must not revert it.
var (
	reconcileScanning atomic.Pointer[func(*Store)]
	reconcileScanned  atomic.Pointer[func(*Store)]
)

// fireSeam calls a seam if one is installed.
//
// Every seam in this package is an atomic, never a bare package var,
// because the goroutines that read them belong to a Store and outlive the
// test that made it: the watcher loop reads watchCheckIntervalOverride on
// its own goroutine, the rescan ticker and the rebuild goroutine read
// these two and rebuildPassEnding on theirs. A store a test leaves
// running therefore reads the seam the NEXT test assigns — a real data
// race, reported by `go test -race -shuffle=on ./pkg/dispatcher/native/`
// against a bare var (write in setWatchCheckInterval, read in
// (*indexWatcher).loop of an earlier test's store), and the `race` job is
// a required check.
func fireSeam(p *atomic.Pointer[func(*Store)], s *Store) {
	if f := p.Load(); f != nil {
		(*f)(s)
	}
}

// markDirtyLocked records a write to the index while a scan is in flight,
// so the swap keeps the index's value for that id. Caller holds mu; every
// index write funnels through setIndexLocked or dropIndexLocked, which
// call it.
func (s *Store) markDirtyLocked(id string) {
	if s.scanning > 0 {
		s.dirty[id] = true
	}
}

// rebuildPassEnding is a test seam, unset in production: called by the
// overflow rebuild goroutine after its last scan and before it releases
// the pending flag — the window in which a request used to be lost.
var rebuildPassEnding atomic.Pointer[func(*Store)]

// Reconcile rebuilds the index from the authoritative on-disk state.
//
// It is the reconciliation net behind the fsnotify fast path. inotify is
// a lossy carrier by construction — a host at fs.inotify.max_user_watches
// refuses the watch outright (ENOSPC), a host at max_user_instances
// refuses the inotify fd (EMFILE), a full kernel queue drops events
// (ErrEventOverflow), the kernel drops a watch whose directory is removed
// or renamed — and none of those is recoverable by waiting for an event
// that will never be sent. Without a net the index is then frozen for the
// life of the process: every `iterion __mcp-board` write is invisible to
// /board and to the dispatcher until the daemon restarts.
//
// The scan — one ReadDir plus a read and a parse per card — runs WITHOUT
// the store mutex, so a board read never waits behind disk I/O; only the
// swap takes the lock, and there is exactly one scan per call whatever
// the write load. An in-process write that lands during the scan (Create,
// a state move, a Delete) marks its id dirty, and the swap takes the
// index's own value for that id — never the scan's, which may predate it.
// A card whose file is present but could not be read keeps the entry the
// index already holds (a stale-but-readable card beats a forced 404, the
// policy applyEvent has); only a file that is gone leaves the index; and
// an issues/ directory that is gone is an error that leaves the index as
// it was — never an empty board.
//
// Concurrent callers (the ticker, an overflow) are serialised by their own
// mutex, so two overlapping scans cannot swap in the older one last.
func (s *Store) Reconcile() error {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()

	s.mu.Lock()
	if s.closed {
		// A rebuild asked for before Close has nothing to serve; Close
		// waits for the one in flight and no new one starts. Said so, not
		// passed as a success: a caller must not read "the index is fresh"
		// from a store that no longer looks at the disk.
		s.mu.Unlock()
		return ErrStoreClosed
	}
	epoch := s.scanEpoch
	s.scanning++
	if s.dirty == nil {
		s.dirty = map[string]bool{}
	}
	s.mu.Unlock()

	fresh, unreadable, err := s.scanIssues()
	if err != nil {
		s.mu.Lock()
		s.scanning--
		s.dirty = nil
		s.mu.Unlock()
		return err
	}
	fireSeam(&reconcileScanned, s)

	s.mu.Lock()
	s.scanning--
	if s.scanEpoch != epoch {
		// The panic-recovery rebuild swapped a NEWER snapshot in while this
		// scan ran (it holds mu, so it cannot take reconcileMu); this
		// older one must not land on top of it. The next tick scans again.
		s.dirty = nil
		s.mu.Unlock()
		return nil
	}
	for id := range s.dirty {
		if cur, ok := s.index[id]; ok {
			fresh[id] = cur
		} else {
			delete(fresh, id)
		}
	}
	s.dirty = nil
	warn := s.swapIndexLocked(fresh, unreadable)
	logger := s.logger
	s.mu.Unlock()
	if warn != "" && logger != nil {
		logger.Warn("%s", warn)
	}
	return nil
}

// reconcileLocked is Reconcile with the store mutex already held (the
// panic-recovery path). No in-process write can race it. On error the
// previous index is preserved: a stale-but-readable board beats an empty
// one.
func (s *Store) reconcileLocked() error {
	fresh, unreadable, err := s.scanIssues()
	if err != nil {
		return err
	}
	s.scanEpoch++ // an unlocked scan in flight is now older than the index
	if warn := s.swapIndexLocked(fresh, unreadable); warn != "" && s.logger != nil {
		s.logger.Warn("%s", warn)
	}
	return nil
}

// swapIndexLocked replaces the index with a scan, keeping the entry it
// already holds for every card the scan could not read. It returns the
// warning to write when the set of unreadable cards CHANGED since the
// last swap — "" otherwise, so a net ticking every two seconds over one
// corrupt file says so once, not 1 800 times an hour. Caller holds mu.
func (s *Store) swapIndexLocked(fresh map[string]*Issue, unreadable map[string]error) string {
	for id := range unreadable {
		if prev, ok := s.index[id]; ok {
			fresh[id] = prev
		}
	}
	s.index = fresh

	ids := make([]string, 0, len(unreadable))
	for id := range unreadable {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fp := strings.Join(ids, ",")
	if fp == s.unreadableFP {
		return ""
	}
	s.unreadableFP = fp
	if len(ids) == 0 {
		return "native index rescan: every issue file reads again"
	}
	return fmt.Sprintf("native index rescan: %d card(s) could not be read and are kept as last seen (e.g. %s: %v)", len(ids), ids[0], unreadable[ids[0]])
}

// scanIssues reads every committed issue file under issues/ into a new
// map. It touches no store state, so it is safe to call with or without
// the mutex. A missing issues/ directory is an ERROR: NewStore creates
// the directory before it ever scans, so the only way to meet its absence
// is a directory removed under a running store, and a scan that read it
// as an empty board would erase every card the index holds. A file that
// vanished between the listing and the read is gone; a file that is
// present but could not be read or parsed is returned apart, with its
// error, for the caller to decide — the rebuild keeps the last value,
// NewStore skips it.
func (s *Store) scanIssues() (fresh map[string]*Issue, unreadable map[string]error, err error) {
	fireSeam(&reconcileScanning, s)
	fresh = map[string]*Issue{}
	entries, err := os.ReadDir(filepath.Join(s.root, issuesDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("native store: scan issues: the issues/ directory is gone (%w) — index kept as last seen", err)
		}
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

// rebuildAsync runs Reconcile on its own goroutine, coalescing the
// requests that arrive while one runs into ONE more pass — never dropping
// them: a request that arrives mid-scan concerns files the running scan
// listed before they changed. The watcher loop asks for it on a
// kernel-queue overflow: the store opens fsnotify with NewWatcher, whose
// event channel is unbuffered, so a rebuild run ON the loop's goroutine
// would stop draining the very queue that just overflowed for the whole
// of the scan.
func (s *Store) rebuildAsync(what string) {
	s.rebuildRerun.Store(true)
	if !s.rebuildPending.CompareAndSwap(false, true) {
		return // the running pass will see the rerun flag and go again
	}
	// The ticket Close waits on. Taken under mu and refused once closed is
	// set, so a request that arrives during or after Close cannot add a
	// goroutine to a WaitGroup that is already being waited on — and the
	// pending flag is released, since no goroutine is coming to release it.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.rebuildPending.Store(false)
		return
	}
	s.rebuildWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.rebuildWG.Done()
		for {
			for s.rebuildRerun.CompareAndSwap(true, false) {
				// A store closed while the rebuild waited is not a failed
				// rebuild: nothing will read the index it did not refresh.
				if err := s.Reconcile(); err != nil && !errors.Is(err, ErrStoreClosed) {
					s.getLogger().Error("native index watcher: %s and the index rebuild failed: %v — board index may serve stale reads until the next write event or restart", what, err)
				}
			}
			fireSeam(&rebuildPassEnding, s)
			s.rebuildPending.Store(false)
			// A request that arrived between the last check above and this
			// release failed its own claim on the pending flag and is
			// relying on this goroutine: look once more, and re-claim.
			if !s.rebuildRerun.Load() || !s.rebuildPending.CompareAndSwap(false, true) {
				return
			}
		}
	}()
}

// startFallbackRescan runs Reconcile on a ticker for a Store that has no
// watcher. It exists so the store's promise — "a write another process
// makes under issues/ becomes visible through List/Get" — holds on a host
// that cannot give us a watch, instead of degrading silently until
// restart. Returns nil when the net is disabled. A rescan error is logged
// when it changes, not on every tick.
func startFallbackRescan(s *Store) *indexRescanner {
	interval, unread := rescanInterval()
	if interval <= 0 {
		return nil
	}
	r := &indexRescanner{interval: interval, envUnread: unread, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		lastErr := ""
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				err := s.Reconcile()
				if errors.Is(err, ErrStoreClosed) {
					return // Close is under way; its stop follows
				}
				msg := ""
				if err != nil {
					msg = err.Error()
				}
				if msg != lastErr {
					lastErr = msg
					if err != nil {
						s.getLogger().Warn("native index rescan: %v", err)
					} else {
						s.getLogger().Warn("native index rescan: scanning again")
					}
				}
			}
		}
	}()
	return r
}

// indexRescanner is the fallback net's goroutine handle.
type indexRescanner struct {
	interval time.Duration
	// envUnread is the ITERION_NATIVE_INDEX_RESCAN value the resolver
	// could not read, "" when there was none — see describe.
	envUnread string
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// describe names the cadence the net runs at, for the one line each
// caller writes when it arms one. It also names a value the operator set
// and this could not read: falling back to the default is the safe
// choice, doing it in silence is not — the board would just keep
// updating at a cadence nobody asked for.
func (r *indexRescanner) describe() string {
	if r.envUnread == "" {
		return r.interval.String()
	}
	return fmt.Sprintf("%s (ITERION_NATIVE_INDEX_RESCAN=%q is neither a Go duration nor a number of seconds and was ignored; %q disables the net)", r.interval, r.envUnread, "off")
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
