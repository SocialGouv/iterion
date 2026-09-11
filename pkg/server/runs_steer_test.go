package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
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

func TestAnswerHuman_HTTPRequestEndDoesNotCancelResumedRun(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	srv, hs := newTestServer(t)

	const source = `
schema gate_out:
  approved: bool

schema work_out:
  done: bool

human gate:
  output: gate_out
  interaction: human

tool work:
  command: ` + "`sleep 0.3; printf '{\"done\":true}'`" + `
  output: work_out

workflow answer_continues:
  entry: gate
  gate -> work when approved
  gate -> fail when not approved
  work -> done
`

	const runID = "run-answer-continues-after-http"
	botPath := filepath.Join(srv.cfg.WorkDir, "answer-continues.bot")
	if err := os.WriteFile(botPath, []byte(source), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open run store: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "answer_continues", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	_, workflowHash, err := runview.CompileWorkflowFromSource(botPath, source)
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	run.FilePath = botPath
	run.WorkflowHash = workflowHash
	run.WorkflowSource = source
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	interactionID := runID + "_gate"
	questions := map[string]any{"approved": "Continue?"}
	if err := st.WriteInteraction(ctx, &store.Interaction{
		ID: interactionID, RunID: runID, NodeID: "gate",
		RequestedAt: time.Now().UTC(), Questions: questions,
	}); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	if err := st.PauseRun(ctx, runID, &store.Checkpoint{
		NodeID:               "gate",
		InteractionID:        interactionID,
		InteractionQuestions: questions,
		Outputs:              map[string]map[string]any{},
		LoopCounters:         map[string]int{},
		ArtifactVersions:     map[string]int{},
		Vars:                 map[string]any{},
	}); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}

	resp := steerPost(t, hs.URL+"/api/runs/"+runID+"/answer-human", map[string]any{
		"answers": map[string]any{"approved": true},
	})
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, http.StatusAccepted, body)
	}
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err = st.LoadRun(ctx, runID)
		if err != nil {
			t.Fatalf("LoadRun after answer: %v", err)
		}
		if run.Status == store.RunStatusFinished {
			return
		}
		if run.Status.IsTerminal() || run.Status == store.RunStatusFailedResumable {
			t.Fatalf("resumed run stopped as %s: %s", run.Status, run.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("resumed run did not finish before deadline; last status = %s", run.Status)
}
