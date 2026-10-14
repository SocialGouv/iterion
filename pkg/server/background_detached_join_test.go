package server

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// registeredWorkerNames is the registry the shutdown will join, by name.
func registeredWorkerNames(srv *Server) []string {
	srv.bgMu.Lock()
	defer srv.bgMu.Unlock()
	names := make([]string, 0, len(srv.bgWorkers))
	for _, w := range srv.bgWorkers {
		names = append(names, w.name)
	}
	return names
}

// awaitDrainedRegistry waits until the join has TAKEN the registry — the same
// critical section that empties it precedes the wait — so a write asked for
// after this point is asked for while the join is waiting. Observing the
// bgJoined flag instead would be observing the defect.
func awaitDrainedRegistry(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if len(registeredWorkerNames(srv)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the join never took the registry: %v", registeredWorkerNames(srv))
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitRegistryClosed reports whether the shutdown closed its registry within
// grace. A shutdown that closes it ahead of its last wait does so in
// microseconds; one that closes it after is still waiting when grace is out.
func awaitRegistryClosed(srv *Server, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		srv.bgMu.Lock()
		joined := srv.bgJoined
		srv.bgMu.Unlock()
		if joined {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func hasWorkerNaming(srv *Server, label string) bool {
	for _, name := range registeredWorkerNames(srv) {
		if strings.Contains(name, label) {
			return true
		}
	}
	return false
}

// A best-effort detached write — an audit row, a last_used_at stamp, an
// invitation email — is JOINED by the shutdown like the server's loops are.
// The write has no reconciling sweep behind it: cut by the process exit, the
// audit row of the mutation that was just answered 200 is simply gone.
func TestShutdownJoinsTheDetachedWritesItSpawns(t *testing.T) {
	t.Run("a write still in flight holds the exit", func(t *testing.T) {
		srv := newMissionTestServer(t)
		// Widened well past the observation window below, as the loop join
		// tests do: a starved machine can then only make this test wait.
		srv.bgJoinBudget = 30 * time.Second
		entered := make(chan struct{})
		release := make(chan struct{})
		returned := make(chan struct{})
		srv.goSafe("test.detachedWrite", func() {
			close(entered)
			<-release
			close(returned)
		})
		<-entered

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		shutdownReturned := make(chan error, 1)
		go func() { shutdownReturned <- srv.Shutdown(ctx) }()
		select {
		case <-shutdownReturned:
			t.Fatal("Shutdown returned while the detached write was still in flight — the write is outside the grace period")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		select {
		case err := <-shutdownReturned:
			if err != nil {
				t.Fatalf("shutdown: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Shutdown never returned once the write had")
		}
		select {
		case <-returned:
		default:
			t.Fatal("Shutdown reported done before the write had returned")
		}
	})

	t.Run("a write spawned during the HTTP drain is still joined", func(t *testing.T) {
		// The shutdown signal fires BEFORE the HTTP drain, so requests are
		// still being answered after it — and the join comes after the drain.
		// A write refused in that window would lose the audit row of a
		// mutation the server answered 200, on every rolling update.
		srv := newMissionTestServer(t)
		close(srv.shutdown)
		ran := make(chan struct{})
		srv.goSafe("test.drainWindowWrite", func() { close(ran) })
		select {
		case <-ran:
		case <-time.After(10 * time.Second):
			t.Fatal("a write spawned while the HTTP drain was running never ran")
		}
		if !hasWorkerNaming(srv, "test.drainWindowWrite") {
			t.Fatalf("the drain-window write was never registered for the join: %v", registeredWorkerNames(srv))
		}
	})

	t.Run("a write does not queue behind the server's state mutex", func(t *testing.T) {
		// The projection semaphore is pre-allocated so ONE stateMu lock
		// covers a webhook delivery (server.go New). A registry borrowing
		// that same mutex would put a second write-lock on every audited
		// mutation, every PAT and config-share request and every delivery,
		// queued behind whatever project-switch writer holds it — and the
		// pipeline board, /api/bots and /api/server/info read through it.
		srv, _ := newJoinTestServer(t)
		ran := make(chan struct{})
		asked := make(chan struct{})
		srv.stateMu.Lock()
		go func() {
			srv.goSafe("test.writeWhileStateMuIsHeld", func() { close(ran) })
			close(asked)
		}()
		asking := false
		select {
		case <-asked:
			asking = true
		case <-time.After(5 * time.Second):
		}
		srv.stateMu.Unlock()
		if !asking {
			t.Fatal("asking for a detached write blocked on stateMu — the registry takes the server's central mutex on a request path")
		}
		select {
		case <-ran:
		case <-time.After(10 * time.Second):
			t.Fatal("the detached write never ran")
		}
	})

	t.Run("a write a joined loop asks for on its way out is joined too", func(t *testing.T) {
		// The retry sweeper's abandon path: a registered loop audits its last
		// decision after its cancel lands (disarmRetry → auditSystem →
		// goSafe). The join is waiting for that very loop, so a registry
		// closed before the wait would refuse the one row this join exists to
		// cover — and report a clean exit while doing it.
		srv, _ := newJoinTestServer(t)
		srv.bgJoinBudget = 30 * time.Second
		rows := audit.NewMemoryStore()
		srv.auditStore = rows

		parked := make(chan struct{})
		release := make(chan struct{})
		srv.goUntilShutdown("test.auditingSweep", func(ctx context.Context) {
			<-ctx.Done()
			close(parked)
			<-release
			srv.auditSystem("tenant-1", "retry-sweeper", "run.retry.abandoned", "run", "run-1", nil)
		})

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		shutdownReturned := make(chan error, 1)
		go func() { shutdownReturned <- srv.Shutdown(ctx) }()
		<-parked
		awaitDrainedRegistry(t, srv)
		close(release)

		select {
		case err := <-shutdownReturned:
			if err != nil {
				t.Fatalf("shutdown: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Shutdown never returned")
		}
		// Read with no wait of its own: a row that lands after Shutdown
		// returned is a row the grace period did not cover.
		got, err := rows.ListByTenant(context.Background(), "tenant-1", audit.Page{})
		if err != nil {
			t.Fatalf("list audit rows: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("the sweep's last audit row never reached the store (%d rows): the join refused the write it was waiting for", len(got))
		}
	})

	t.Run("a write the board dispatcher's drain asks for on its way out is joined too", func(t *testing.T) {
		// The dispatcher's drain is the one OTHER thing the shutdown waits
		// for: its in-flight cards release their claims on a detached
		// context, and the card-processing path they are returning from
		// audits what it decided. A registry closed one step ahead of that
		// wait refuses a write asked for while the process is demonstrably
		// still alive — and reports a clean exit while doing it.
		srv, _ := newJoinTestServer(t)
		srv.bgJoinBudget = 30 * time.Second
		rows := audit.NewMemoryStore()
		srv.auditStore = rows

		boardDone := make(chan struct{})
		srv.stateMu.Lock()
		srv.boardDispDone = boardDone
		srv.stateMu.Unlock()

		closedEarly := make(chan bool, 1)
		go func() {
			// Where an in-flight processCard stands: the shutdown signal has
			// fired, the HTTP drain is over, and the shutdown is parked on
			// this drain.
			<-srv.shutdown
			closedEarly <- awaitRegistryClosed(srv, 500*time.Millisecond)
			srv.auditSystem("tenant-1", "board-dispatcher", "card.claim.released", "run", "run-1", nil)
			close(boardDone)
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if <-closedEarly {
			t.Error("the registry closed while the shutdown was still waiting for the board dispatcher's drain — the refusal's reason (nobody is left to wait) is not true there")
		}
		// Read with no wait of its own: a row that lands after Shutdown
		// returned is a row the grace period did not cover.
		got, err := rows.ListByTenant(context.Background(), "tenant-1", audit.Page{})
		if err != nil {
			t.Fatalf("list audit rows: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("the audit row the drain asked for never reached the store (%d rows): the join closed its registry one wait too early", len(got))
		}
	})

	t.Run("a chain of writes asked for during the join cannot stretch the exit", func(t *testing.T) {
		// The join re-reads its registry so a write asked for by a goroutine
		// it is waiting for is joined too. That must not become an unbounded
		// wait: each link here asks for the next, and the ONE budget is what
		// stops the chain.
		srv, _ := newJoinTestServer(t)
		srv.bgJoinBudget = 150 * time.Millisecond
		const links = 1000
		const perLink = 20 * time.Millisecond
		var mu sync.Mutex
		ran := 0
		var link func()
		link = func() {
			mu.Lock()
			ran++
			n := ran
			mu.Unlock()
			time.Sleep(perLink)
			if n < links {
				srv.goSafe("test.chainedWrite", link)
			}
		}
		srv.goUntilShutdown("test.chainingSweep", func(ctx context.Context) {
			<-ctx.Done()
			link()
		})

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		started := time.Now()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		waited := time.Since(started)
		// The whole chain is links × perLink = 20s of work. Anything near
		// that means the join waited the chain out instead of bounding it;
		// the margin is wide enough that a starved machine only passes.
		if waited > 5*time.Second {
			t.Fatalf("Shutdown took %s: the join followed the chain instead of spending one budget on it", waited)
		}
		mu.Lock()
		done := ran
		mu.Unlock()
		if done >= links {
			t.Fatalf("the whole chain ran (%d links): this test no longer exercises the budget", done)
		}
	})

	t.Run("a write spawned after the join is refused, not left unwatched", func(t *testing.T) {
		srv, logged := newJoinTestServer(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		ran := make(chan struct{})
		srv.goSafe("test.lateWrite", func() { close(ran) })
		select {
		case <-ran:
			t.Fatal("the write ran anyway — nothing is waiting for it, and the process is exiting")
		case <-time.After(100 * time.Millisecond):
		}
		if out := logged(); !strings.Contains(out, "test.lateWrite") {
			t.Fatalf("a dropped write said nothing: %q", out)
		}
	})
}

// claimParkingOAuthStore parks the sweep in the re-read ClaimRefreshRecord
// does UNDER the claim — so the park is observed with the claim already
// persisted — and fails every call on a dead context, the one behaviour
// MongoOAuthStore has (it hands ctx to the driver) and MemoryOAuthStore does
// not (its methods take `_ context.Context`). A fixture that ignored ctx would
// prove the join and silently skip what the shutdown does to the claim.
type claimParkingOAuthStore struct {
	secrets.OAuthStore
	parkOnce sync.Once
	entered  chan struct{}
	release  chan struct{}

	mu         sync.Mutex
	releaseErr error
	released   chan struct{}
}

func (s *claimParkingOAuthStore) ListByUser(ctx context.Context, userID string) ([]secrets.OAuthRecord, error) {
	s.parkOnce.Do(func() {
		close(s.entered)
		<-s.release
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// No row matches the snapshot the claim was taken on: the shape
	// ClaimRefreshRecord hands the claim straight back for, so the sweep needs
	// no provider round trip to exercise the release.
	return nil, nil
}

func (s *claimParkingOAuthStore) ReleaseRefreshClaim(ctx context.Context, id, owner string, notBefore *time.Time) error {
	err := ctx.Err()
	if err == nil {
		err = s.OAuthStore.ReleaseRefreshClaim(ctx, id, owner, notBefore)
	}
	s.mu.Lock()
	s.releaseErr = err
	s.mu.Unlock()
	close(s.released)
	return err
}

// seedDueOAuthRecord stores one refreshable record the sweep will find due.
func seedDueOAuthRecord(t *testing.T, st secrets.OAuthStore, sealer secrets.Sealer, owner string) string {
	t.Helper()
	sealed, err := secrets.SealOAuthPayload(sealer, owner, secrets.OAuthKindClaudeCode,
		[]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-old1234567890abcdef","refreshToken":"rf-old","expiresAt":0}}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	exp := time.Now().Add(5 * time.Minute)
	if err := st.Upsert(context.Background(), secrets.OAuthRecord{
		UserID:               owner,
		Kind:                 secrets.OAuthKindClaudeCode,
		SealedPayload:        sealed,
		AccessTokenExpiresAt: &exp,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec, err := st.Get(context.Background(), owner, secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return rec.ID
}

// The connect path's out-of-band refresh sweep is joined too, and the join is
// ALL it owes: the sweep claims each record before exchanging at the provider
// and hands the claim back on the same context, so a process that exits
// mid-sweep — or a context cancelled at the shutdown signal — leaves a record
// fenced by a claim nobody holds until the lease runs out, RefreshClaimTTL in
// which no replica refreshes that credential.
func TestShutdownJoinsTheOAuthConnectKick(t *testing.T) {
	srv := newMissionTestServer(t)
	srv.bgJoinBudget = 30 * time.Second
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	mem := secrets.NewMemoryOAuthStore()
	recID := seedDueOAuthRecord(t, mem, sealer, "owner-1")
	parking := &claimParkingOAuthStore{
		OAuthStore: mem,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
		released:   make(chan struct{}),
	}
	srv.sealer = sealer
	srv.oauthStore = parking
	srv.cfg.AnthropicOAuthClientID = "test-client-id"

	srv.kickOAuthRefresh("owner-1", secrets.OAuthKindClaudeCode)
	select {
	case <-parking.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the connect kick never reached the store")
	}
	held, err := mem.GetByID(context.Background(), recID)
	if err != nil {
		t.Fatalf("read back the record: %v", err)
	}
	if held.RefreshClaimOwner == "" {
		t.Fatalf("the sweep parked without having claimed the record, so nothing here is fenced: %+v", held)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	shutdownReturned := make(chan error, 1)
	go func() { shutdownReturned <- srv.Shutdown(ctx) }()
	select {
	case <-shutdownReturned:
		t.Fatal("Shutdown returned while the connect kick was mid-sweep — a claimed record is left fenced")
	case <-time.After(100 * time.Millisecond):
	}
	// The shutdown SIGNAL has fired by now (the join above is waiting on this
	// sweep), so what the sweep does next runs with the shutdown under way.
	<-srv.shutdown
	close(parking.release)
	select {
	case <-parking.released:
	case <-time.After(30 * time.Second):
		t.Fatal("the sweep never tried to hand the claim back")
	}
	select {
	case err := <-shutdownReturned:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Shutdown never returned once the sweep had")
	}

	parking.mu.Lock()
	rerr := parking.releaseErr
	parking.mu.Unlock()
	if rerr != nil {
		t.Errorf("handing the claim back failed with %v — the kick runs on a context the shutdown kills, and the record stays fenced for %s", rerr, secrets.RefreshClaimTTL)
	}
	after, err := mem.GetByID(context.Background(), recID)
	if err != nil {
		t.Fatalf("read back the record: %v", err)
	}
	if after.RefreshClaimOwner != "" {
		t.Errorf("the record came out of the shutdown still claimed by %q until %v: no replica refreshes this credential for %s",
			after.RefreshClaimOwner, after.RefreshNotBefore, secrets.RefreshClaimTTL)
	}
}

// The model-spec refresh is a shared flight: one goroutine fetches and rewrites
// the on-disk cache while every waiting request parks on its channel. It is a
// WRITE, so it follows goSafe's policy rather than a loop's: started on a
// context of its own — including during the HTTP drain, where requests are
// still answered — joined until the rewrite is done, and refused only past the
// join, where it must answer its waiters rather than park them on a channel
// nothing will ever close.
func TestShutdownJoinsTheModelsRefresh(t *testing.T) {
	t.Run("a refresh still in flight holds the exit", func(t *testing.T) {
		srv := newMissionTestServer(t)
		srv.bgJoinBudget = 30 * time.Second
		entered := make(chan struct{})
		release := make(chan struct{})
		srv.modelSpecsRefresh = func(context.Context) error {
			close(entered)
			<-release
			return nil
		}
		refreshed := make(chan error, 1)
		go func() { refreshed <- srv.refreshModels(context.Background()) }()
		<-entered

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		shutdownReturned := make(chan error, 1)
		go func() { shutdownReturned <- srv.Shutdown(ctx) }()
		select {
		case <-shutdownReturned:
			t.Fatal("Shutdown returned while the model-spec refresh was still rewriting its cache")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		select {
		case err := <-shutdownReturned:
			if err != nil {
				t.Fatalf("shutdown: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Shutdown never returned once the refresh had")
		}
		select {
		case <-refreshed:
		case <-time.After(10 * time.Second):
			t.Fatal("the waiting request was never answered")
		}
	})

	t.Run("the join covers the cache rewrite, not just the wait for it", func(t *testing.T) {
		// modelspecs.Registry.Refresh runs the shared fetch and the cache
		// rewrite on a goroutine of its OWN under context.Background(), and
		// gives the caller's context only the WAIT for it. A refresh whose
		// wait the shutdown cancels therefore returns at once and leaves the
		// rewrite running outside the registry — joined in name only. This
		// fixture has that shape.
		srv := newMissionTestServer(t)
		srv.cfg.Mode = "cloud" // the local detector probe is not under test here
		srv.bgJoinBudget = 30 * time.Second
		writing := make(chan struct{})
		release := make(chan struct{})
		rewritten := make(chan struct{})
		srv.modelSpecsRefresh = func(ctx context.Context) error {
			go func() {
				close(writing)
				<-release
				close(rewritten)
			}()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-rewritten:
				return nil
			}
		}
		refreshed := make(chan error, 1)
		go func() { refreshed <- srv.refreshModels(context.Background()) }()
		<-writing

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		shutdownReturned := make(chan error, 1)
		go func() { shutdownReturned <- srv.Shutdown(ctx) }()
		select {
		case <-shutdownReturned:
			close(release)
			t.Fatal("Shutdown returned while the model-spec cache rewrite was still in flight: the join covered this process's wait for the refresh, not the refresh")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		select {
		case err := <-shutdownReturned:
			if err != nil {
				t.Fatalf("shutdown: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Shutdown never returned once the rewrite had")
		}
		select {
		case <-rewritten:
		default:
			t.Fatal("Shutdown returned before the cache rewrite finished")
		}
		select {
		case <-refreshed:
		case <-time.After(10 * time.Second):
			t.Fatal("the waiting request was never answered")
		}
	})

	t.Run("a refresh asked during the HTTP drain is still started", func(t *testing.T) {
		// The shutdown signal fires BEFORE http.Server.Shutdown, so
		// /api/models?refresh=1 is still answered for the whole drain — and
		// the join comes after it. A refresh refused in that window answers
		// the picker with a refresh error plus the stale catalog, on every
		// rolling update, for nothing: the write is joined like any other.
		srv := newMissionTestServer(t)
		srv.cfg.Mode = "cloud"
		ran := make(chan struct{})
		srv.modelSpecsRefresh = func(context.Context) error { close(ran); return nil }
		close(srv.shutdown)

		if err := srv.refreshModels(context.Background()); err != nil {
			t.Fatalf("a refresh answered during the HTTP drain was refused: %v", err)
		}
		select {
		case <-ran:
		default:
			t.Fatal("the refresh never ran, and its waiter was answered anyway")
		}
		if !hasWorkerNaming(srv, "models-refresh") {
			t.Fatalf("the drain-window refresh was never registered for the join: %v", registeredWorkerNames(srv))
		}
	})

	t.Run("a refresh that panics answers its waiters instead of parking them", func(t *testing.T) {
		// The flight is installed before the write is asked for, and goSafe
		// RECOVERS the panic — so a task that publishes its outcome only on
		// the way out would leave the flight on a channel nothing closes, and
		// every later /api/models?refresh=1 would park on it for the
		// remaining life of the process.
		srv, _ := newJoinTestServer(t)
		srv.cfg.Mode = "cloud"
		srv.modelSpecsRefresh = func(context.Context) error { panic("the aggregator client blew up") }

		answered := make(chan error, 1)
		go func() { answered <- srv.refreshModels(context.Background()) }()
		select {
		case err := <-answered:
			if err == nil {
				t.Fatal("a refresh that panicked answered its waiter with success")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a refresh that panicked never answered its waiters — they are parked on a channel nothing closes")
		}
		srv.modelsRefreshMu.Lock()
		inFlight := srv.modelsRefresh
		srv.modelsRefreshMu.Unlock()
		if inFlight != nil {
			t.Fatal("the panicking flight was left installed — every later request joins a refresh that will never run")
		}
	})

	t.Run("a refresh asked after the join answers instead of parking its waiters", func(t *testing.T) {
		srv, _ := newJoinTestServer(t)
		srv.modelSpecsRefresh = func(context.Context) error { return nil }
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		answered := make(chan error, 1)
		go func() { answered <- srv.refreshModels(context.Background()) }()
		select {
		case <-answered:
		case <-time.After(10 * time.Second):
			t.Fatal("a refresh asked after the join never answered — its waiters are parked on a channel nothing closes")
		}
		srv.modelsRefreshMu.Lock()
		inFlight := srv.modelsRefresh
		srv.modelsRefreshMu.Unlock()
		if inFlight != nil {
			t.Fatal("the refused flight was left installed — every later request joins a refresh that never runs")
		}
	})
}
