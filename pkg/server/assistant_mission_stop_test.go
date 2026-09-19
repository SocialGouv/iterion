package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/assistantmission"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runwatch"
)

// newMissionTestServer is a local server the test shuts down ITSELF, once:
// Shutdown is not idempotent (the file watcher's Stop closes a channel), so
// the shared newTestServer, whose cleanup shuts down too, cannot serve a
// test whose subject is the shutdown.
func newMissionTestServer(t *testing.T) *Server {
	t.Helper()
	workDir := t.TempDir()
	srv := New(Config{
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, os.Stderr))
	if srv.runs == nil {
		t.Fatal("expected the run console service to be wired")
	}
	return srv
}

// parkingMissions is a mission store whose candidate listing parks until
// released and then WRITES into the store root — the shape of #1254: a sweep
// landing a filesystem write after its coordinator was cancelled. Only the
// first call parks; later sweeps of the same coordinator, or of the next
// one, pass straight through.
type parkingMissions struct {
	assistantmission.Store
	dir     string
	entered chan struct{}
	release chan struct{}
}

func (p *parkingMissions) ListReconcileCandidates(ctx context.Context, owner string, now time.Time, limit int) ([]assistantmission.Mission, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-p.release
	_ = os.MkdirAll(p.dir, 0o700)
	_ = os.WriteFile(filepath.Join(p.dir, "late-write.json"), []byte("{}\n"), 0o600)
	return nil, nil
}

func newParkingMissions(t *testing.T) *parkingMissions {
	t.Helper()
	dir := t.TempDir()
	return &parkingMissions{Store: assistantmission.NewFSStore(dir), dir: dir, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

// awaitEntered fails the test unless the first sweep reached the store.
func awaitEntered(t *testing.T, p *parkingMissions) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first sweep never reached the store")
	}
}

// When a coordinator's done channel closes, its sweep has stopped touching
// the store: the store root can be removed and stays removed. A done that
// closed while the sweep was still writing would let the write land in a
// directory being removed — #1254.
func TestAssistantMissionDoneClosesAfterTheSweepsLastWrite(t *testing.T) {
	p := newParkingMissions(t)
	s := &Server{}
	if prev := s.restartAssistantMissions(nil, runwatch.NewFSStore(t.TempDir()), p); prev != nil {
		t.Fatal("a first restart returned a previous loop")
	}
	awaitEntered(t, p)

	done := s.stopAssistantMissions(context.Background())
	if done == nil {
		t.Fatal("stop returned no loop to join")
	}
	select {
	case <-done:
		t.Fatal("done closed while the sweep was still inside a store call")
	case <-time.After(100 * time.Millisecond):
	}
	close(p.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done did not close once the sweep had returned")
	}
	if err := os.RemoveAll(p.dir); err != nil {
		t.Fatalf("remove the store root once done closed: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if entries, err := os.ReadDir(p.dir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the sweep wrote into the store root after done closed: %v", names)
	}
}

// A restart hands back the previous coordinator's done channel, so the
// caller can join the loop it cancelled: the channel is not closed while
// the previous sweep is still inside a store call, and closes once it
// returns.
func TestRestartAssistantMissionsReturnsThePreviousLoopToJoin(t *testing.T) {
	p := newParkingMissions(t)
	s := &Server{}
	watches := runwatch.NewFSStore(t.TempDir())
	s.restartAssistantMissions(nil, watches, p)
	awaitEntered(t, p)

	prev := s.restartAssistantMissions(nil, watches, assistantmission.NewFSStore(t.TempDir()))
	if prev == nil {
		t.Fatal("the restart returned no previous loop")
	}
	select {
	case <-prev:
		t.Fatal("the previous loop was reported gone while its sweep was still inside a store call")
	case <-time.After(100 * time.Millisecond):
	}
	close(p.release)
	select {
	case <-prev:
	case <-time.After(5 * time.Second):
		t.Fatal("the previous loop was not reported gone once its sweep had returned")
	}
	if done := s.stopAssistantMissions(context.Background()); done != nil {
		<-done
	}
}

// The shutdown joins the mission sweep after the HTTP drain: a sweep that
// returns within the budget is gone when Shutdown returns; one that does not
// is not waited for past backgroundJoinBudget, which every background loop
// shares, so the exit sequence keeps the arithmetic of its grace period.
func TestShutdownJoinsTheMissionSweepWithinItsBudget(t *testing.T) {
	t.Run("a sweep that returns is joined", func(t *testing.T) {
		srv := newMissionTestServer(t)
		// Well past the 100ms the sweep parks for below, so a starved machine
		// can only make this test wait longer, never decide it the other way.
		srv.bgJoinBudget = 30 * time.Second
		p := newParkingMissions(t)
		srv.restartAssistantMissions(srv.runs, srv.assistantWatches, p)
		awaitEntered(t, p)
		srv.stateMu.RLock()
		done := srv.assistantMissionDone
		srv.stateMu.RUnlock()

		go func() {
			time.Sleep(100 * time.Millisecond)
			close(p.release)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		select {
		case <-done:
		default:
			t.Fatal("Shutdown returned before the mission sweep had")
		}
	})

	t.Run("a sweep that does not return is not waited for past the budget", func(t *testing.T) {
		srv := newMissionTestServer(t)
		p := newParkingMissions(t)
		srv.restartAssistantMissions(srv.runs, srv.assistantWatches, p)
		awaitEntered(t, p)
		srv.stateMu.RLock()
		done := srv.assistantMissionDone
		srv.stateMu.RUnlock()
		t.Cleanup(func() {
			close(p.release)
			<-done
		})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		started := time.Now()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("Shutdown waited %s for a sweep that never returned", elapsed)
		}
	})
}
