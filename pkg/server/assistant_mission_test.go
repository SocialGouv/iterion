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

	"github.com/SocialGouv/iterion/pkg/assistantmission"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestAssistantMissionCreateReattachesTerminalInvocationWithoutRenewal(t *testing.T) {
	srv, hs := newTestServer(t)
	seedRun(t, srv, "mission-target", "target", store.RunStatusFailedResumable)
	seedRun(t, srv, "mission-assistant", "chatbot", store.RunStatusPausedWaitingHuman)

	botsRoot := filepath.Join(srv.cfg.WorkDir, "bots")
	bundleDir := filepath.Join(botsRoot, "chatbot")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	botPath := filepath.Join(bundleDir, "main.bot")
	if err := os.WriteFile(botPath, []byte("workflow chatbot:\n  entry: chat\n  human chat:\n    prompt: wait\n    output_schema:\n      type: object\n  chat -> chat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.yaml"), []byte("name: chatbot\nchat:\n  nodes:\n    chat:\n      kind: human\n      text_field: message\n      host_event_field: host_event\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Bots.Paths = []string{botsRoot}
	run, err := srv.runs.RunStore().LoadRun(context.Background(), "mission-assistant")
	if err != nil {
		t.Fatal(err)
	}
	run.FilePath, run.BotID = botPath, "chatbot"
	if err := srv.runs.RunStore().SaveRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	watch := runwatch.Watch{ID: "mission-watch", OwnerID: "local", TargetRunID: "mission-target", AssistantRunID: "mission-assistant", Mode: runwatch.ModePropose, State: runwatch.WatchActive, CreatedAt: now, UpdatedAt: now}
	if err := srv.assistantWatches.CreateWatch(context.Background(), watch); err != nil {
		t.Fatal(err)
	}
	post := func(body string) (int, []byte) {
		t.Helper()
		resp, err := http.Post(hs.URL+"/api/runs/mission-target/assistant-missions", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, payload
	}
	status, body := post(`{"invocation_key":"goal:stable","watch_id":"mission-watch","ttl_seconds":600,"max_actions":2}`)
	if status != http.StatusOK {
		t.Fatalf("create = %d: %s", status, body)
	}
	var created assistantmission.Mission
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Policy.ExpiresAt.IsZero() {
		t.Fatalf("incomplete mission: %#v", created)
	}
	for _, path := range []string{
		"/api/runs/mission-target/assistant-missions",
		"/api/runs/mission-target/assistant-missions/" + created.ID,
	} {
		resp, err := http.Get(hs.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Contains(payload, []byte(created.ID)) {
			t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, payload)
		}
	}
	stopResp, err := http.Post(hs.URL+"/api/runs/mission-target/assistant-missions/"+created.ID+"/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	stopBody, _ := io.ReadAll(stopResp.Body)
	stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop = %d: %s", stopResp.StatusCode, stopBody)
	}
	stillActive, err := srv.assistantWatches.GetWatch(context.Background(), watch.ID)
	if err != nil || stillActive.State != runwatch.WatchActive {
		t.Fatalf("stopping mission changed watch: %#v err=%v", stillActive, err)
	}
	if err := srv.assistantWatches.StopWatch(context.Background(), watch.ID, "", runwatch.WatchStopped, "test", now); err != nil {
		t.Fatal(err)
	}
	status, body = post(`{"invocation_key":"goal:stable","ttl_seconds":600,"max_actions":2}`)
	if status != http.StatusOK {
		t.Fatalf("terminal reattach = %d: %s", status, body)
	}
	var reattached assistantmission.Mission
	if err := json.Unmarshal(body, &reattached); err != nil {
		t.Fatal(err)
	}
	if reattached.ID != created.ID || !reattached.Policy.ExpiresAt.Equal(created.Policy.ExpiresAt) || reattached.State != assistantmission.StateStopped {
		t.Fatalf("reattach changed durable mission: created=%#v reattached=%#v", created, reattached)
	}
}

func TestValidateMissionProposalRejectsWatchAndSourceMutation(t *testing.T) {
	for _, proposal := range []assistantmission.Proposal{
		{ID: "run.watch", Args: map[string]any{"run_id": "target"}},
		{ID: assistantmission.ActionResume, Args: map[string]any{"run_id": "target", "force": true}},
		{ID: assistantmission.ActionResume, Args: map[string]any{"run_id": "other"}},
		{ID: assistantmission.ActionRewind, Args: map[string]any{"run_id": "target", "auto": true, "restore_scope": "full"}},
	} {
		if err := validateMissionProposal(proposal, "target"); err == nil {
			t.Fatalf("unsafe proposal accepted: %#v", proposal)
		}
	}
}

func TestAssistantMissionCoordinatorExecutesOnlyPersistedRewindProposal(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	botPath := filepath.Join(srv.cfg.WorkDir, "target.bot")
	const source = `schema out:
  value: string
agent first:
  model: "test"
  output: out
agent repair:
  model: "test"
  output: out
workflow target:
  entry: first
  first -> repair
  repair -> done
`
	if err := os.WriteFile(botPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.runs.RunStore().CreateRun(ctx, "mission-exec-target", "target", nil); err != nil {
		t.Fatal(err)
	}
	target, _ := srv.runs.RunStore().LoadRun(ctx, "mission-exec-target")
	target.FilePath, target.WorkflowSource, target.Status = botPath, source, store.RunStatusFailedResumable
	target.Checkpoint = &store.Checkpoint{NodeID: "repair", Outputs: map[string]map[string]any{"first": {"value": "ok"}, "repair": {"value": "bad"}}}
	if err := srv.runs.RunStore().SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}
	seedRun(t, srv, "mission-exec-assistant", "assistant", store.RunStatusPausedWaitingHuman)
	if err := srv.runs.RunStore().WriteArtifact(ctx, &store.Artifact{
		RunID: "mission-exec-assistant", NodeID: "proposal", Version: 0,
		Data: map[string]any{"assistant_actions": []any{map[string]any{
			"id":   assistantmission.ActionRewind,
			"args": map[string]any{"run_id": "mission-exec-target", "node_id": "repair"},
		}}}, WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	watch := runwatch.Watch{ID: "mission-exec-watch", OwnerID: "local", TargetRunID: target.ID, AssistantRunID: "mission-exec-assistant", Mode: runwatch.ModePropose, State: runwatch.WatchActive, CreatedAt: now, UpdatedAt: now}
	if err := srv.assistantWatches.CreateWatch(ctx, watch); err != nil {
		t.Fatal(err)
	}
	mission := assistantmission.Mission{Version: 1, ID: "mission-exec", InvocationKey: "goal:" + target.ID, OperatorID: "local", TargetRunID: target.ID, WatchID: watch.ID, AssistantRunID: watch.AssistantRunID, Policy: assistantmission.Policy{Actions: []string{assistantmission.ActionRewind}, TTLSeconds: 600, ExpiresAt: now.Add(10 * time.Minute), MaxActions: 1, ContractVersion: assistantmission.ContractVersion}, State: assistantmission.StateActive, Activation: &assistantmission.DeliveryReceipt{ID: "activated", Kind: "assistant-mission-started", State: assistantmission.ReceiptSucceeded}, ProposalFrontier: map[string]int{}, CreatedAt: now, UpdatedAt: now}
	if _, _, err := srv.assistantMissions.CreateOrGet(ctx, mission); err != nil {
		t.Fatal(err)
	}
	coord := &assistantMissionCoordinator{server: srv, runs: srv.runs, watches: srv.assistantWatches, missions: srv.assistantMissions, worker: "test-worker"}
	coord.attempt(ctx, mission.ID) // persist proposal + prepared receipt
	coord.attempt(ctx, mission.ID) // issue + execute rewind
	coord.attempt(ctx, mission.ID) // reconcile run_rewound receipt
	got, err := srv.assistantMissions.Get(ctx, assistantmission.Scope{OperatorID: "local", TargetRunID: target.ID}, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DispatchedActions != 1 || len(got.Receipts) != 1 || got.Receipts[0].State != assistantmission.ReceiptSucceeded {
		t.Fatalf("mission action ledger = %#v", got)
	}
	rewound, _ := srv.runs.RunStore().LoadRun(ctx, target.ID)
	if rewound.Status != store.RunStatusPausedOperator || rewound.Checkpoint.NodeID != "repair" {
		t.Fatalf("target after mission rewind = %#v", rewound)
	}
}
