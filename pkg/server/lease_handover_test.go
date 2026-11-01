package server

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/lease"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
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

// steppingDownIntegrationStore is the holder's own step-down arriving inside
// the pass's first query: the term's context is cancelled while the store
// reads, and the call comes back the way a store call answers a cancelled
// request.
type steppingDownIntegrationStore struct {
	*forge.MemoryRepoIntegrationStore
	cancel func()
	calls  atomic.Int64
}

func (s *steppingDownIntegrationStore) ListSyncEnabled(ctx context.Context) ([]forge.RepoIntegration, error) {
	s.calls.Add(1)
	s.cancel()
	return nil, fmt.Errorf("forge: list sync-enabled integrations: %w", ctx.Err())
}

// The term-end rule at the board sync's own recording sites: a term cancelled
// inside the pass's first query is evidence about nothing — the cancellation
// is this holder stepping down, and a pass that recorded it would read
// refused, the verdict pkg/lease takes as the far side having ANSWERED.
// Driven through boardSyncPasses, the production term loop, so the verdict
// travels into the Health a term accounts into.
func TestBoardSyncPass_aCancelledTermAccountsIntoNothing(t *testing.T) {
	health := &lease.Health{Passes: 3}
	// Where this holder stands: two passes of its term reached no forge.
	health.Observe(lease.VerdictUnreachable)
	health.Observe(lease.VerdictUnreachable)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ints := &steppingDownIntegrationStore{MemoryRepoIntegrationStore: forge.NewMemoryRepoIntegrationStore(), cancel: cancel}
	s := &Server{forgeIntegrations: ints}
	s.boardSyncPasses(ctx, time.Millisecond, health)
	if n := ints.calls.Load(); n != 1 {
		t.Fatalf("the pass ran %d list calls, want exactly 1 — the loop stops on the term's end, not on a handover", n)
	}
	if st := health.Snapshot(); st.LastPassVerdict != lease.VerdictIdle.String() || st.FailingPasses != 2 {
		t.Fatalf("handover state = %+v, want an idle pass over an intact run of 2 unreachable passes — this holder's own step-down says nothing about its forge egress", st)
	}
}

// unreachableStore is the same failure class on the sweep's OTHER far side:
// the run document the scan named cannot be loaded because this node cannot
// reach the store.
func unreachableStore() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// gateRunLoadFailure is a store whose run reads fail with one given error —
// the scan answered (the sweep got its refs), the load behind it did not.
type gateRunLoadFailure struct {
	store.RunStore
	err error
}

func (g gateRunLoadFailure) LoadRun(context.Context, string) (*store.Run, error) {
	return nil, g.err
}

