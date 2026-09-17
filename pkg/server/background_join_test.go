package server

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// lockedBuffer collects log output written from the server's own goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newJoinTestServer is newMissionTestServer with the log level the join's
// overrun warning is emitted at, captured instead of printed.
func newJoinTestServer(t *testing.T) (*Server, func() string) {
	t.Helper()
	workDir := t.TempDir()
	out := &lockedBuffer{}
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelWarn, out))
	return srv, out.String
}

// awaitRegisteredLoop waits for the channel the shutdown will join — not the
// one the loop body closes. The two differ by the deferred close that runs
// after the body returns, and a test that waits on the wrong one races it.
func awaitRegisteredLoop(t *testing.T, srv *Server, name string) {
	t.Helper()
	srv.stateMu.Lock()
	var done <-chan struct{}
	for _, w := range srv.bgWorkers {
		if w.name == name {
			done = w.done
		}
	}
	srv.stateMu.Unlock()
	if done == nil {
		t.Fatalf("loop %q was never registered for the shutdown to join", name)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("loop %q did not return", name)
	}
}

// A background loop is JOINED by the shutdown, not merely cancelled by it.
// The distinction is the whole point: a loop stopped by a cancel alone returns
// after its cancel did, so the process can exit between a claim and its
// release, or mid-write into a store already being torn down — and the grace
// period the chart provisions never covered that last write.
func TestShutdownJoinsTheLoopsItCancels(t *testing.T) {
	t.Run("a loop still writing when its cancel lands holds the exit", func(t *testing.T) {
		srv := newMissionTestServer(t)
		// The join budget must not race the observation window below. At the
		// production 500ms the two are a factor of five apart, which is a bet
		// on scheduling latency; widened here they are three hundred, and a
		// starved machine can only make this test WAIT longer.
		srv.bgJoinBudget = 30 * time.Second
		entered := make(chan struct{})
		release := make(chan struct{})
		returned := make(chan struct{})
		srv.goUntilShutdown("test.writingLoop", func(ctx context.Context) {
			close(entered)
			<-ctx.Done()
			// The last write: begun before the cancel, still going after it.
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
			t.Fatal("Shutdown returned while the loop was still inside its last write")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		select {
		case err := <-shutdownReturned:
			if err != nil {
				t.Fatalf("shutdown: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Shutdown never returned once the loop had")
		}
		select {
		case <-returned:
		default:
			t.Fatal("Shutdown reported done before the loop had returned")
		}
	})

	t.Run("the exit costs one budget however many loops are unresponsive", func(t *testing.T) {
		// Eight loops that never return. Under one budget the exit costs one;
		// under a budget spent per loop it costs eight, and the arithmetic
		// upstream — which provisions ShutdownDelay + teardown and nothing
		// else — is wrong by however many loops this server happens to start.
		//
		// What this falsifies is a per-loop BUDGET. It says nothing about
		// serial versus concurrent waiting, and cannot: against one shared
		// deadline the two are the same wall-clock, which is exactly why the
		// implementation is the simpler serial one.
		const stuck = 8
		srv := newMissionTestServer(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		for i := range stuck {
			entered := make(chan struct{})
			srv.goUntilShutdown(fmt.Sprintf("test.stuckLoop%d", i), func(context.Context) {
				close(entered)
				<-release
			})
			<-entered
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		started := time.Now()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if perLoop := stuck * srv.backgroundJoinBudget(); time.Since(started) >= perLoop {
			t.Fatalf("the exit waited %s on %d unresponsive loops — at or past the %s a budget spent per loop would cost",
				time.Since(started), stuck, perLoop)
		}
	})

	t.Run("the overrun names the loops that overran, and only those", func(t *testing.T) {
		srv, logged := newJoinTestServer(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		entered := make(chan struct{})
		srv.goUntilShutdown("test.stuckLoop", func(context.Context) {
			close(entered)
			<-release
		})
		<-entered
		// Eight loops that finish before the join asks. Against a spent budget,
		// a join that does not ask "already returned?" first decides each of
		// them by coin flip — naming even one is the defect, and with eight the
		// mutation has one chance in 256 of going unnoticed.
		//
		// They are released only once all eight are registered: a loop that has
		// already returned when the NEXT one registers is compacted out of the
		// registry, and eight witnesses would become one.
		promptRelease := make(chan struct{})
		names := make([]string, 0, 8)
		for i := range 8 {
			name := fmt.Sprintf("test.promptLoop%d", i)
			names = append(names, name)
			srv.goUntilShutdown(name, func(context.Context) { <-promptRelease })
		}
		close(promptRelease)
		for _, name := range names {
			awaitRegisteredLoop(t, srv, name)
		}

		expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		srv.joinBackgroundWorkers(expired)

		out := logged()
		if !strings.Contains(out, "test.stuckLoop") {
			t.Fatalf("the overrun warning does not name the loop that overran: %q", out)
		}
		if strings.Contains(out, "test.promptLoop") {
			t.Fatalf("the overrun warning names a loop that had already returned: %q", out)
		}
	})

	t.Run("the warning reports the wait that happened, not the budget", func(t *testing.T) {
		// A caller whose own deadline is already spent gets no window at all:
		// context.WithTimeout on an expired parent is already Done. Reporting
		// the budget there sends an operator hunting a slow loop on the
		// strength of a wait that never took place.
		srv, logged := newJoinTestServer(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		entered := make(chan struct{})
		srv.goUntilShutdown("test.stuckLoop", func(context.Context) {
			close(entered)
			<-release
		})
		<-entered

		expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		srv.joinBackgroundWorkers(expired)

		out := logged()
		if !strings.Contains(out, "test.stuckLoop") {
			t.Fatalf("the warning does not name the loop that was still running: %q", out)
		}
		if claimed := "waited " + srv.backgroundJoinBudget().String(); strings.Contains(out, claimed) {
			t.Fatalf("the warning claims %q on a caller whose deadline was already gone: %q", claimed, out)
		}
	})

	t.Run("a loop started after the join is refused, not left unwatched", func(t *testing.T) {
		// Boot and shutdown overlap: the signal arm calls Shutdown while
		// ListenAndServe is still wiring loops. A loop registered past the
		// join's snapshot would be started with nobody left to wait for it —
		// the very defect, inside the mechanism meant to close it.
		srv, logged := newJoinTestServer(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}

		ran := make(chan struct{})
		srv.goUntilShutdown("test.lateLoop", func(context.Context) { close(ran) })

		// The refusal must be audible — this is what falsifies the guard.
		if out := logged(); !strings.Contains(out, "test.lateLoop") {
			t.Fatalf("a loop was started after the join with no word said: %q", out)
		}
		select {
		case <-ran:
			t.Fatal("the loop ran anyway — nothing is waiting for its last write")
		case <-time.After(100 * time.Millisecond):
		}
		srv.stateMu.Lock()
		registered := len(srv.bgWorkers)
		srv.stateMu.Unlock()
		if registered != 0 {
			t.Fatalf("the refused loop was registered anyway (%d entries) — a join that never comes", registered)
		}
	})
}

// The handle goUntilShutdown returns stops its loop on its own, without the
// shutdown signal — what a caller holding a stop of its own (the board sync
// worker, the outcome router) relies on.
func TestGoUntilShutdownStopsOneLoopWithoutTheShutdownSignal(t *testing.T) {
	srv := newMissionTestServer(t)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	ran := make(chan struct{})
	stop := srv.goUntilShutdown("test.earlyStop", func(ctx context.Context) {
		close(ran)
		<-ctx.Done()
	})
	<-ran
	select {
	case <-srv.shutdown:
		t.Fatal("the server was already shutting down — the stop below would prove nothing")
	default:
	}
	stop()
	awaitRegisteredLoop(t, srv, "test.earlyStop")
}
