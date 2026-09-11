package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Recovery-passive is intentionally tested through ListenAndServe rather
// than only through handlers: the dangerous work happens during boot, after
// the listener is opened. The mode must still present an honest, usable
// local console while leaving all autonomous coordinators absent.
func TestRecoveryPassiveStartsNoAutonomousCoordinators(t *testing.T) {
	workDir := t.TempDir()
	storeDir := filepath.Join(workDir, ".iterion")
	seed, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateRun(context.Background(), "must-stay-running", "demo", nil); err != nil {
		t.Fatal(err)
	}
	if err := seed.UpdateRunStatus(context.Background(), "must-stay-running", store.RunStatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	srv := New(Config{
		Port:                    0,
		Bind:                    "127.0.0.1",
		WorkDir:                 workDir,
		StoreDir:                storeDir,
		RecoveryPassive:         true,
		SkipProjectRegistration: true,
		DisableAuth:             true,
	}, iterlog.New(iterlog.LevelError, os.Stderr))
	stillRunning, err := seed.LoadRun(context.Background(), "must-stay-running")
	if err != nil {
		t.Fatal(err)
	}
	if stillRunning.Status != store.RunStatusRunning {
		t.Fatalf("recovery-passive boot changed run status to %s", stillRunning.Status)
	}

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("recovery-passive listener did not bind")
	}

	if srv.watcher != nil {
		t.Fatal("recovery-passive started the file watcher")
	}
	if srv.watchCoord != nil || srv.triggerCoord != nil || srv.cloudTriggerCoord != nil {
		t.Fatal("recovery-passive started a board or trigger coordinator")
	}
	if srv.assistantWatch != nil || srv.assistantWatchCancel != nil {
		t.Fatal("recovery-passive started the assistant watch coordinator")
	}
	if srv.localEvents == nil {
		t.Fatal("recovery-passive did not retain the explicit-run event path")
	}

	client := &http.Client{Timeout: 2 * time.Second}
	for _, path := range []string{"/api/server/info", "/readyz"} {
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		var payload struct {
			RecoveryPassive bool `json:"recovery_passive"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			_ = resp.Body.Close()
			t.Fatalf("decode %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if !payload.RecoveryPassive {
			t.Fatalf("%s did not report recovery_passive", path)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ListenAndServe = %v, want http.ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recovery-passive listener did not stop")
	}
}

// The passive mode must not turn the console into read-only mode. This uses
// the production listener and the same host-event route a confirmed Copi
// action uses: its own paused chat can continue, while no watch coordinator
// is allowed to consume or create an autonomous episode.
func TestRecoveryPassiveAllowsExplicitAssistantHostEvent(t *testing.T) {
	workDir := t.TempDir()
	botsRoot := filepath.Join(workDir, "bots")
	bundleDir := filepath.Join(botsRoot, "chatbot")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("mkdir bot bundle: %v", err)
	}
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
  command: ` + "`printf '{\"done\":true}'`" + `
  output: work_out

workflow passive_host_event:
  entry: chat

  chat -> work
  work -> done
`
	botPath := filepath.Join(bundleDir, "main.bot")
	if err := os.WriteFile(botPath, []byte(source), 0o600); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte("name: chatbot\nchat:\n  nodes:\n    chat:\n      kind: human\n      text_field: message\n      host_event_field: host_event\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	srv := New(Config{
		Port:                    0,
		Bind:                    "127.0.0.1",
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		RecoveryPassive:         true,
		SkipProjectRegistration: true,
		DisableAuth:             true,
		Bots:                    BotsConfig{Paths: []string{botsRoot}},
	}, iterlog.New(iterlog.LevelError, os.Stderr))
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("listener did not bind")
	}

	st, err := store.New(srv.cfg.StoreDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	const runID = "passive-host-event"
	if _, err := st.CreateRun(ctx, runID, "passive_host_event", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	_, hash, err := runview.CompileWorkflowFromSource(botPath, source)
	if err != nil {
		t.Fatalf("compile workflow: %v", err)
	}
	run.BotID, run.FilePath, run.WorkflowHash, run.WorkflowSource = "chatbot", botPath, hash, source
	if err := st.SaveRun(ctx, run); err != nil {
		t.Fatalf("save run: %v", err)
	}
	interactionID := runID + "_chat"
	questions := map[string]any{}
	if err := st.WriteInteraction(ctx, &store.Interaction{ID: interactionID, RunID: runID, NodeID: "chat", RequestedAt: time.Now().UTC(), Questions: questions}); err != nil {
		t.Fatalf("write interaction: %v", err)
	}
	if err := st.PauseRun(ctx, runID, &store.Checkpoint{
		NodeID: "chat", InteractionID: interactionID, InteractionQuestions: questions,
		Outputs: map[string]map[string]any{}, LoopCounters: map[string]int{}, ArtifactVersions: map[string]int{}, Vars: map[string]any{},
	}); err != nil {
		t.Fatalf("pause run: %v", err)
	}

	resp := steerPost(t, "http://"+addr+"/api/runs/"+runID+"/host-event", map[string]any{
		"kind": "action-completed",
		"event": map[string]any{
			"action":  "run.watch",
			"args":    map[string]any{"target_run_id": "unrelated-target", "mode": "diagnose"},
			"message": "Watching the requested run",
		},
	})
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("host event status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_ = resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err = st.LoadRun(ctx, runID)
		if err != nil {
			t.Fatalf("load run after host event: %v", err)
		}
		if run.Status == store.RunStatusFinished {
			break
		}
		if run.Status.IsTerminal() || run.Status == store.RunStatusFailedResumable {
			t.Fatalf("explicit host event stopped as %s: %s", run.Status, run.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("explicit host event did not finish: %s", run.Status)
	}
	if srv.assistantWatch != nil {
		t.Fatal("explicit host event unexpectedly started a watch coordinator")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ListenAndServe = %v, want http.ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not stop")
	}
}