// The merge-gate sweep's pass reports the forge outcomes of the OFFERS it
// made, and the scan's answer is not one of them: before #2825 every pass
// whose Mongo query answered read "progressed", so a holder whose forge
// egress was broken kept the term while every gating pull request it owed a
// check sat unanswered.
func TestGateSweepPass_classifiesWhatItsOffersReached(t *testing.T) {
	oneRef := func(id string) *fakeGateSweepLister {
		return &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: id}}}
	}
	for _, c := range []struct {
		name  string
		build func(t *testing.T) (*Server, gateSweepLister)
		want  lease.PassVerdict
		why   string
	}{
		{
			name: "a scan it could not reach",
			build: func(t *testing.T) (*Server, gateSweepLister) {
				s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}})
				return s, &fakeGateSweepLister{err: unreachableForge()}
			},
			want: lease.VerdictUnreachable,
			why:  "a pass that cannot list the candidates of its window attempted work and reached nothing",
		},
		{
			name: "a scan that answered an empty window",
			build: func(t *testing.T) (*Server, gateSweepLister) {
				s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}})
				return s, &fakeGateSweepLister{}
			},
			want: lease.VerdictIdle,
			why:  "the store answering says nothing about the forge this net's offers post to",
		},
		{
			name: "a window where the runs owe nothing",
			build: func(t *testing.T) (*Server, gateSweepLister) {
				gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
				s, runID := gateReconcileFixture(t, map[string]any{"unrelated_other_thing": 1}, gc)
				return s, oneRef(runID)
			},
			want: lease.VerdictIdle,
			why:  "a run that gates nothing exits on a local field read, and a quiet window is the healthiest state this net has — never proof of reach",
		},
		{
			name: "an offer that reached the forge",
			build: func(t *testing.T) (*Server, gateSweepLister) {
				gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
				s, runID := gateReconcileFixture(t, gatingInputs(), gc)
				return s, oneRef(runID)
			},
			want: lease.VerdictProgressed,
			why:  "the offer read the pull request and posted the verdict it owed",
		},
		{
			name: "every offer failing for a transport reason",
			build: func(t *testing.T) (*Server, gateSweepLister) {
				gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef", getErr: unreachableForge()}}
				s, runID := gateReconcileFixture(t, gatingInputs(), gc)
				return s, oneRef(runID)
			},
			want: lease.VerdictUnreachable,
			why:  "this is the holder whose forge egress is broken: a peer on another node may well reach what it cannot",
		},
		{
			name: "an offer the forge refused",
			build: func(t *testing.T) (*Server, gateSweepLister) {
				gc := &listingGateClient{fakeGateClient: fakeGateClient{
					headSHA: "deadbeef",
					getErr:  &forge.StatusError{Provider: forge.ProviderGitHub, Op: "GET pull", Code: 403},
				}}
				s, runID := gateReconcileFixture(t, gatingInputs(), gc)
				return s, oneRef(runID)
			},
			want: lease.VerdictRefused,
			why:  "the answer coming back IS the reach — handing the term over would move the refusal, not repair it",
		},
		{
			name: "a run the scan named and the store cannot load",
			build: func(t *testing.T) (*Server, gateSweepLister) {
				gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
				s, runID := gateReconcileFixture(t, gatingInputs(), gc)
				s.cfg.Store = gateRunLoadFailure{RunStore: s.cfg.Store, err: unreachableStore()}
				return s, oneRef(runID)
			},
			want: lease.VerdictUnreachable,
			why:  "an offer that cannot read its run attempted work and did none of it; the cause is the store's, and it is the egress class a peer may not share",
		},
		{
			name: "a run the scan named and an operator deleted",
			build: func(t *testing.T) (*Server, gateSweepLister) {
				gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
				s, runID := gateReconcileFixture(t, gatingInputs(), gc)
				s.cfg.Store = gateRunLoadFailure{RunStore: s.cfg.Store, err: fmt.Errorf("store: load run %s: %w", runID, store.ErrRunDeleted)}
				return s, oneRef(runID)
			},
			want: lease.VerdictIdle,
			why:  "an absent or deleted run is an ANSWER from the store, not a failure to reach it",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, lister := c.build(t)
			var pass lease.Pass
			ctx := withGatePass(context.Background(), &pass)
			s.sweepGates(ctx, lister, time.Now().UTC(), gateSweepLookback, time.Time{})
			if got := pass.Verdict(); got != c.want {
				t.Fatalf("verdict = %s, want %s — %s", got, c.want, c.why)
			}
		})
	}
}

