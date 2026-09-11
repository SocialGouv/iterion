package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A host event is an HTTP acknowledgement, not the lifetime of Copi's next
// turn. It must also adopt the current source after a deploy: otherwise a
// completed action gets a source-drift 500 and the assistant never learns its
// result. In in-process mode closing this response used to cancel the resumed
// engine immediately, leaving Copi terminal before it could handle the action
// result that woke it.
func TestDeliverHostEvent_AdoptsCurrentSourceAndContinuesPastHTTPRequestEnd(t *testing.T) {
	t.Setenv("ITERION_RUNS_DETACHED", "0")
	srv, hs := newTestServer(t)

	const source = `
schema chat_out:
  message: string
  host_event: json

schema work_out:
  done: bool

human chat:
  output: chat_out
  interaction: human_or_host

tool work:
  command: ` + "`sleep 0.3; printf '{\"done\":true}'`" + `
  output: work_out

workflow host_event_continues:
  entry: chat

  chat -> work
  work -> done
`

	botsRoot := filepath.Join(srv.cfg.WorkDir, "bots")
	bundleDir := filepath.Join(botsRoot, "chatbot")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	botPath := filepath.Join(bundleDir, "main.bot")
	if err := os.WriteFile(botPath, []byte(source), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	const manifest = `name: chatbot
chat:
  nodes:
    chat:
      kind: human
      text_field: message
      host_event_field: host_event
`
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	srv.cfg.Bots.Paths = []string{botsRoot}

	const runID = "run-host-event-continues-after-http"
	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open run store: %v", err)
	}
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "host_event_continues", nil); err != nil {
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
	run.BotID = "chatbot"
	run.FilePath = botPath
	run.WorkflowHash = workflowHash
	run.WorkflowSource = source
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	interactionID := runID + "_chat"
	questions := map[string]any{}
	if err := st.WriteInteraction(ctx, &store.Interaction{
		ID: interactionID, RunID: runID, NodeID: "chat",
		RequestedAt: time.Now().UTC(), Questions: questions,
	}); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	if err := st.PauseRun(ctx, runID, &store.Checkpoint{
		NodeID:               "chat",
		InteractionID:        interactionID,
		InteractionQuestions: questions,
		Outputs:              map[string]map[string]any{},
		LoopCounters:         map[string]int{},
		ArtifactVersions:     map[string]int{},
		Vars:                 map[string]any{},
	}); err != nil {
		t.Fatalf("PauseRun: %v", err)
	}
	// Simulate a safe Copi redeploy after the action card was rendered. The
	// paused chat node is unchanged; only the source hash drifts.
	if err := os.WriteFile(botPath, []byte(source+"\n## deployed after card\n"), 0o600); err != nil {
		t.Fatalf("update workflow source: %v", err)
	}

	resp := steerPost(t, hs.URL+"/api/runs/"+runID+"/host-event", map[string]any{
		"kind": "action-completed",
		"event": map[string]any{
			"action": "run.watch",
			"args": map[string]any{
				"target_run_id": "run-target",
				"mode":          "diagnose",
			},
			"message": "Watching run run-target for failures",
		},
	})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, http.StatusOK, body)
	}
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err = st.LoadRun(ctx, runID)
		if err != nil {
			t.Fatalf("LoadRun after host event: %v", err)
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
