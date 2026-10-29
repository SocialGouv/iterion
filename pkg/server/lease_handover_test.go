package server

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/lease"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// unreachableForge is the failure a per-node resolver flap or a NetworkPolicy
// reaches a sweep as, wrapped the way an HTTP client wraps it.
func unreachableForge() error {
	return &url.Error{Op: "Get", URL: "https://forge.example/api/v4", Err: &net.DNSError{Err: "no such host", Name: "forge.example", IsNotFound: true}}
}

// sickIntegrationStore is a repo-integration store this replica cannot reach:
// every pass's first act — listing what to sweep — fails for a transport
// reason, which is the sick-but-alive leader of #2685.
type sickIntegrationStore struct {
	*forge.MemoryRepoIntegrationStore
	n atomic.Int64
}

func (s *sickIntegrationStore) ListSyncEnabled(context.Context) ([]forge.RepoIntegration, error) {
	s.n.Add(1)
	return nil, unreachableForge()
}

// The pass says what it reached, and the absence of work is not sickness.
func TestBoardSyncPass_classifiesWhatThePassReached(t *testing.T) {
	t.Run("a list it could not reach is not an idle pass", func(t *testing.T) {
		s := &Server{forgeIntegrations: &sickIntegrationStore{MemoryRepoIntegrationStore: forge.NewMemoryRepoIntegrationStore()}}
		if got := s.syncAllEnabledIntegrations(context.Background()); got != lease.VerdictUnreachable {
			t.Fatalf("verdict = %s, want %s", got, lease.VerdictUnreachable)
		}
	})
	t.Run("nothing to mirror is an idle pass", func(t *testing.T) {
		s := &Server{forgeIntegrations: forge.NewMemoryRepoIntegrationStore()}
		if got := s.syncAllEnabledIntegrations(context.Background()); got != lease.VerdictIdle {
			t.Fatalf("verdict = %s, want %s — a fleet with no sync-enabled repo is the healthiest state this net has", got, lease.VerdictIdle)
		}
	})
}

// The merge-gate sweep's verdict is its SCAN's: a pass that cannot reach the
// rows of its window attempted work and reached nothing, while a scan that
// answers proves the reach whatever the window held.
func TestGateSweepPass_theScanIsWhatTheVerdictRests(t *testing.T) {
	for _, c := range []struct {
		name   string
		lister *fakeGateSweepLister
		want   lease.PassVerdict
	}{
		{"a scan it could not reach", &fakeGateSweepLister{err: unreachableForge()}, lease.VerdictUnreachable},
		{"a scan that answered an empty window", &fakeGateSweepLister{}, lease.VerdictProgressed},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}})
			var pass lease.Pass
			ctx := withGatePass(context.Background(), &pass)
			s.sweepGates(ctx, c.lister, time.Now().UTC(), gateSweepLookback, time.Time{})
			if got := pass.Verdict(); got != c.want {
				t.Fatalf("verdict = %s, want %s", got, c.want)
			}
		})
	}
}

// A sweep called outside an elected term — a test, the event path — accounts
// into no pass at all, and must not panic for it.
func TestGateSweepPass_aSweepWithNoPassAccountsIntoNothing(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}})
	s.sweepGates(context.Background(), &fakeGateSweepLister{err: unreachableForge()}, time.Now().UTC(), gateSweepLookback, time.Time{})
}

// forgeRefreshPasses is the shape every net's pass loop shares: K passes that
// reached nothing end the term, and returning is what releases the lease.
func TestForgeRefreshPasses_handsOverAfterItsRunOfUnreachablePasses(t *testing.T) {
	// K = 1 is its own case, and not a smaller version of K = 3: a term
	// opens with a pass, and a loop that honours the handover only on the
	// TICKED passes still ends a K = 3 term after three — it just runs one
	// pass too many, and one too many is the whole blackhole when K is 1.
	for _, k := range []int{1, 3} {
		t.Run(fmt.Sprintf("K=%d", k), func(t *testing.T) {
			health := &lease.Health{Passes: k}
			var n atomic.Int64
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				forgeRefreshPasses(ctx, time.Millisecond, health, func(context.Context) lease.PassVerdict {
					n.Add(1)
					var pass lease.Pass
					pass.Failed(unreachableForge())
					return pass.Verdict()
				})
			}()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatalf("the pass loop never returned after %d unreachable passes: the term cannot end on the work failing", n.Load())
			}
			if got := n.Load(); got != int64(k) {
				t.Fatalf("the loop ran %d passes, want exactly the %d that end a term", got, k)
			}
		})
	}
}

