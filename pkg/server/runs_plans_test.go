package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestListPlans_Endpoint pins the /api/runs/:id/plans contract: valid run
// with no plans → empty array (200, not 404); captured snapshots come back
// in ascending seq order with their todos intact.
func TestListPlans_Endpoint(t *testing.T) {
	srv, hs := newTestServer(t)
	const runID = "plan-run"
	seedRun(t, srv, runID, "wf", store.RunStatusFinished)

	// Empty case: no plans/ dir → [] and 200.
	t.Run("empty when no plans captured", func(t *testing.T) {
		plans := getPlans(t, hs.URL, runID)
		if len(plans) != 0 {
			t.Fatalf("expected empty plans, got %d", len(plans))
		}
	})

	// Seed two snapshots directly on disk (the shape the capture hook writes).
	dir := filepath.Join(srv.cfg.StoreDir, "runs", runID, "plans")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir plans: %v", err)
	}
	snaps := []store.PlanSnapshot{
		{Seq: 0, NodeID: "campaign", Iteration: 0, Tool: "TodoWrite", Todos: []store.PlanTodo{{Content: "a", Status: "in_progress"}}},
		{Seq: 1, NodeID: "campaign", Iteration: 1, Tool: "TodoWrite", Todos: []store.PlanTodo{{Content: "a", Status: "completed"}, {Content: "b", Status: "pending"}}},
	}
	for _, s := range snaps {
		b, _ := json.MarshalIndent(s, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, padSeq(s.Seq)), b, 0o600); err != nil {
			t.Fatalf("write plan %d: %v", s.Seq, err)
		}
	}

	t.Run("returns captured snapshots in ascending seq order", func(t *testing.T) {
		plans := getPlans(t, hs.URL, runID)
		if len(plans) != 2 {
			t.Fatalf("expected 2 plans, got %d", len(plans))
		}
		if plans[0].Seq != 0 || plans[1].Seq != 1 {
			t.Errorf("expected ascending [0,1], got [%d,%d]", plans[0].Seq, plans[1].Seq)
		}
		if len(plans[1].Todos) != 2 || plans[1].Todos[0].Status != "completed" {
			t.Errorf("todos not round-tripped: %+v", plans[1].Todos)
		}
	})

	t.Run("unknown run is 404", func(t *testing.T) {
		resp, err := http.Get(hs.URL + "/api/runs/does-not-exist/plans")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

func getPlans(t *testing.T, base, runID string) []store.PlanSnapshot {
	t.Helper()
	resp, err := http.Get(base + "/api/runs/" + runID + "/plans")
	if err != nil {
		t.Fatalf("GET plans: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Plans []store.PlanSnapshot `json:"plans"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out.Plans
}

func padSeq(n int) string {
	// Mirror the store's %04d.json naming.
	s := "0000"
	d := []byte(s)
	i := len(d) - 1
	for n > 0 && i >= 0 {
		d[i] = byte('0' + n%10)
		n /= 10
		i--
	}
	return string(d) + ".json"
}
