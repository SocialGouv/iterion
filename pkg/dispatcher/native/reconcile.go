package native

import (
	"os"
	"strconv"
	"sync"
	"time"
)

// fallbackRescanInterval is how often a Store whose watch the host
// refused re-reads issues/ from disk. Overridable with
// ITERION_NATIVE_INDEX_RESCAN (a Go duration; "off" disables the net and
// restores the historical blind-until-restart behaviour).
var fallbackRescanInterval = resolveRescanInterval()

func resolveRescanInterval() time.Duration {
	raw := os.Getenv("ITERION_NATIVE_INDEX_RESCAN")
	if raw == "" {
		return 2 * time.Second
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
	return 2 * time.Second
}

// Reconcile rebuilds the index from the authoritative on-disk state.
//
// It is the reconciliation net behind the fsnotify fast path. inotify is
// a lossy carrier by construction — a host at fs.inotify.max_user_watches
// refuses the watch outright (ENOSPC), a host at max_user_instances
// refuses the inotify fd (EMFILE), and neither refusal is recoverable by
// waiting for an event that will never be sent. Without a net the index
// is then frozen at its NewStore snapshot for the life of the process:
// every `iterion __mcp-board` write is invisible to /board and to the
// dispatcher until the daemon restarts.
//
// Unlike populateIndex it is a full rebuild, so an issue whose file was
// removed out-of-process also leaves the index.
func (s *Store) Reconcile() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcileLocked()
}

// reconcileLocked is Reconcile with the store mutex already held. On
// error the previous index is preserved: a stale-but-readable board
// beats an empty one.
func (s *Store) reconcileLocked() error {
	prev := s.index
	s.index = map[string]*Issue{}
	if err := s.populateIndex(); err != nil {
		s.index = prev
		return err
	}
	return nil
}

// startFallbackRescan runs Reconcile on a ticker for a Store that has no
// watcher. It exists so the store's promise — "a write another process
// makes under issues/ becomes visible through List/Get" — holds on a host
// that cannot give us a watch, instead of degrading silently until
// restart. Returns nil when the net is disabled.
func startFallbackRescan(s *Store) *indexRescanner {
	if fallbackRescanInterval <= 0 {
		return nil
	}
	r := &indexRescanner{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(r.done)
		t := time.NewTicker(fallbackRescanInterval)
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