// Its mirror, as its own isolate: a sweep that finds nothing due keeps the
// term for as long as it holds the lease.
func TestForgeRefreshPasses_anIdleSweepKeepsTheTerm(t *testing.T) {
	health := &lease.Health{Passes: 3}
	var n atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		forgeRefreshPasses(ctx, time.Millisecond, health, func(context.Context) lease.PassVerdict {
			n.Add(1)
			return lease.VerdictIdle
		})
	}()
	for n.Load() < 20 {
		select {
		case <-done:
			t.Fatalf("the pass loop returned after %d idle passes: an idle net handed its lease over", n.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// The fleet shape, through the entry point ListenAndServe starts: three
// replicas campaign for the board-issue sync, the first one elected cannot
// reach the store its passes list, and a peer ends up sweeping. Before the
// handover, that peer never ran a single pass however long the outage lasted.
func TestBoardIssueSync_aSickHolderHandsOverToAReplicaThatCanReachTheForge(t *testing.T) {
	shared := lease.NewMemoryStore()
	// Who is elected first is the store's to decide, so the sick replica is
	// whichever one serves the first term — and every field of this observer
	// is read and written under its mutex: it is fed by three campaign
	// goroutines.
	var mu sync.Mutex
	first := ""
	passes := map[string]int{}
	pass := func(owner string) (sick bool) {
		mu.Lock()
		defer mu.Unlock()
		if first == "" {
			first = owner
		}
		passes[owner]++
		return owner == first
	}
	healthyPasses := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for owner, c := range passes {
			if owner != first {
				n += c
			}
		}
		return n
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	log := &lockedBuffer{}
	for i := 0; i < 3; i++ {
		owner := fmt.Sprintf("replica-%d", i)
		ints := &electionObservingIntegrationStore{
			MemoryRepoIntegrationStore: forge.NewMemoryRepoIntegrationStore(),
			pass:                       func() bool { return pass(owner) },
		}
		s := &Server{
			leases:            shared,
			replicaID:         owner,
			logger:            iterlog.New(iterlog.LevelInfo, log),
			forgeIntegrations: ints,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runBoardSyncWorker(ctx, 50*time.Millisecond)
		}()
	}
	// Generous on purpose: the property is that a healthy replica EVENTUALLY
	// sweeps, and without the handover none ever does, whatever the budget.
	deadline := time.Now().Add(15 * time.Second)
	for healthyPasses() == 0 {
		if time.Now().After(deadline) {
			cancel()
			wg.Wait()
			t.Fatalf("no peer ran a single pass while the holder's every pass failed for a transport reason: %s", log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if out := log.String(); !strings.Contains(out, "handing over") {
		t.Errorf("no handover line in the campaign log: %s", out)
	}
}

// electionObservingIntegrationStore lets the test decide, per pass, whether
// THIS replica can reach the store its sweep lists from.
type electionObservingIntegrationStore struct {
	*forge.MemoryRepoIntegrationStore
	pass func() (sick bool)
}

func (e *electionObservingIntegrationStore) ListSyncEnabled(ctx context.Context) ([]forge.RepoIntegration, error) {
	if e.pass() {
		return nil, unreachableForge()
	}
	return e.MemoryRepoIntegrationStore.ListSyncEnabled(ctx)
}

// The operator's override: off disarms every net, a count replaces K, and a
// value that is neither keeps the default AND says so — an explicit choice is
// never replaced in silence, and a typo must not silently disarm the cover.
func TestLeaseHandoverFromEnv_readsTheOverrideAndRefusesToGuess(t *testing.T) {
	for _, c := range []struct {
		raw        string
		wantOff    bool
		wantPasses int
		wantWarn   string
	}{
		{"", false, 0, ""},
		{"off", true, 0, "no leased net will hand its term over"},
		{"FALSE", true, 0, "no leased net will hand its term over"},
		{"0", true, 0, "no leased net will hand its term over"},
		{"5", false, 5, ""},
		{"  7  ", false, 7, ""},
		{"-2", false, 0, "neither off nor a positive pass count"},
		{"soon", false, 0, "neither off nor a positive pass count"},
	} {
		t.Run("ITERION_LEASE_HANDOVER="+c.raw, func(t *testing.T) {
			var warned []string
			off, passes := leaseHandoverFromEnv(
				func(name string) string {
					if name != leaseHandoverEnv {
						t.Fatalf("read %s, want %s", name, leaseHandoverEnv)
					}
					return c.raw
				},
				func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) },
			)
			if off != c.wantOff || passes != c.wantPasses {
				t.Fatalf("(off, passes) = (%v, %d), want (%v, %d)", off, passes, c.wantOff, c.wantPasses)
			}
			switch {
			case c.wantWarn == "" && len(warned) > 0:
				t.Fatalf("warned about a value it accepted: %v", warned)
			case c.wantWarn != "":
				if len(warned) != 1 || !strings.Contains(warned[0], c.wantWarn) || !strings.Contains(warned[0], leaseHandoverEnv) {
					t.Fatalf("warnings = %v, want one naming %s and %q", warned, leaseHandoverEnv, c.wantWarn)
				}
			}
		})
	}
}

// The policy is per NET: one shared between two of them would let a broken
// forge end the board sync's term on the gate sweep's failures.
func TestNewLeaseHealth_isOnePolicyPerNet(t *testing.T) {
	s := &Server{leaseHandoverPasses: 4}
	a, b := s.newLeaseHealth(), s.newLeaseHealth()
	if a == b {
		t.Fatal("two nets were handed the same handover policy")
	}
	for i := 1; i <= 4; i++ {
		if got := a.Observe(lease.VerdictUnreachable); got != (i == 4) {
			t.Fatalf("pass %d of 4 handed over = %v, want %v: the operator's pass count was not honoured", i, got, i == 4)
		}
	}
	if st := b.Snapshot(); st.FailingPasses != 0 {
		t.Fatalf("the second net's policy carries %d failing passes from the first", st.FailingPasses)
	}
	if st := a.Snapshot(); st.Passes != 4 {
		t.Fatalf("snapshot = %+v, want the operator's 4 passes", st)
	}
}
