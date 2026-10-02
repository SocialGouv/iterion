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

// The #1381 race: the stored bot REPUBLISHED between the two coordinator
// passes. The preview certifies v1 and pins its botsource version on the
// receipt; the apply must resolve THAT version — a blast radius computed in
// v2, where the node after `alpha` has been renamed, would keep `beta`'s
// stale output on a receipt that reports success, and ExpectedPivot cannot
// catch it (both graphs name the pivot `alpha`).
func TestAssistantMissionRewind_AppliesTheVersionThePreviewPinned(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.botSources = botsource.NewMemoryStore()
	srv.cfg.Mode = "cloud"
	ctx := context.Background()

	const executed = `schema out:
  value: string
agent setup:
  model: "test"
  output: out
agent alpha:
  model: "test"
  output: out
agent beta:
  model: "test"
  output: out
workflow target:
  entry: setup
  setup -> alpha
  alpha -> beta
  beta -> done
`
	// v2 renames the node that follows `alpha`, so the two versions disagree
	// on exactly the output whose destruction the rewind exists to perform.
	republished := strings.NewReplacer(
		"agent beta:", "agent gamma:",
		"alpha -> beta", "alpha -> gamma",
		"beta -> done", "gamma -> done",
	).Replace(executed)
	if republished == executed {
		t.Fatal("the fixture no longer renames the node the pin must see past")
	}
	created, err := srv.botSources.Create(store.WithTenant(ctx, "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "shared",
		Files: map[string]string{botsource.MainBotFile: executed},
	})
	if err != nil {
		t.Fatal(err)
	}

	const runID = "mission-pinned-target"
	if _, err := srv.runs.RunStore().CreateRun(ctx, runID, "target", nil); err != nil {
		t.Fatal(err)
	}
	target, _ := srv.runs.RunStore().LoadRun(ctx, runID)
	target.FilePath = "bots/shared/main.bot"
	target.BotSourceTier, target.BotSourceTenant = store.BotSourceTierTeam, "t1"
	target.WorkflowSource, target.Status = executed, store.RunStatusFailedResumable
	target.Checkpoint = &store.Checkpoint{NodeID: "beta", Outputs: map[string]map[string]any{
		"setup": {"value": "ok"}, "alpha": {"value": "ok"}, "beta": {"value": "stale"},
	}}
	if err := srv.runs.RunStore().SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}

	seedRun(t, srv, "mission-pinned-assistant", "assistant", store.RunStatusPausedWaitingHuman)
	if err := srv.runs.RunStore().WriteArtifact(ctx, &store.Artifact{
		RunID: "mission-pinned-assistant", NodeID: "proposal", Version: 0,
		Data: map[string]any{"assistant_actions": []any{map[string]any{
			"id":   assistantmission.ActionRewind,
			"args": map[string]any{"run_id": runID, "node_id": "alpha"},
		}}}, WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	watch := runwatch.Watch{ID: "mission-pinned-watch", OwnerID: "local", TargetRunID: runID,
		AssistantRunID: "mission-pinned-assistant", Mode: runwatch.ModePropose, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now}
	if err := srv.assistantWatches.CreateWatch(ctx, watch); err != nil {
		t.Fatal(err)
	}
	mission := assistantmission.Mission{
		Version: 1, ID: "mission-pinned", InvocationKey: "goal:" + runID, OperatorID: "local",
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
	coord.attempt(ctx, mission.ID) // preview: compute the pivot, pin the version

	got, err := srv.assistantMissions.Get(ctx, assistantmission.Scope{OperatorID: "local", TargetRunID: runID}, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Receipts) != 1 || got.Receipts[0].State == assistantmission.ReceiptRejected {
		t.Fatalf("the preview could not certify the rewind: %#v", got.Receipts)
	}
	if got.Receipts[0].SourceVersion != 1 {
		t.Fatalf("preview pinned source version %d, want 1 — the apply has nothing to resolve against", got.Receipts[0].SourceVersion)
	}
	if got.Receipts[0].SourceID != created.ID {
		t.Fatalf("preview pinned source row %q, want the row it resolved (%q) — the apply cannot tell incarnations apart", got.Receipts[0].SourceID, created.ID)
	}
	if got.Receipts[0].ExpectedPivot != "alpha" {
		t.Fatalf("ExpectedPivot = %q, want alpha", got.Receipts[0].ExpectedPivot)
	}

	// The republication lands BETWEEN the passes.
	cur, err := srv.botSources.GetBySlug(store.WithTenant(ctx, "t1"), "t1", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.botSources.Update(store.WithTenant(ctx, "t1"), botsource.BotSource{
		ID: cur.ID, TenantID: cur.TenantID, Slug: cur.Slug, Version: cur.Version,
		Files: map[string]string{botsource.MainBotFile: republished},
	}); err != nil {
		t.Fatal(err)
	}

	coord.attempt(ctx, mission.ID) // apply: must resolve the PINNED v1, not the current v2

	got, err = srv.assistantMissions.Get(ctx, assistantmission.Scope{OperatorID: "local", TargetRunID: runID}, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Receipts[0].State == assistantmission.ReceiptRejected {
		t.Fatalf("the apply refused a pinned version the store still serves: %s", got.Receipts[0].Error)
	}
	rewound, err := srv.runs.RunStore().LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if rewound.Checkpoint == nil || rewound.Checkpoint.NodeID != "alpha" {
		t.Fatalf("anchor after the mission rewind = %#v", rewound.Checkpoint)
	}
	// The property: `beta` runs after `alpha` in the program the preview
	// certified, so rewinding to `alpha` must drop its output. It survives
	// only if the blast radius was computed in v2, where `beta` no longer
	// exists — the republished graph the preview never saw.
	if _, still := rewound.Checkpoint.Outputs["beta"]; still {
		t.Errorf("`beta` kept its output across a rewind to `alpha` — the blast radius was computed in the "+
			"republished version, where `beta` no longer exists. Outputs: %v", rewound.Checkpoint.Outputs)
	}
}

// The OTHER half of the #1381 race: the slug deleted and RE-AUTHORED between
// the two coordinator passes. The pin is identity-keyed (row id + version),
// so the apply must act on the CERTIFIED incarnation — never the recreated
// row, whose graph may differ arbitrarily.
func TestAssistantMissionRewind_AppliesTheIncarnationThePreviewCertified(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.botSources = botsource.NewMemoryStore()
	srv.cfg.Mode = "cloud"
	ctx := context.Background()

	const executed = `schema out:
  value: string
agent setup:
  model: "test"
  output: out
agent alpha:
  model: "test"
  output: out
agent beta:
  model: "test"
  output: out
workflow target:
  entry: setup
  setup -> alpha
  alpha -> beta
  beta -> done
`
	created, err := srv.botSources.Create(store.WithTenant(ctx, "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "shared",
		Files: map[string]string{botsource.MainBotFile: executed},
	})
	if err != nil {
		t.Fatal(err)
	}

	const runID = "mission-reborn-target"
	if _, err := srv.runs.RunStore().CreateRun(ctx, runID, "target", nil); err != nil {
		t.Fatal(err)
	}
	target, _ := srv.runs.RunStore().LoadRun(ctx, runID)
	target.FilePath = "bots/shared/main.bot"
	target.BotSourceTier, target.BotSourceTenant = store.BotSourceTierTeam, "t1"
	target.WorkflowSource, target.Status = executed, store.RunStatusFailedResumable
	target.Checkpoint = &store.Checkpoint{NodeID: "beta", Outputs: map[string]map[string]any{
		"setup": {"value": "ok"}, "alpha": {"value": "ok"}, "beta": {"value": "stale"},
	}}
	if err := srv.runs.RunStore().SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}

	seedRun(t, srv, "mission-reborn-assistant", "assistant", store.RunStatusPausedWaitingHuman)
	if err := srv.runs.RunStore().WriteArtifact(ctx, &store.Artifact{
		RunID: "mission-reborn-assistant", NodeID: "proposal", Version: 0,
		Data: map[string]any{"assistant_actions": []any{map[string]any{
			"id":   assistantmission.ActionRewind,
			"args": map[string]any{"run_id": runID, "node_id": "alpha"},
		}}}, WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	watch := runwatch.Watch{ID: "mission-reborn-watch", OwnerID: "local", TargetRunID: runID,
		AssistantRunID: "mission-reborn-assistant", Mode: runwatch.ModePropose, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now}
	if err := srv.assistantWatches.CreateWatch(ctx, watch); err != nil {
		t.Fatal(err)
	}
	mission := assistantmission.Mission{
		Version: 1, ID: "mission-reborn", InvocationKey: "goal:" + runID, OperatorID: "local",
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
	coord.attempt(ctx, mission.ID) // preview: certify the FIRST incarnation

	got, err := srv.assistantMissions.Get(ctx, assistantmission.Scope{OperatorID: "local", TargetRunID: runID}, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Receipts) != 1 || got.Receipts[0].State == assistantmission.ReceiptRejected {
		t.Fatalf("the preview could not certify the rewind: %#v", got.Receipts)
	}
	if got.Receipts[0].SourceID != created.ID {
		t.Fatalf("preview pinned row %q, want the first incarnation %q", got.Receipts[0].SourceID, created.ID)
	}

	// The delete-and-recreate lands BETWEEN the passes: same slug, NEW row
	// id, and a graph where the node after `alpha` has been renamed.
	if err := srv.botSources.Delete(store.WithTenant(ctx, "t1"), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.botSources.Create(store.WithTenant(ctx, "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "shared",
		Files: map[string]string{botsource.MainBotFile: strings.NewReplacer(
			"agent beta:", "agent gamma:",
			"alpha -> beta", "alpha -> gamma",
			"beta -> done", "gamma -> done",
		).Replace(executed)},
	}); err != nil {
		t.Fatal(err)
	}

	coord.attempt(ctx, mission.ID) // apply: must serve the CERTIFIED incarnation

	got, err = srv.assistantMissions.Get(ctx, assistantmission.Scope{OperatorID: "local", TargetRunID: runID}, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Receipts[0].State == assistantmission.ReceiptRejected {
		t.Fatalf("the apply refused a pinned incarnation the store still serves: %s", got.Receipts[0].Error)
	}
	rewound, err := srv.runs.RunStore().LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if rewound.Checkpoint == nil || rewound.Checkpoint.NodeID != "alpha" {
		t.Fatalf("anchor after the mission rewind = %#v", rewound.Checkpoint)
	}
	// The certified incarnation's graph, not the recreated row's: `beta`
	// (dropped downstream in the certified graph) must lose its output, even
	// though the recreated row knows no `beta` at all.
	if _, still := rewound.Checkpoint.Outputs["beta"]; still {
		t.Errorf("`beta` kept its output — the apply acted in the RECREATED row's graph instead of the "+
			"certified incarnation's. Outputs: %v", rewound.Checkpoint.Outputs)
	}
}

// A pin whose version is GONE from the history (a store reset) must refuse
// the apply — never fall through to the current row, which would recompute
// the blast radius in a program the preview never certified.
func TestAssistantMissionRewind_RejectsWhenThePinnedVersionIsGone(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.botSources = botsource.NewMemoryStore()
	srv.cfg.Mode = "cloud"
	ctx := context.Background()

	const goneFixture = `schema out:
  value: string
agent setup:
  model: "test"
  output: out
agent alpha:
  model: "test"
  output: out
agent beta:
  model: "test"
  output: out
workflow target:
  entry: setup
  setup -> alpha
  alpha -> beta
  beta -> done
`
	live, err := srv.botSources.Create(store.WithTenant(ctx, "t1"), botsource.BotSource{
		TenantID: "t1", Slug: "shared",
		Files: map[string]string{botsource.MainBotFile: goneFixture},
	})
	if err != nil {
		t.Fatal(err)
	}
	const runID = "mission-pin-gone-target"
	if _, err := srv.runs.RunStore().CreateRun(ctx, runID, "target", nil); err != nil {
		t.Fatal(err)
	}
	target, _ := srv.runs.RunStore().LoadRun(ctx, runID)
	target.FilePath = "bots/shared/main.bot"
	target.BotSourceTier, target.BotSourceTenant = store.BotSourceTierTeam, "t1"
	target.WorkflowSource, target.Status = goneFixture, store.RunStatusFailedResumable
	target.Checkpoint = &store.Checkpoint{NodeID: "beta", Outputs: map[string]map[string]any{
		"setup": {"value": "ok"}, "alpha": {"value": "ok"}, "beta": {"value": "stale"},
	}}
	if err := srv.runs.RunStore().SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}

	seedRun(t, srv, "mission-pin-gone-assistant", "assistant", store.RunStatusPausedWaitingHuman)
	now := time.Now().UTC()
	watch := runwatch.Watch{ID: "mission-pin-gone-watch", OwnerID: "local", TargetRunID: runID,
		AssistantRunID: "mission-pin-gone-assistant", Mode: runwatch.ModePropose, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now}
	if err := srv.assistantWatches.CreateWatch(ctx, watch); err != nil {
		t.Fatal(err)
	}
	mission := assistantmission.Mission{
		Version: 1, ID: "mission-pin-gone", InvocationKey: "goal:" + runID, OperatorID: "local",
		TargetRunID: runID, WatchID: watch.ID, AssistantRunID: watch.AssistantRunID,
		Policy: assistantmission.Policy{Actions: []string{assistantmission.ActionRewind}, TTLSeconds: 600,
			ExpiresAt: now.Add(10 * time.Minute), MaxActions: 1, ContractVersion: assistantmission.ContractVersion},
		State:      assistantmission.StateActive,
		Activation: &assistantmission.DeliveryReceipt{ID: "activated", Kind: "assistant-mission-started", State: assistantmission.ReceiptSucceeded},
		// A receipt from a preview whose certified version the store no
		// longer carries: version 42 never existed.
		Receipts: []assistantmission.ActionReceipt{{
			ID: "mission-pin-gone-receipt", Action: assistantmission.ActionRewind, Digest: "gone",
			AssistantRunID: watch.AssistantRunID, ExpectedPivot: "alpha", SourceVersion: 42,
			// The LIVE row's id: the refusal must probe it and name the
			// raced-snapshot cause, not send the operator after a deletion.
			SourceID: live.ID,
			State:    assistantmission.ReceiptPrepared, CreatedAt: now, UpdatedAt: now,
		}},
		ProposalFrontier: map[string]int{}, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := srv.assistantMissions.CreateOrGet(ctx, mission); err != nil {
		t.Fatal(err)
	}
	coord := &assistantMissionCoordinator{server: srv, runs: srv.runs, watches: srv.assistantWatches,
		missions: srv.assistantMissions, worker: "test-worker"}
	coord.attempt(ctx, mission.ID)

	got, err := srv.assistantMissions.Get(ctx, assistantmission.Scope{OperatorID: "local", TargetRunID: runID}, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Receipts[0]
	if r.State != assistantmission.ReceiptRejected {
		t.Fatalf("receipt state = %q — the apply ran against a version the store cannot produce", r.State)
	}
	if !strings.Contains(r.Error, "42") {
		t.Errorf("refusal = %q — it must name the pinned version it could not resolve", r.Error)
	}
	// The probe distinguishes the cause: the row still exists (at version 1),
	// so the refusal must say the SNAPSHOT is what is missing — not send the
	// operator chasing a deletion that never happened.
	if !strings.Contains(r.Error, "its snapshot is missing") {
		t.Errorf("refusal = %q — a live row must be named as the missing-snapshot cause", r.Error)
	}
	rewound, err := srv.runs.RunStore().LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if rewound.Checkpoint == nil || rewound.Checkpoint.NodeID != "beta" || len(rewound.Checkpoint.Outputs) != 3 {
		t.Fatalf("the refused apply must leave the checkpoint untouched, got %#v", rewound.Checkpoint)
	}
}

// And when that resolution FAILS, the mission must reject — never proceed with
// an empty path.
//
// Nothing downstream would catch it: ErrRewindStoredBotSourceUnresolved is
// raised by the --auto pivot resolver alone, and the apply always names an
// explicit node. An empty CurrentSourcePath there falls back to the baked
// twin, computes the drop set in another program, and reports a SUCCEEDED
// receipt. ExpectedPivot guards the pivot's name, not the graph.
//
// The row is deleted after the run was launched from it — one of three
// reachable causes, alongside a store blip and a failed materialization, all
// of which the HTTP endpoint on this server answers 503/400.
func TestAssistantMissionRewind_RejectsWhenTheStoredBotCannotBeResolved(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.botSources = botsource.NewMemoryStore()
	srv.cfg.Mode = "cloud"
	ctx := context.Background()

	const runID = "mission-unresolvable-target"
	if _, err := srv.runs.RunStore().CreateRun(ctx, runID, "target", nil); err != nil {
		t.Fatal(err)
	}
	target, _ := srv.runs.RunStore().LoadRun(ctx, runID)
	// A stored-bot run whose row does not exist: deleted after the launch.
	target.FilePath = "bots/vanished/main.bot"
	target.BotSourceTier, target.BotSourceTenant = store.BotSourceTierTeam, "t1"
	target.Status = store.RunStatusFailedResumable
	target.Checkpoint = &store.Checkpoint{NodeID: "repair", Outputs: map[string]map[string]any{"repair": {"v": 1}}}
	if err := srv.runs.RunStore().SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}

	coord := &assistantMissionCoordinator{server: srv, runs: srv.runs, watches: srv.assistantWatches,
		missions: srv.assistantMissions, worker: "test-worker"}
	path, _, _, release, err := coord.rewindCurrentSourceAt(ctx, runID, 0, "")
	defer release()
	if err == nil {
		t.Fatalf("resolution returned %q and no error — the mission would rewind against the baked catalog twin "+
			"and drop the wrong nodes, on a receipt that reports success", path)
	}
	if path != "" {
		t.Errorf("path = %q beside an error, want empty", path)
	}
}

// The OTHER refusal cause: the pinned row itself is gone from the store (a
// history reset), so the probe cannot name a raced write — the refusal must
// say the history does not carry the version, and the checkpoint must stay
// untouched.
func TestAssistantMissionRewind_NamesAResetWhenTheRowIsGone(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.botSources = botsource.NewMemoryStore()
	srv.cfg.Mode = "cloud"
	ctx := context.Background()

	const runID = "mission-row-gone-target"
	if _, err := srv.runs.RunStore().CreateRun(ctx, runID, "target", nil); err != nil {
		t.Fatal(err)
	}
	target, _ := srv.runs.RunStore().LoadRun(ctx, runID)
	target.FilePath = "bots/shared/main.bot"
	target.BotSourceTier, target.BotSourceTenant = store.BotSourceTierTeam, "t1"
	target.Status = store.RunStatusFailedResumable
	target.Checkpoint = &store.Checkpoint{NodeID: "beta", Outputs: map[string]map[string]any{
		"setup": {"value": "ok"}, "alpha": {"value": "ok"}, "beta": {"value": "stale"},
	}}
	if err := srv.runs.RunStore().SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}

	seedRun(t, srv, "mission-row-gone-assistant", "assistant", store.RunStatusPausedWaitingHuman)
	now := time.Now().UTC()
	watch := runwatch.Watch{ID: "mission-row-gone-watch", OwnerID: "local", TargetRunID: runID,
		AssistantRunID: "mission-row-gone-assistant", Mode: runwatch.ModePropose, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now}
	if err := srv.assistantWatches.CreateWatch(ctx, watch); err != nil {
		t.Fatal(err)
	}
	mission := assistantmission.Mission{
		Version: 1, ID: "mission-row-gone", InvocationKey: "goal:" + runID, OperatorID: "local",
		TargetRunID: runID, WatchID: watch.ID, AssistantRunID: watch.AssistantRunID,
		Policy: assistantmission.Policy{Actions: []string{assistantmission.ActionRewind}, TTLSeconds: 600,
			ExpiresAt: now.Add(10 * time.Minute), MaxActions: 1, ContractVersion: assistantmission.ContractVersion},
		State:      assistantmission.StateActive,
		Activation: &assistantmission.DeliveryReceipt{ID: "activated", Kind: "assistant-mission-started", State: assistantmission.ReceiptSucceeded},
		Receipts: []assistantmission.ActionReceipt{{
			ID: "mission-row-gone-receipt", Action: assistantmission.ActionRewind, Digest: "gone-row",
			AssistantRunID: watch.AssistantRunID, ExpectedPivot: "alpha", SourceVersion: 1,
			SourceID: "row-that-never-was",
			State:    assistantmission.ReceiptPrepared, CreatedAt: now, UpdatedAt: now,
		}},
		ProposalFrontier: map[string]int{}, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := srv.assistantMissions.CreateOrGet(ctx, mission); err != nil {
		t.Fatal(err)
	}
	coord := &assistantMissionCoordinator{server: srv, runs: srv.runs, watches: srv.assistantWatches,
		missions: srv.assistantMissions, worker: "test-worker"}
	coord.attempt(ctx, mission.ID)

	got, err := srv.assistantMissions.Get(ctx, assistantmission.Scope{OperatorID: "local", TargetRunID: runID}, mission.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Receipts[0]
	if r.State != assistantmission.ReceiptRejected {
		t.Fatalf("receipt state = %q — the apply ran against a row the store cannot name", r.State)
	}
	if !strings.Contains(r.Error, "history does not carry it") {
		t.Errorf("refusal = %q — a vanished row must be named as the history-reset cause", r.Error)
	}
	rewound, err := srv.runs.RunStore().LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if rewound.Checkpoint == nil || rewound.Checkpoint.NodeID != "beta" || len(rewound.Checkpoint.Outputs) != 3 {
		t.Fatalf("the refused apply must leave the checkpoint untouched, got %#v", rewound.Checkpoint)
	}
}
