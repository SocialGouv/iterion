package server

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// spyLeaseStore records every successful Acquire on the store it wraps, so a
// test can tell a sweeper that holds THE shared lease from one that elected
// itself on a private store or under another name.
type spyLeaseStore struct {
	lease.Store
	mu       sync.Mutex
	acquired map[string]map[string]int // lease name → owner → acquisitions
}

func (s *spyLeaseStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	ok, err := s.Store.Acquire(ctx, name, owner, now, ttl)
	if ok {
		s.mu.Lock()
		if s.acquired == nil {
			s.acquired = map[string]map[string]int{}
		}
		if s.acquired[name] == nil {
			s.acquired[name] = map[string]int{}
		}
		s.acquired[name][owner]++
		s.mu.Unlock()
	}
	return ok, err
}

func (s *spyLeaseStore) acquisitions(name, owner string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquired[name][owner]
}

// Three replicas share one lease store: only the holder sweeps. A replica
// sweeping without holding the SHARED forge-token-refresh lease under its own
// id — elected on a private store, or under another name — is the race of
// #2682: two sweeps refreshing one rotating refresh token, the loser marking
// the connection needs_reauth.
func TestForgeTokenRefresh_OnlyTheLeaseHolderSweeps(t *testing.T) {
	shared := &spyLeaseStore{Store: lease.NewMemoryStore()}
	type replica struct {
		s    *Server
		log  *lockedBuffer
		n    atomic.Int64
		stop context.CancelFunc
		done chan struct{}
	}
	const replicas = 3
	rs := make([]*replica, replicas)
	for i := range rs {
		log := &lockedBuffer{}
		s := &Server{leases: shared, replicaID: fmt.Sprintf("replica-%d", i), logger: iterlog.New(iterlog.LevelInfo, log)}
		ctx, cancel := context.WithCancel(context.Background())
		r := &replica{s: s, log: log, stop: cancel, done: make(chan struct{})}
		rs[i] = r
		go func() {
			defer close(r.done)
			r.s.runElectedForgeTokenRefresh(ctx, 200*time.Millisecond, func(context.Context) { r.n.Add(1) })
		}()
	}
	t.Cleanup(func() {
		for _, r := range rs {
			r.stop()
			<-r.done
		}
	})
	total := func() (n int64) {
		for _, r := range rs {
			n += r.n.Load()
		}
		return n
	}
	waitForCond(t, 10*time.Second, "several refresh passes", func() bool { return total() >= 5 })
	var unelected []string
	for _, r := range rs {
		if r.n.Load() > 0 && shared.acquisitions(leaseForgeTokenRefresh, r.s.replicaID) == 0 {
			unelected = append(unelected, r.s.replicaID)
		}
	}
	if len(unelected) > 0 {
		t.Fatalf("%v swept without acquiring %q on the shared lease store — two replicas refreshing one rotating refresh token is how a healthy connection ends needs_reauth", unelected, leaseForgeTokenRefresh)
	}
}

// A term that ends before its first tick still refreshes once: a rolling
// deploy re-phases every ticker, and a token about to be refreshed must not
// sit dying until a tick that may never come in this term.
func TestForgeRefreshPasses_ATermOpensWithAPass(t *testing.T) {
	var n atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		forgeRefreshPasses(ctx, time.Hour, func(context.Context) { n.Add(1) })
	}()
	waitForCond(t, 5*time.Second, "the term's opening pass", func() bool { return n.Load() >= 1 })
	cancel()
	<-done
	if got := n.Load(); got != 1 {
		t.Fatalf("passes = %d in a term shorter than one tick, want exactly 1 (the opening pass)", got)
	}
}
