package store

import (
	"context"
	"os"
	"path/filepath"
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
