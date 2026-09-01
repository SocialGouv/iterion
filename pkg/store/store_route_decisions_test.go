package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The registry is run-scoped state and rides the package's own file
// conventions: 0o600 like every other run file (filePerm), and written
// through the atomic helper. A plain os.WriteFile is what this pins
// against — it left a truncated route_decisions.json on a SIGKILL,
// which loadRouteDecisions could never decode again, so every later
// ClaimRouteDecision errored and the run became unroutable with its
// whole decision audit gone. The atomicity is also what lets
// ListRoutableRuns read these files without the store mutex.
func TestRouteDecisionsFileFollowsStoreFileConventions(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	const runID = "run-perm"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if claimed, _, err := s.ClaimRouteDecision(ctx, RouteDecision{RunID: runID, OutcomeSeq: 1, Decision: "merge"}, time.Now().Add(-RouteClaimLease)); err != nil || !claimed {
		t.Fatalf("claim = (%t, %v)", claimed, err)
	}

	fi, err := os.Stat(s.routeDecisionsPath(runID))
	if err != nil {
		t.Fatalf("stat route_decisions.json: %v", err)
	}
	if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("route_decisions.json mode = %04o, want %04o (the package's run-scoped filePerm)", got, filePerm)
	}
	// No staging file survives the write: the atomic helper renames its
	// temp into place, a plain write leaves nothing to clean but also
	// never staged anything — so this only ever fails on a half-done
	// atomic write, which is the state the whole helper exists to avoid.
	entries, err := os.ReadDir(filepath.Dir(s.routeDecisionsPath(runID)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 0 && e.Name()[0] == '.' {
			t.Errorf("write left a staging file behind: %s", e.Name())
		}
	}

	// The watermark is the router's activation instant and is
	// first-writer-wins: once established, no later call moves it.
	wm1, err := s.EnsureRouterWatermark(ctx)
	if err != nil || wm1.IsZero() {
		t.Fatalf("EnsureRouterWatermark = (%v, %v)", wm1, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "router_watermark.json")); err != nil {
		t.Fatalf("stat router_watermark.json: %v", err)
	} else if got := fi.Mode().Perm(); got != filePerm {
		t.Errorf("router_watermark.json mode = %04o, want %04o", got, filePerm)
	}
	wm2, err := s.EnsureRouterWatermark(ctx)
	if err != nil {
		t.Fatalf("EnsureRouterWatermark (second): %v", err)
	}
	if !wm2.Equal(wm1) {
		t.Errorf("watermark moved: %v then %v — a later instant skips every terminal in between", wm1, wm2)
	}
}

// ListRoutableRuns deliberately holds no lock: s.mu is the exclusive
// mutex every run write serialises on, and taking it across a whole-
// directory scan stalled every write in the process once a minute for
// as long as the router was on. This is the proof that dropping it is
// safe — the scan runs against a store being written concurrently, so
// the race detector (CI's `race` job) fails on any unsafety the removal
// would have introduced. It also fails plainly if a concurrent write
// can make the scan error, which is what a torn read would look like.
func TestListRoutableRunsScansConcurrentlyWithWrites(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	pol := &RoutingPolicy{Version: 1, SuccessWhen: "outputs.g.ok", AllowedActions: []string{"merge"}}
	pol.Hash = pol.ComputeHash()

	const n = 12
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("run-%02d", i)
		ids = append(ids, id)
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
	}

	// Writers: the exact calls that serialise on s.mu.
	var writers, reader sync.WaitGroup
	for _, id := range ids {
		writers.Add(1)
		go func(id string) {
			defer writers.Done()
			for i := 0; i < 8; i++ {
				r, err := s.LoadRun(ctx, id)
				if err != nil {
					t.Errorf("LoadRun %s: %v", id, err)
					return
				}
				r.RoutingPolicy = pol
				r.Status = RunStatusFinished
				if err := s.SaveRun(ctx, r); err != nil {
					t.Errorf("SaveRun %s: %v", id, err)
					return
				}
				if _, _, err := s.ClaimRouteDecision(ctx, RouteDecision{RunID: id, OutcomeSeq: 1, Decision: "merge"}, time.Now().Add(-RouteClaimLease)); err != nil {
					t.Errorf("claim %s: %v", id, err)
					return
				}
			}
		}(id)
	}
	// Reader: the unlocked scan, on repeat until the writers are done.
	stop := make(chan struct{})
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 50); err != nil {
				t.Errorf("ListRoutableRuns during concurrent writes: %v", err)
				return
			}
		}
	}()
	writers.Wait()
	close(stop)
	reader.Wait()

	// Non-vacuous: the scan really did have candidates to read.
	got, err := s.ListRoutableRuns(ctx, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("final ListRoutableRuns: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the scan found nothing — the concurrency proof ran over an empty candidate set")
	}
}
