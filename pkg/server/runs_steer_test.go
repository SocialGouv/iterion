package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func steerPost(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// TestSteerRoutes_TruthfulContract exercises the HTTP mapping of the
// steering contract: 404 unknown run, 409 terminal, 409 not-held (a
// "running" run no process holds), 400 invalid command.
func TestSteerRoutes_TruthfulContract(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "r-done", "wf", store.RunStatusFinished)
	seedRun(t, srv, "r-live", "wf", store.RunStatusRunning)

	t.Run("bump unknown run 404", func(t *testing.T) {
		resp := steerPost(t, hs.URL+"/api/runs/absent/bump-loop", map[string]any{"loop_name": "l", "delta": 1})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("bump terminal run 409", func(t *testing.T) {
		resp := steerPost(t, hs.URL+"/api/runs/r-done/bump-loop", map[string]any{"loop_name": "l", "delta": 1})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})

	t.Run("bump not-held run 409", func(t *testing.T) {
		resp := steerPost(t, hs.URL+"/api/runs/r-live/bump-loop", map[string]any{"loop_name": "l", "delta": 1})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})

	t.Run("raise with empty budget 400", func(t *testing.T) {
		resp := steerPost(t, hs.URL+"/api/runs/r-live/raise-budget", map[string]any{"budget": map[string]any{}})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("raise with bad duration 400", func(t *testing.T) {
		resp := steerPost(t, hs.URL+"/api/runs/r-live/raise-budget", map[string]any{"budget": map[string]any{"max_duration": "5x"}})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("answer-human on running run 409", func(t *testing.T) {
		resp := steerPost(t, hs.URL+"/api/runs/r-live/answer-human", map[string]any{"answers": map[string]any{"ok": true}})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
	})

	t.Run("answer-human without answers 400", func(t *testing.T) {
		resp := steerPost(t, hs.URL+"/api/runs/r-live/answer-human", map[string]any{})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})
}
