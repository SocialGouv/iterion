package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/assistantmission"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A mission rewinds through TWO calls — the preview that names the pivot and
// the apply that performs it — and both derive the blast radius from the graph
// the current source compiles to. Wiring one and not the other names a pivot in
// the real program and applies it in the baked twin.
//
// The twin here orders the two middle nodes the other way, so a drop set taken
// from it keeps `second`, whose stale output the rewind exists to remove. The
// run is served by a team bot, which is the shape that has no source on this
// pod at all.
func TestAssistantMissionRewind_UsesTheStoredBotsCurrentGraphOnBothCalls(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.botSources = botsource.NewMemoryStore()
	srv.cfg.Mode = "cloud"
	ctx := context.Background()

	const executed = `schema out:
  value: string
agent first:
  model: "test"
  output: out
agent second:
  model: "test"
  output: out
agent repair:
  model: "test"
  output: out
workflow target:
  entry: first
  first -> repair
  repair -> second
  second -> done
`
	// The baked catalog twin: same declarations, `second` BEFORE `repair`.
	twin := strings.NewReplacer(
		"first -> repair", "first -> second",
		"repair -> second", "second -> repair",
		"second -> done", "repair -> done",
	).Replace(executed)
	if twin == executed {
		t.Fatal("the fixture no longer carries the edges this test swaps")
	}
	botDir := filepath.Join(srv.cfg.WorkDir, "bots", "shared")
	if err := os.MkdirAll(botDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botDir, botsource.MainBotFile), []byte(twin), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.botSources.Create(store.WithTenant(ctx, "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "shared",
		Files: map[string]string{botsource.MainBotFile: executed},
	}); err != nil {
		t.Fatal(err)
	}

	const runID = "mission-stored-target"
	if _, err := srv.runs.RunStore().CreateRun(ctx, runID, "target", nil); err != nil {
		t.Fatal(err)
	}
	target, _ := srv.runs.RunStore().LoadRun(ctx, runID)
	target.FilePath = "bots/shared/main.bot"
	target.BotSourceTier, target.BotSourceTenant = store.BotSourceTierTeam, "t1"
	target.WorkflowSource, target.Status = executed, store.RunStatusFailedResumable
	target.Checkpoint = &store.Checkpoint{NodeID: "second", Outputs: map[string]map[string]any{
		"first": {"value": "ok"}, "repair": {"value": "bad"}, "second": {"value": "stale"},
	}}
	if err := srv.runs.RunStore().SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}

	seedRun(t, srv, "mission-stored-assistant", "assistant", store.RunStatusPausedWaitingHuman)
	if err := srv.runs.RunStore().WriteArtifact(ctx, &store.Artifact{
		RunID: "mission-stored-assistant", NodeID: "proposal", Version: 0,
		Data: map[string]any{"assistant_actions": []any{map[string]any{
			"id":   assistantmission.ActionRewind,
			"args": map[string]any{"run_id": runID, "node_id": "repair"},
		}}}, WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	watch := runwatch.Watch{ID: "mission-stored-watch", OwnerID: "local", TargetRunID: runID,
		AssistantRunID: "mission-stored-assistant", Mode: runwatch.ModePropose, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now}
	if err := srv.assistantWatches.CreateWatch(ctx, watch); err != nil {
		t.Fatal(err)
	}
	mission := assistantmission.Mission{
		Version: 1, ID: "mission-stored", InvocationKey: "goal:" + runID, OperatorID: "local",
		TargetRunID: runID, WatchID: watch.ID, AssistantRunID: watch.AssistantRunID,
		Policy: assistantmission.Policy{Actions: []string{assistantmission.ActionRewind}, TTLSeconds: 600,
			ExpiresAt: now.Add(10 * time.Minute), MaxActions: 1, ContractVersion: assistantmission.ContractVersion},
		State:            assistantmission.StateActive,
		Activation:       &assistantmission.DeliveryReceipt{ID: "activated", Kind: "assistant-mission-started", State: assistantmission.ReceiptSucceeded},
		ProposalFrontier: map[string]int{}, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := srv.assistantMissions.CreateOrGet(ctx, mission); err != nil {
		t.Fatal(err)
	}
	coord := &assistantMissionCoordinator{server: srv, runs: srv.runs, watches: srv.assistantWatches,
		missions: srv.assistantMissions, worker: "test-worker"}
	coord.attempt(ctx, mission.ID) // persist the proposal + the prepared receipt
	coord.attempt(ctx, mission.ID) // issue + execute the rewind

	got, err := srv.assistantMissions.Get(ctx, assistantmission.Scope{OperatorID: "local", TargetRunID: runID}, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Receipts) != 1 || got.Receipts[0].State == assistantmission.ReceiptRejected {
		t.Fatalf("the mission could not rewind a stored-bot run: %#v", got.Receipts)
	}
	rewound, err := srv.runs.RunStore().LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if rewound.Checkpoint == nil || rewound.Checkpoint.NodeID != "repair" {
		t.Fatalf("anchor after the mission rewind = %#v", rewound.Checkpoint)
	}
	// The property: `second` runs AFTER `repair` in the program this run
	// executed, so rewinding to `repair` must drop its output. It survives
	// only if the blast radius came from the twin, where the two are swapped.
	if _, still := rewound.Checkpoint.Outputs["second"]; still {
		t.Errorf("`second` kept its output across a rewind to `repair` — its blast radius was computed in the "+
			"baked twin, where `second` runs first. Outputs: %v", rewound.Checkpoint.Outputs)
	}
}