// The auto-fix lane is the pass's second offer on every run, and it reports
// its own forge reach: without it a holder whose egress breaks between the two
// lanes would have half a verdict. Called the way the sweep calls it
// (autofixOffer under the term's pass), so the attribution is this lane's
// alone — driving sweepGates would mix in the reconciler's offer on the same
// client.
func TestAutofixOffer_reportsWhatItReachedForTheHandover(t *testing.T) {
	redGate := stubGateClient{head: autofixReachHead, state: forge.CommitStateFailure, ctxName: autofixReachGate}
	for _, c := range []struct {
		name    string
		gc      forgeGateClient
		autofix bool
		// loadErr breaks the run read this offer starts with.
		loadErr error
		want    lease.PassVerdict
		why     string
	}{
		{
			name: "an offer that reached the forge", gc: redGate,
			autofix: true, want: lease.VerdictProgressed, why: "the lane read the pull request it was offered",
		},
		{
			name: "an offer that could not reach it", gc: unreachableAutofixClient{},
			autofix: true, want: lease.VerdictUnreachable, why: "the lane's first forge round-trip never arrived",
		},
		{
			name: "a repo that never opted in", gc: redGate,
			autofix: false, want: lease.VerdictIdle, why: "the lane declined on a local read with no forge traffic at all",
		},
		{
			name: "a run the store cannot load", gc: redGate, autofix: true, loadErr: unreachableStore(),
			want: lease.VerdictUnreachable, why: "this lane's offer answers for its own run read, exactly as the reconciler's does",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, runID := gateAutofixReachFixture(t, c.gc, c.autofix)
			if c.loadErr != nil {
				s.cfg.Store = gateRunLoadFailure{RunStore: s.cfg.Store, err: c.loadErr}
			}
			var pass lease.Pass
			s.autofixOffer(withGatePass(context.Background(), &pass), runID)
			if got := pass.Verdict(); got != c.want {
				t.Fatalf("verdict = %s, want %s — %s", got, c.want, c.why)
			}
		})
	}
}

// A sweep called outside an elected term — a test, the event path — accounts
// into no pass at all, and must not panic for it: every recorder here is
// called on a nil pass, scan and offers alike.
func TestGateSweepPass_aSweepWithNoPassAccountsIntoNothing(t *testing.T) {
	s, runID := gateReconcileFixture(t, gatingInputs(), &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef", getErr: unreachableForge()}})
	s.sweepGates(context.Background(), &fakeGateSweepLister{err: unreachableForge()}, time.Now().UTC(), gateSweepLookback, time.Time{})
	s.sweepGates(context.Background(), &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: runID}}}, time.Now().UTC(), gateSweepLookback, time.Time{})
}

// steppingDownForge is the holder's own step-down arriving inside an offer:
// the term's context is cancelled while the first forge round-trip is in
// flight, and the call comes back the way an HTTP client answers it —
// carrying the cancellation, or carrying the answer that was already on the
// wire when the term ended.
type steppingDownForge struct {
	cancel func()
	// answered makes the round-trip SUCCEED as the term ends: the 200 that
	// was in flight, reaching a holder with nothing left to act on it.
	answered bool
	calls    atomic.Int64
}

func (f *steppingDownForge) GetPullRequest(_ context.Context, repo string, _ int) (forge.PullRef, error) {
	f.calls.Add(1)
	f.cancel()
	if f.answered {
		return forge.PullRef{HeadSHA: "deadbeef", HeadRepoFullName: repo, State: "open"}, nil
	}
	return forge.PullRef{}, &url.Error{Op: "Get", URL: "https://forge.example/api/v4", Err: context.Canceled}
}

func (f *steppingDownForge) SetCommitStatus(context.Context, string, string, forge.CommitStatus) error {
	return nil
}

// A term cancelled while an offer is in flight is evidence about nothing —
// whichever way that round-trip then answers. The cancellation is this holder
// stepping down, and BOTH arms of the recorder would overwrite the term's own
// state with it (one Observe branch per verdict, health.go):
//   - the round-trip cut off reads `refused`, which pkg/lease reads as the far
//     side having answered — it clears the run of unreachable passes this term
//     accumulated and closes the circuit endTerm deliberately leaves open;
//   - the round-trip that answered reads `progressed`, which clears that run,
//     the circuit AND the run of sick terms: strictly more state, on an answer
//     this holder is no longer serving.
func TestGateSweepPass_aCancelledTermAccountsIntoNothing(t *testing.T) {
	for _, c := range []struct {
		name     string
		answered bool
	}{
		{name: "the cancellation cut the round-trip off"},
		{name: "the answer landed as the term ended", answered: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			health := &lease.Health{Passes: 3}
			// Where this holder stands: two passes of its term reached no forge.
			health.Observe(lease.VerdictUnreachable)
			health.Observe(lease.VerdictUnreachable)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gc := &steppingDownForge{cancel: cancel, answered: c.answered}
			s, runID := gateReconcileFixture(t, gatingInputs(), gc)
			// The production loop, so the verdict travels where a term reads
			// it: one pass, its offer cancelled mid-flight, handed to Observe.
			s.runGateSweeper(ctx, &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: runID}}}, health)
			if n := gc.calls.Load(); n != 1 {
				t.Fatalf("the pass made %d forge round-trips, want exactly 1 — a pass that never reached the forge would be idle for the wrong reason", n)
			}
			if st := health.Snapshot(); st.LastPassVerdict != lease.VerdictIdle.String() || st.FailingPasses != 2 {
				t.Fatalf("handover state = %+v, want an idle pass over an intact run of 2 unreachable passes — this holder's own step-down says nothing about its forge egress", st)
			}
		})
	}
}

// gateRunUnreadableFor fails LoadRun for ONE id, the way the store answers a
// row it cannot decode, and passes every other read through — the window
// holding one unreadable row beside live ones.
type gateRunUnreadableFor struct {
	store.RunStore
	id  string
	err error
}

func (g gateRunUnreadableFor) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if id == g.id {
		return nil, g.err
	}
	return g.RunStore.LoadRun(ctx, id)
}

// noEgressForge is this replica with no route to the forge at all: every
// round-trip fails on DNS, and the offers that tried are counted.
type noEgressForge struct{ calls atomic.Int64 }

func (f *noEgressForge) GetPullRequest(context.Context, string, int) (forge.PullRef, error) {
	f.calls.Add(1)
	return forge.PullRef{}, unreachableForge()
}

func (f *noEgressForge) SetCommitStatus(context.Context, string, string, forge.CommitStatus) error {
	return unreachableForge()
}

// One pass carries items from BOTH of this net's far sides — the store it
// reads each row from, the forge it offers each run to — and nothing orders
// them: ListNotifiableRuns sorts on `updated_at` alone, with no tiebreaker
// (pkg/store/mongo). So a verdict read off which row came back first is not a
// fact: with the undecodable row first the pass would read `refused`, the
// verdict Observe takes as the far side having ANSWERED, and a holder with
// zero forge egress would clear its own run of unreachable passes every
// minute and never hand the net over — #2825's defect, unfixed in one of two
// orders of the same window.
func TestGateSweepPass_aWindowOfBothFarSidesReadsTheSameInEitherRowOrder(t *testing.T) {
	const undecodable = "run-undecodable"
	// The shape the Mongo store answers an unprojectable row with: the store
	// ANSWERED, so it is no evidence about reach — and the forge is what this
	// pass could not reach.
	decodeErr := fmt.Errorf("mongo: decode run %s: cannot unmarshal string into Go struct field store.Run.status of type int", undecodable)
	for _, c := range []struct {
		name  string
		order func(gating string) []mongostore.NotifiableRunRef
	}{
		{
			name: "the undecodable row first",
			order: func(gating string) []mongostore.NotifiableRunRef {
				return []mongostore.NotifiableRunRef{{ID: undecodable}, {ID: gating}}
			},
		},
		{
			name: "the gating row first",
			order: func(gating string) []mongostore.NotifiableRunRef {
				return []mongostore.NotifiableRunRef{{ID: gating}, {ID: undecodable}}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			gc := &noEgressForge{}
			s, gating := gateReconcileFixture(t, gatingInputs(), gc)
			s.cfg.Store = gateRunUnreadableFor{RunStore: s.cfg.Store, id: undecodable, err: decodeErr}
			health := &lease.Health{Passes: 3}
			// Two passes of this term already reached no forge: K = 3, so
			// this pass is the one that decides the handover.
			health.Observe(lease.VerdictUnreachable)
			health.Observe(lease.VerdictUnreachable)
			var pass lease.Pass
			s.sweepGates(withGatePass(context.Background(), &pass), &fakeGateSweepLister{refs: c.order(gating)}, time.Now().UTC(), gateSweepLookback, time.Time{})
			if n := gc.calls.Load(); n != 1 {
				t.Fatalf("the pass made %d forge round-trips, want exactly 1 — the gating row must have been offered for its failure to be in the pass at all", n)
			}
			if got := pass.Verdict(); got != lease.VerdictUnreachable {
				t.Fatalf("verdict = %s, want %s — a pass with no success and one item that reached nothing is unreachable whatever order its rows arrived in", got, lease.VerdictUnreachable)
			}
			if handOver := health.Observe(pass.Verdict()); !handOver {
				t.Fatalf("the pass did not hand the net over: state = %+v, want the run of unreachable passes reaching K and a peer getting the lease", health.Snapshot())
			}
		})
	}
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

// steppingDownConnections is the holder's own step-down arriving inside the
// refresh worker's first query: the term's context is cancelled while the
// store lists what is due, and the call comes back the way a store call
// answers a cancelled request.
type steppingDownConnections struct {
	forge.ConnectionStore
	cancel func()
	calls  atomic.Int64
}

func (c *steppingDownConnections) ExpiringBefore(ctx context.Context, _ time.Time) ([]forge.Connection, error) {
	c.calls.Add(1)
	c.cancel()
	return nil, fmt.Errorf("forge: list expiring connections: %w", ctx.Err())
}

// The term-end rule at the forge token refresh's recorder, through the
// production sweep and the production pass loop: a term cancelled inside the
// worker's first query records nothing, and the Health keeps the run of
// unreachable passes the term had already accumulated.
// forgeCredentialSweep is what ListenAndServe hands runElectedForgeTokenRefresh.
func TestForgeCredentialSweep_aCancelledTermAccountsIntoNothing(t *testing.T) {
	health := &lease.Health{Passes: 3}
	health.Observe(lease.VerdictUnreachable)
	health.Observe(lease.VerdictUnreachable)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conns := &steppingDownConnections{ConnectionStore: forge.NewMemoryConnectionStore(), cancel: cancel}
	s := &Server{
		forgeConnections:  conns,
		forgeOrchestrator: &forge.Orchestrator{Integrations: forge.NewMemoryRepoIntegrationStore()},
	}
	worker := &forge.RefreshWorker{Connections: conns, Lead: time.Hour}
	forgeRefreshPasses(ctx, time.Millisecond, health, func(ctx context.Context) lease.PassVerdict {
		return s.forgeCredentialSweep(ctx, worker)
	})
	if n := conns.calls.Load(); n != 1 {
		t.Fatalf("the sweep ran %d list calls, want exactly 1 — the loop stops on the term's end", n)
	}
	if st := health.Snapshot(); st.LastPassVerdict != lease.VerdictIdle.String() || st.FailingPasses != 2 {
		t.Fatalf("handover state = %+v, want an idle pass over an intact run of 2 unreachable passes — this holder's own step-down says nothing about its forge egress", st)
	}
}

// steppingDownWatchStore is the holder's own step-down arriving inside the
// watch sweep's store reads, at the stage the test selects: the upper-bound
// read, the page read, or the page read whose answer was already on the wire
// when the term ended.
type steppingDownWatchStore struct {
	runwatch.Store
	cancel func()
	stage  int // 1: upper bound; 2: page read; 3: page read that answers anyway
	calls  atomic.Int64
	page   []runwatch.Watch
}

func (s *steppingDownWatchStore) ActiveWatchUpperBound(ctx context.Context) (*runwatch.WatchCursor, error) {
	if s.stage != 1 {
		return s.Store.ActiveWatchUpperBound(ctx)
	}
	s.calls.Add(1)
	s.cancel()
	return nil, fmt.Errorf("watch store: active watch upper bound: %w", ctx.Err())
}

func (s *steppingDownWatchStore) ListActivePage(ctx context.Context, after *runwatch.WatchCursor, upper runwatch.WatchCursor, limit int) ([]runwatch.Watch, error) {
	if s.stage == 0 {
		return s.Store.ListActivePage(ctx, after, upper, limit)
	}
	s.calls.Add(1)
	s.cancel()
	if s.stage == 3 {
		// The answer was in flight when the term ended; it lands anyway.
		return s.page, nil
	}
	return nil, fmt.Errorf("watch store: list active page: %w", ctx.Err())
}

// The term-end rule at the assistant run watch's recording sites, through
// the production sweep loop: a term cancelled inside its query — or one whose
// page answered as the term ended — records nothing, and the Health keeps the
// run of unreachable passes the term had accumulated. The pass's far side is
// the run store, so the sites are its reads (assistant_run_watch.go sweep).
func TestAssistantWatchSweep_aCancelledTermAccountsIntoNothing(t *testing.T) {
	for _, c := range []struct {
		name  string
		stage int
		page  []runwatch.Watch
	}{
		{name: "the upper-bound read cut off", stage: 1},
		{name: "the page read cut off", stage: 2},
		{name: "the page answered as the term ended", stage: 3, page: []runwatch.Watch{{ID: "w-armed", State: runwatch.WatchActive}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newArmFixture(t, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// One armed watch, so the pass's first read answers a non-nil
			// upper bound and the page read under test is reached at all.
			if err := f.ws.CreateWatch(context.Background(), runwatch.Watch{
				ID: "handover-watch", TargetRunID: "run-nowhere", AssistantRunID: "assistant-nowhere",
				State: runwatch.WatchActive, Kinds: []string{trigger.KindRunFailed}, CreatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			ws := &steppingDownWatchStore{Store: f.ws, cancel: cancel, stage: c.stage, page: c.page}
			health := &lease.Health{Passes: 3}
			// Where this holder stands: two passes of its term reached no store.
			health.Observe(lease.VerdictUnreachable)
			health.Observe(lease.VerdictUnreachable)
			f.coord.setRuntime(f.srv.runs, ws, f.srv.cfg.Bots.Paths)
			f.coord.sweepLoop(ctx, nil, health)
			if n := ws.calls.Load(); n != 1 {
				t.Fatalf("the sweep made %d store calls at stage %d, want exactly 1 — the loop stops on the term's end", n, c.stage)
			}
			if st := health.Snapshot(); st.LastPassVerdict != lease.VerdictIdle.String() || st.FailingPasses != 2 {
				t.Fatalf("handover state = %+v, want an idle pass over an intact run of 2 unreachable passes — this holder's own step-down says nothing about its store egress", st)
			}
		})
	}
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

// The same fleet shape for the merge-gate sweep, whose SCAN answers on every
// replica: three of them campaign, the first one elected cannot reach the
// forge its offers post to, and a peer ends up offering. With the verdict
// resting on the scan alone the holder kept the term forever — its pass read
// "progressed" every minute while every gating pull request it owed a check
// waited on an answer that never came.
func TestGateSweep_aSickHolderHandsOverToAPeerThatReachesTheForge(t *testing.T) {
	shared := lease.NewMemoryStore()
	// Who is elected first is the lease store's to decide, so the sick
	// replica is whichever one serves the first term. Every field here is
	// read and written under the mutex: three campaign goroutines feed it.
	var mu sync.Mutex
	first := ""
	offers := map[string]int{}
	offer := func(owner string) (sick bool) {
		mu.Lock()
		defer mu.Unlock()
		if first == "" {
			first = owner
		}
		offers[owner]++
		return owner == first
	}
	healthyOffers := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for owner, c := range offers {
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
		gc := &electionObservingGateClient{
			listingGateClient: listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}},
			reach:             func() bool { return offer(owner) },
		}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		s.leases, s.replicaID, s.logger = shared, owner, iterlog.New(iterlog.LevelInfo, log)
		// The sweep's lease is paced by its interval, so the test's interval
		// is also the term's TTL and retry (gateSweepLeaseSpec).
		s.gateSweepTick = 50 * time.Millisecond
		lister := &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: runID}}}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runElectedGateSweeper(ctx, lister)
		}()
	}
	// Generous on purpose: the property is that a healthy replica EVENTUALLY
	// offers, and without the handover none ever does, whatever the budget.
	deadline := time.Now().Add(15 * time.Second)
	for healthyOffers() == 0 {
		if time.Now().After(deadline) {
			cancel()
			wg.Wait()
			t.Fatalf("no peer offered a single run while the holder's every offer failed for a transport reason: %s", log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if out := log.String(); !strings.Contains(out, "handing over") {
		t.Errorf("no handover line in the campaign log: %s", out)
	}
}

// electionObservingGateClient lets the test decide, per offer, whether THIS
// replica reaches the forge the merge gate is posted on.
type electionObservingGateClient struct {
	listingGateClient
	reach func() (sick bool)
}

func (e *electionObservingGateClient) GetPullRequest(ctx context.Context, repo string, number int) (forge.PullRef, error) {
	if e.reach() {
		return forge.PullRef{}, unreachableForge()
	}
	return e.listingGateClient.GetPullRequest(ctx, repo, number)
}

const (
	autofixReachHead = "cafe1234cafe1234cafe1234cafe1234cafe1234"
	autofixReachGate = "iterion/review"
)

// unreachableAutofixClient is the auto-fix lane's forge with no egress to it.
type unreachableAutofixClient struct{ stubGateClient }

func (unreachableAutofixClient) GetPullRequest(context.Context, string, int) (forge.PullRef, error) {
	return forge.PullRef{}, unreachableForge()
}

// gateAutofixReachFixture wires the lane end to end — a terminal gating run,
// its grant, the repo's integration and webhook config — so the offer under
// test is the production one. autofix pins whether the repo opted in.
func gateAutofixReachFixture(t *testing.T, gc forgeGateClient, autofix bool) (*Server, string) {
	t.Helper()
	const (
		team  = "t1"
		repo  = "acme/widgets"
		prURL = "https://github.com/acme/widgets/pull/7"
	)
	s := newWebhookTestServer(t)
	s.cfg.WorkDir = writeConsumerBotFixture(t, "fixer-bot", "prior_review")
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Store = rs
	conns := forge.NewMemoryConnectionStore()
	if err := conns.Create(context.Background(), forge.Connection{ID: "c1", TenantID: team, Provider: forge.ProviderGitHub}); err != nil {
		t.Fatal(err)
	}
	s.forgeConnections = conns
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	s.forgePublishTokens.Register("run-token", ForgePublishGrant{TeamID: team, ConnectionID: "c1", Repo: repo})
	ints := forge.NewMemoryRepoIntegrationStore()
	if err := ints.Create(context.Background(), forge.RepoIntegration{
		ID: "i1", TenantID: team, ConnectionID: "c1", RepoFullName: repo,
		BotIDs: []string{"fixer-bot"}, WebhookID: "w1", AutoFixOnGateFailure: autofix,
		LaunchVars: map[string]string{gateContextVar: autofixReachGate},
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeIntegrations = ints
	if err := s.webhookConfigs.Create(context.Background(), webhooks.Config{
		ID: "w1", TenantID: team, BotIDs: []string{"fixer-bot"},
	}); err != nil {
		t.Fatal(err)
	}
	s.forgeGateClientFor = func(context.Context, forge.Connection) (forgeGateClient, error) { return gc, nil }
	s.webhookLaunchBot = func(context.Context, string, map[string]string, string, string, string, map[string]string, map[string]string) (string, error) {
		return "run-fixer", nil
	}
	id, err := store.GenerateRunID()
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(context.Background(), id, "reviewer-bot", map[string]any{
		"pr_url": prURL, "gate_context": autofixReachGate, "head_sha": autofixReachHead,
		forgePublishVarToken: "run-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	run.BotID = "reviewer-bot"
	run.Status = store.RunStatusFinished
	if err := rs.SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return s, run.ID
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
