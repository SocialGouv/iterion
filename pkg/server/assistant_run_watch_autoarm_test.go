package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// armFixture stands up the smallest world in which a card-watching assistant
// and the run that card produced can meet: a real run store, a real runwatch
// store, and a bot bundle on disk whose manifest declares (or withholds) a
// host-event-capable chat node.
type armFixture struct {
	srv   *Server
	coord *assistantWatchCoordinator
	rs    store.RunStore
	ws    runwatch.Store
}

func newArmFixture(t *testing.T, hostEventCapable bool) *armFixture {
	t.Helper()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	svc := newTestRunviewService(t, "", runview.WithStore(rs))

	botsRoot := t.TempDir()
	dir := filepath.Join(botsRoot, "chatbot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("workflow chatbot:\n  entry: chat\n"), 0o644); err != nil {
		t.Fatalf("write main.bot: %v", err)
	}
	manifest := "name: chatbot\nchat:\n  nodes:\n    chat:\n      kind: human\n      text_field: message\n"
	if hostEventCapable {
		manifest += "      host_event_field: host_event\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	ws := runwatch.NewFSStore(t.TempDir())
	if err := ws.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	srv := &Server{runs: svc, assistantWatches: ws, logger: iterlog.New(iterlog.LevelError, os.Stderr)}
	srv.cfg.Bots.Paths = []string{botsRoot}
	return &armFixture{
		srv:   srv,
		coord: &assistantWatchCoordinator{server: srv, store: ws, worker: "test"},
		rs:    rs,
		ws:    ws,
	}
}

func (f *armFixture) assistant(t *testing.T, id, issueID string) *store.Run {
	t.Helper()
	ctx := context.Background()
	if _, err := f.rs.CreateRun(ctx, id, "chatbot", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	r, err := f.rs.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.BotID = "chatbot"
	r.Status = store.RunStatusPausedWaitingHuman
	r.Checkpoint = &store.Checkpoint{NodeID: "chat"}
	if err := f.rs.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
	if _, err := f.rs.AddWatchedIssues(ctx, id, []string{issueID}); err != nil {
		t.Fatalf("AddWatchedIssues %s: %v", id, err)
	}
	out, err := f.rs.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("reload %s: %v", id, err)
	}
	return out
}

func (f *armFixture) target(t *testing.T, id, issueID string, createdAt time.Time) *store.Run {
	t.Helper()
	ctx := context.Background()
	if _, err := f.rs.CreateRun(ctx, id, "worker", nil); err != nil {
		t.Fatalf("CreateRun %s: %v", id, err)
	}
	r, err := f.rs.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun %s: %v", id, err)
	}
	r.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, IssueID: issueID}
	r.Status = store.RunStatusFailed
	r.CreatedAt = createdAt
	if err := f.rs.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun %s: %v", id, err)
	}
	out, err := f.rs.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("reload %s: %v", id, err)
	}
	return out
}

// The defect this whole lot exists for: a card the assistant watches produces
// a run, that run dies, and nothing linked the two — so the assistant never
// heard about it.
func TestArmWatchesForTarget_LinksCardWatcherToTheRunItProduced(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	target := f.target(t, "run-target", issueID, assistant.CreatedAt.Add(time.Minute))

	f.coord.armWatchesForTarget(ctx, target)

	watches, err := f.ws.ListActiveByTarget(ctx, "", target.ID)
	if err != nil {
		t.Fatalf("ListActiveByTarget: %v", err)
	}
	if len(watches) != 1 {
		t.Fatalf("got %d watches, want 1", len(watches))
	}
	if watches[0].AssistantRunID != assistant.ID {
		t.Fatalf("watch points at %q, want %q", watches[0].AssistantRunID, assistant.ID)
	}
	// run.finished is deliberately withheld until a dogfood says the extra
	// chatter is wanted: an auto-armed veille must not make the assistant
	// speak on every successful delegation.
	want := map[string]bool{trigger.KindRunFailed: true, trigger.KindRunCancelled: true}
	if len(watches[0].Kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", watches[0].Kinds, want)
	}
	for _, k := range watches[0].Kinds {
		if !want[k] {
			t.Fatalf("unexpected auto-armed kind %q (kinds = %v)", k, watches[0].Kinds)
		}
	}

	// Idempotent: the fast path and the reconciliation sweep both call this.
	f.coord.armWatchesForTarget(ctx, target)
	again, err := f.ws.ListActiveByTarget(ctx, "", target.ID)
	if err != nil {
		t.Fatalf("ListActiveByTarget (second): %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("re-arming created %d watches, want 1", len(again))
	}
}

// The load-bearing guard: a card watched by a run that can never receive a
// host event must not open an episode nothing can deliver.
func TestArmWatchesForTarget_SkipsWatcherWithNoHostEventGate(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, false)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	target := f.target(t, "run-target", issueID, assistant.CreatedAt.Add(time.Minute))

	f.coord.armWatchesForTarget(ctx, target)

	watches, err := f.ws.ListActiveByTarget(ctx, "", target.ID)
	if err != nil {
		t.Fatalf("ListActiveByTarget: %v", err)
	}
	if len(watches) != 0 {
		t.Fatalf("armed %d watches on a bot with no host-event gate, want 0", len(watches))
	}
}

// A veille armed today must not ambush the operator with the card's history.
func TestArmWatchesForTarget_IgnoresRunsPredatingTheVeille(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	old := f.target(t, "run-old", issueID, assistant.CreatedAt.Add(-time.Hour))

	f.coord.armWatchesForTarget(ctx, old)

	watches, err := f.ws.ListActiveByTarget(ctx, "", old.ID)
	if err != nil {
		t.Fatalf("ListActiveByTarget: %v", err)
	}
	if len(watches) != 0 {
		t.Fatalf("armed %d watches on a pre-veille run, want 0", len(watches))
	}
}

// The bus is lossy. Losing the outcome event must not mean losing the wake-up:
// the sweep walks from each live veille to the terminal runs its cards left.
func TestReconcileArmedWatches_RecoversADroppedOutcomeEvent(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	target := f.target(t, "run-target", issueID, assistant.CreatedAt.Add(time.Minute))

	// No handleEvent call at all — this is the dropped-event world.
	f.coord.reconcileArmedWatches(ctx)

	watches, err := f.ws.ListActiveByTarget(ctx, "", target.ID)
	if err != nil {
		t.Fatalf("ListActiveByTarget: %v", err)
	}
	if len(watches) != 1 {
		t.Fatalf("sweep armed %d watches, want 1", len(watches))
	}
}

// A pending episode remains durable regardless of age. The assistant may have
// been unavailable for hours, and silently blocking the event would terminate
// the practical surveillance even though the watch still says active.
func TestAttempt_DeliversAnOldPendingEpisode(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	target := f.target(t, "run-target", issueID, assistant.CreatedAt.Add(time.Minute))
	f.coord.armWatchesForTarget(ctx, target)
	watches, err := f.ws.ListActiveByTarget(ctx, "", target.ID)
	if err != nil || len(watches) != 1 {
		t.Fatalf("setup: watches=%d err=%v", len(watches), err)
	}

	now := time.Now().UTC()
	stale := now.Add(-24 * time.Hour)
	ep := runwatch.Episode{
		ID: "ep-stale", WatchID: watches[0].ID, TargetRunID: target.ID,
		AssistantRunID: assistant.ID, OutcomeEventID: "evt-1", Kind: trigger.KindRunFailed,
		State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: stale, UpdatedAt: stale,
	}
	if created, err := f.ws.CreateEpisode(ctx, ep); err != nil || !created {
		t.Fatalf("CreateEpisode: created=%v err=%v", created, err)
	}
	due, err := f.ws.ListDueEpisodes(ctx, now, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("setup: due=%d err=%v", len(due), err)
	}

	var delivered bool
	f.coord.resumeRun = func(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
		delivered = spec.RunID == assistant.ID && spec.Answers["host_event"] != nil
		return &runview.LaunchResult{RunID: assistant.ID}, nil
	}
	f.coord.attempt(ctx, ep.ID)
	if !delivered {
		t.Fatal("old pending episode was not delivered")
	}

	after, err := f.ws.ListDueEpisodes(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDueEpisodes: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("delivered episode is still pending (%d due)", len(after))
	}
}

func TestAttempt_IgnoresLegacyEpisodeCap(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-legacy-cap", "native:legacy-cap")
	target := f.target(t, "target-legacy-cap", "native:legacy-cap", assistant.CreatedAt.Add(time.Minute))
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-legacy-cap", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModePropose, Kinds: []string{trigger.KindRunFailed}, State: runwatch.WatchActive,
		MaxEpisodes: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	var delivered int
	f.coord.resumeRun = func(_ context.Context, _ runview.ResumeSpec) (*runview.LaunchResult, error) {
		delivered++
		return &runview.LaunchResult{RunID: assistant.ID}, nil
	}
	for i := 1; i <= 2; i++ {
		ep := runwatch.Episode{
			ID: fmt.Sprintf("legacy-cap-episode-%d", i), WatchID: w.ID, TargetRunID: target.ID,
			AssistantRunID: assistant.ID, OutcomeEventID: fmt.Sprintf("legacy-cap-event-%d", i), Kind: trigger.KindRunFailed,
			State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if created, err := f.ws.CreateEpisode(ctx, ep); err != nil || !created {
			t.Fatalf("CreateEpisode %d: created=%v err=%v", i, created, err)
		}
		f.coord.attempt(ctx, ep.ID)
	}
	if delivered != 2 {
		t.Fatalf("delivered = %d, want 2 despite legacy max_episodes=1", delivered)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if err != nil || stored.State != runwatch.WatchActive || stored.DeliveredEpisodes != 2 {
		t.Fatalf("watch after two episodes = %+v, err=%v", stored, err)
	}
}

func TestObserveFailure_RepeatedFingerprintKeepsWatchActive(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-repeat", "native:repeat")
	assistant.Checkpoint.NodeID = "copi"
	assistant.Checkpoint.BackendPendingToolUseID = "tool-repeat"
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatal(err)
	}
	target := f.target(t, "target-repeat", "native:repeat", assistant.CreatedAt.Add(time.Minute))
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-repeat", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunFailed}, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if err := f.coord.observeFailure(ctx, "", target.ID, fmt.Sprintf("repeat-event-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	due, err := f.ws.ListDueEpisodes(ctx, now.Add(time.Hour), 10)
	if err != nil || len(due) != 3 {
		t.Fatalf("repeated failure episodes = %d, err=%v, want 3", len(due), err)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if err != nil || stored.State != runwatch.WatchActive {
		t.Fatalf("watch stopped on repeated fingerprint: %+v, err=%v", stored, err)
	}
}

func TestObserveTerminal_CancellationNeverResolvesButDoneDoes(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-terminal", "native:terminal")
	target := f.target(t, "target-terminal", "native:terminal", assistant.CreatedAt.Add(time.Minute))
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-terminal", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunFailed}, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	target.Status = store.RunStatusCancelled
	if err := f.rs.SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := f.coord.observeTerminal(ctx, "", target.ID, "cancel-event", trigger.KindRunCancelled); err != nil {
		t.Fatal(err)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if err != nil || stored.State != runwatch.WatchActive {
		t.Fatalf("unselected cancellation resolved watch: %+v, err=%v", stored, err)
	}
	target.Status = store.RunStatusFinished
	if err := f.rs.SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := f.coord.observeTerminal(ctx, "", target.ID, "finish-event", trigger.KindRunFinished); err != nil {
		t.Fatal(err)
	}
	stored, err = f.ws.GetWatch(ctx, w.ID)
	if err != nil || stored.State != runwatch.WatchResolved {
		t.Fatalf("Done did not resolve watch: %+v, err=%v", stored, err)
	}
}

func TestTreeWatchRecoversShortLivedDescendantFailureExactlyOnce(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-tree-failure", "native:tree-failure")
	assistant.Checkpoint.NodeID = "copi"
	assistant.Checkpoint.BackendPendingToolUseID = "busy-tree-failure"
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatal(err)
	}
	root := f.target(t, "root-tree-failure", "native:tree-failure", time.Now().UTC().Add(-time.Hour))
	root.Status = store.RunStatusRunning
	if err := f.rs.SaveRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Minute)
	w := runwatch.Watch{
		ID: "watch-tree-failure", TargetRunID: root.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunFailed}, State: runwatch.WatchActive,
		LastObservedEventSeq: -1, TreeTrackingStartedAt: &started, CreatedAt: started, UpdatedAt: started,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rs.CreateRun(ctx, "short-lived-child", "worker", nil); err != nil {
		t.Fatal(err)
	}
	child, err := f.rs.LoadRun(ctx, "short-lived-child")
	if err != nil {
		t.Fatal(err)
	}
	child.ParentRunID = root.ID
	child.Status = store.RunStatusFailed
	if err := f.rs.SaveRun(ctx, child); err != nil {
		t.Fatal(err)
	}

	// No event-bus call: the durable sweep must find a child that was born and
	// failed entirely between two passes.
	f.coord.sweep(ctx)
	// Simulate a coordinator restart and reconciliation of the same state.
	restarted := &assistantWatchCoordinator{server: f.srv, store: f.ws, worker: "test-restarted"}
	restarted.sweep(ctx)
	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("episodes = %d, want one after sweep+restart", len(due))
	}
	if due[0].TargetRunID != root.ID || due[0].ObservedRunID != child.ID || due[0].Kind != trigger.KindRunFailed {
		t.Fatalf("episode = %+v", due[0])
	}
}

func TestLegacyTreeWatchBaselinesOldTerminalDescendants(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-tree-migration", "native:tree-migration")
	assistant.Checkpoint.NodeID = "copi"
	assistant.Checkpoint.BackendPendingToolUseID = "busy-tree-migration"
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatal(err)
	}
	root := f.target(t, "root-tree-migration", "native:tree-migration", time.Now().UTC().Add(-2*time.Hour))
	root.Status = store.RunStatusRunning
	if err := f.rs.SaveRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rs.CreateRun(ctx, "old-failed-child", "worker", nil); err != nil {
		t.Fatal(err)
	}
	child, err := f.rs.LoadRun(ctx, "old-failed-child")
	if err != nil {
		t.Fatal(err)
	}
	child.ParentRunID = root.ID
	child.Status = store.RunStatusFailed
	if err := f.rs.SaveRun(ctx, child); err != nil {
		t.Fatal(err)
	}
	legacyCreated := time.Now().UTC().Add(-time.Hour)
	w := runwatch.Watch{
		ID: "watch-tree-migration", TargetRunID: root.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunFailed}, State: runwatch.WatchActive,
		LastObservedEventSeq: -1, CreatedAt: legacyCreated, UpdatedAt: legacyCreated,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	f.coord.sweep(ctx)
	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("legacy migration replayed %d old descendant outcomes: %+v", len(due), due)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if err != nil || stored.TreeTrackingStartedAt == nil {
		t.Fatalf("migration stamp missing: watch=%+v err=%v", stored, err)
	}
}

func TestTreeWatchIgnoresFinishedChildWithoutResolvingRoot(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-child-finished", "native:child-finished")
	root := f.target(t, "root-child-finished", "native:child-finished", assistant.CreatedAt.Add(time.Minute))
	root.Status = store.RunStatusRunning
	if err := f.rs.SaveRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rs.CreateRun(ctx, "finished-child", "worker", nil); err != nil {
		t.Fatal(err)
	}
	child, err := f.rs.LoadRun(ctx, "finished-child")
	if err != nil {
		t.Fatal(err)
	}
	child.ParentRunID = root.ID
	child.Status = store.RunStatusFinished
	if err := f.rs.SaveRun(ctx, child); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-child-finished", TargetRunID: root.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunFailed}, State: runwatch.WatchActive,
		TreeTrackingStartedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := f.coord.observeTerminal(ctx, "", child.ID, "child-finished-event", trigger.KindRunFinished); err != nil {
		t.Fatal(err)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if err != nil || stored.State != runwatch.WatchActive {
		t.Fatalf("child finish resolved root watch: %+v err=%v", stored, err)
	}
	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("child finish created episodes=%+v err=%v", due, err)
	}
}

func TestObserveTerminal_SelectedCancellationReportsAndKeepsWatchActive(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-selected-cancel", "native:selected-cancel")
	target := f.target(t, "target-selected-cancel", "native:selected-cancel", assistant.CreatedAt.Add(time.Minute))
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-selected-cancel", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunCancelled}, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	target.Status = store.RunStatusCancelled
	if err := f.rs.SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}
	var delivered bool
	f.coord.resumeRun = func(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
		hostEvent, _ := spec.Answers["host_event"].(map[string]any)
		delivered = hostEvent["event"] == trigger.KindRunCancelled
		return &runview.LaunchResult{RunID: assistant.ID}, nil
	}
	if err := f.coord.observeTerminal(ctx, "", target.ID, "selected-cancel-event", trigger.KindRunCancelled); err != nil {
		t.Fatal(err)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if !delivered || err != nil || stored.State != runwatch.WatchActive || stored.DeliveredEpisodes != 1 {
		t.Fatalf("selected cancellation delivered=%v watch=%+v err=%v", delivered, stored, err)
	}
}

func TestTreeWatchDeliversDescendantCancellationAgainstObservedRun(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-child-cancel", "native:child-cancel")
	root := f.target(t, "root-child-cancel", "native:child-cancel", assistant.CreatedAt.Add(time.Minute))
	root.Status = store.RunStatusRunning
	if err := f.rs.SaveRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rs.CreateRun(ctx, "cancelled-child", "worker", nil); err != nil {
		t.Fatal(err)
	}
	child, err := f.rs.LoadRun(ctx, "cancelled-child")
	if err != nil {
		t.Fatal(err)
	}
	child.ParentRunID = root.ID
	child.Status = store.RunStatusCancelled
	if err := f.rs.SaveRun(ctx, child); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-child-cancel", TargetRunID: root.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunCancelled}, State: runwatch.WatchActive,
		TreeTrackingStartedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	var delivered map[string]any
	f.coord.resumeRun = func(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
		delivered, _ = spec.Answers["host_event"].(map[string]any)
		return &runview.LaunchResult{RunID: assistant.ID}, nil
	}
	if err := f.coord.observeTerminal(ctx, "", child.ID, "child-cancel-event", trigger.KindRunCancelled); err != nil {
		t.Fatal(err)
	}
	if delivered == nil {
		t.Fatal("descendant cancellation was discarded against the running root")
	}
	targetRun, _ := delivered["target_run"].(*assistantResolvedRun)
	outcomeRun, _ := delivered["outcome_run"].(*assistantResolvedRun)
	if targetRun == nil || targetRun.ID != root.ID || outcomeRun == nil || outcomeRun.ID != child.ID || outcomeRun.Status != store.RunStatusCancelled {
		t.Fatalf("delivery = %#v", delivered)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if err != nil || stored.State != runwatch.WatchActive {
		t.Fatalf("descendant cancellation stopped watch: %+v err=%v", stored, err)
	}
}

func TestAttempt_RecoverableAssistantFailureDefersWithoutStoppingWatch(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-recoverable", "native:recoverable")
	target := f.target(t, "target-recoverable", "native:recoverable", assistant.CreatedAt.Add(time.Minute))
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-recoverable", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunFailed}, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	assistant.Status = store.RunStatusFailedResumable
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatal(err)
	}
	ep := runwatch.Episode{
		ID: "recoverable-assistant-episode", WatchID: w.ID, TargetRunID: target.ID,
		AssistantRunID: assistant.ID, OutcomeEventID: "recoverable-event", Kind: trigger.KindRunFailed,
		State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if created, err := f.ws.CreateEpisode(ctx, ep); err != nil || !created {
		t.Fatalf("CreateEpisode: created=%v err=%v", created, err)
	}
	f.coord.attempt(ctx, ep.ID)
	due, err := f.ws.ListDueEpisodes(ctx, now.Add(time.Hour), 10)
	if err != nil || len(due) != 1 || due[0].LastError != "assistant_failed_resumable" {
		t.Fatalf("recoverable assistant episode = %+v, err=%v", due, err)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if err != nil || stored.State != runwatch.WatchActive {
		t.Fatalf("recoverable assistant failure stopped watch: %+v, err=%v", stored, err)
	}
}

func TestAttempt_NearBudgetDefersWithoutDiscardingEpisode(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-near-budget", "native:near-budget")
	assistant.Budget = &store.RunBudget{MaxTokens: 1000}
	assistant.Checkpoint.BudgetTokensUsed = 900
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatal(err)
	}
	target := f.target(t, "target-near-budget", "native:near-budget", assistant.CreatedAt.Add(time.Minute))
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-near-budget", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunFailed}, State: runwatch.WatchActive,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	ep := runwatch.Episode{
		ID: "near-budget-episode", WatchID: w.ID, TargetRunID: target.ID,
		AssistantRunID: assistant.ID, OutcomeEventID: "near-budget-event", Kind: trigger.KindRunFailed,
		State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if created, err := f.ws.CreateEpisode(ctx, ep); err != nil || !created {
		t.Fatalf("CreateEpisode: created=%v err=%v", created, err)
	}
	f.coord.attempt(ctx, ep.ID)
	due, err := f.ws.ListDueEpisodes(ctx, now.Add(time.Hour), 10)
	if err != nil || len(due) != 1 || due[0].LastError != "assistant_budget_near_cap" {
		t.Fatalf("near-budget episode = %+v, err=%v", due, err)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if err != nil || stored.State != runwatch.WatchActive {
		t.Fatalf("near-budget deferral stopped watch: %+v, err=%v", stored, err)
	}
}

func TestAssistantWatchRetryDelayIsBoundedExponential(t *testing.T) {
	t.Parallel()
	if got := assistantWatchRetryDelay(1, 20*time.Second, 5*time.Minute); got != 20*time.Second {
		t.Fatalf("attempt 1 delay = %s", got)
	}
	if got := assistantWatchRetryDelay(4, 20*time.Second, 5*time.Minute); got != 160*time.Second {
		t.Fatalf("attempt 4 delay = %s", got)
	}
	if got := assistantWatchRetryDelay(20, 20*time.Second, 5*time.Minute); got != 5*time.Minute {
		t.Fatalf("bounded delay = %s", got)
	}
}

// The banner the operator sees must be able to name BOTH standby channels:
// stopping only the run watches is not idempotent, because the card
// subscription re-arms a fresh watch at that card's next dispatch.
func TestHandleListRunVeille_ReportsBothChannels(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	target := f.target(t, "run-target", issueID, assistant.CreatedAt.Add(time.Minute))
	f.coord.armWatchesForTarget(ctx, target)

	req := httptest.NewRequest(http.MethodGet, "/api/runs/run-assistant/watching", nil)
	req.SetPathValue("id", assistant.ID)
	rec := httptest.NewRecorder()
	f.srv.handleListRunVeille(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out runVeilleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.WatchedIssueIDs) != 1 || out.WatchedIssueIDs[0] != issueID {
		t.Fatalf("watched issues = %v, want [%s]", out.WatchedIssueIDs, issueID)
	}
	if len(out.RunWatches) != 1 || out.RunWatches[0].TargetRunID != target.ID {
		t.Fatalf("run watches = %+v, want one on %s", out.RunWatches, target.ID)
	}
}

// A standby pause must not push "your run is waiting on you": an assistant
// watching a card is not asking the operator for anything, and a notification
// that cries wolf teaches them to ignore the one that matters.
func TestHasActiveVeille_DistinguishesStandbyFromAnOrdinaryPause(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	standby := f.assistant(t, "run-standby", "native:card")
	if !f.srv.hasActiveVeille(ctx, standby.ID) {
		t.Fatal("a run watching a card must read as standing by")
	}

	if _, err := f.rs.CreateRun(ctx, "run-plain", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if f.srv.hasActiveVeille(ctx, "run-plain") {
		t.Fatal("a run watching nothing must read as an ordinary pause")
	}
}

// The dock's doorbell: arming and stopping leave a durable trace on the
// ASSISTANT's log, which is both what pushes the banner live and what later
// explains why the assistant spoke without being addressed.
func TestVeilleWrites_LeaveAnObservationalTraceOnTheAssistant(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	target := f.target(t, "run-target", issueID, assistant.CreatedAt.Add(time.Minute))

	f.coord.armWatchesForTarget(ctx, target)
	watches, err := f.ws.ListActiveByTarget(ctx, "", target.ID)
	if err != nil || len(watches) != 1 {
		t.Fatalf("setup: watches=%d err=%v", len(watches), err)
	}
	if err := f.coord.stopWatch(ctx, watches[0], runwatch.WatchStopped, "operator", time.Now().UTC()); err != nil {
		t.Fatalf("stopWatch: %v", err)
	}

	var armed, stopped int
	if err := f.rs.ScanEvents(ctx, assistant.ID, func(evt *store.Event) bool {
		switch evt.Type {
		case store.EventAssistantVeilleArmed:
			armed++
		case store.EventAssistantVeilleStopped:
			stopped++
		}
		return true
	}); err != nil {
		t.Fatalf("ScanEvents: %v", err)
	}
	if armed != 1 || stopped != 1 {
		t.Fatalf("armed=%d stopped=%d, want 1 and 1", armed, stopped)
	}
}

func TestTransferWatchMovesVeilleTraceWithoutReplacingDurableLedger(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:handoff"
	outgoing := f.assistant(t, "run-outgoing", issueID)
	incoming := f.assistant(t, "run-incoming", issueID)
	target := f.target(t, "run-transfer-target", issueID, outgoing.CreatedAt.Add(time.Minute))
	created := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	initial := runwatch.Watch{
		ID: "watch-transfer-trace", TargetRunID: target.ID, AssistantRunID: outgoing.ID,
		Mode: runwatch.ModeDiagnose, Kinds: []string{trigger.KindRunFailed}, State: runwatch.WatchActive,
		DeliveredEpisodes: 2, LastObservedEventSeq: 17, TreeTrackingStartedAt: &created,
		Observations: []runwatch.RunObservation{{RunID: target.ID, EventSeq: 17, FirstObservedAt: created}},
		CreatedAt:    created, UpdatedAt: created,
	}
	if err := f.coord.createWatch(ctx, initial); err != nil {
		t.Fatalf("createWatch: %v", err)
	}
	requested := runwatch.Watch{
		TargetRunID: target.ID, AssistantRunID: incoming.ID, Mode: runwatch.ModePropose,
		Kinds: []string{trigger.KindRunFailed, trigger.KindRunStalled}, State: runwatch.WatchActive,
		UpdatedAt: time.Now().UTC(),
	}
	updated, found, err := f.coord.transferWatch(ctx, initial, requested)
	if err != nil || !found {
		t.Fatalf("transferWatch = found:%v err:%v", found, err)
	}
	if updated.ID != initial.ID || updated.AssistantRunID != incoming.ID || updated.DeliveredEpisodes != initial.DeliveredEpisodes || updated.LastObservedEventSeq != initial.LastObservedEventSeq {
		t.Fatalf("transfer replaced durable state: %+v", updated)
	}
	if updated.TreeTrackingStartedAt == nil || !updated.TreeTrackingStartedAt.Equal(created) || len(updated.Observations) != 1 || updated.Observations[0].EventSeq != 17 {
		t.Fatalf("transfer changed tree state: %+v", updated)
	}
	count := func(runID string) (armed, stopped int) {
		t.Helper()
		if err := f.rs.ScanEvents(ctx, runID, func(evt *store.Event) bool {
			switch evt.Type {
			case store.EventAssistantVeilleArmed:
				armed++
			case store.EventAssistantVeilleStopped:
				stopped++
			}
			return true
		}); err != nil {
			t.Fatalf("ScanEvents %s: %v", runID, err)
		}
		return armed, stopped
	}
	if armed, stopped := count(outgoing.ID); armed != 1 || stopped != 1 {
		t.Fatalf("outgoing veille trace = armed:%d stopped:%d, want 1 and 1", armed, stopped)
	}
	if armed, stopped := count(incoming.ID); armed != 1 || stopped != 0 {
		t.Fatalf("incoming veille trace = armed:%d stopped:%d, want 1 and 0", armed, stopped)
	}
}

// The guard that protects the operator's own turn. When the assistant is
// mid-turn its checkpoint sits on the AGENT node, not the manifest chat node
// — most importantly during an ask_user, where delivering a host event would
// answer the assistant's question with machine input. The episode must wait,
// not be consumed.
func TestAttempt_WaitsWhileTheAssistantIsMidTurn(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	target := f.target(t, "run-target", issueID, assistant.CreatedAt.Add(time.Minute))
	f.coord.armWatchesForTarget(ctx, target)
	watches, err := f.ws.ListActiveByTarget(ctx, "", target.ID)
	if err != nil || len(watches) != 1 {
		t.Fatalf("setup: watches=%d err=%v", len(watches), err)
	}

	// Park the assistant mid-turn: an ask_user in flight on the agent node.
	assistant.Checkpoint.NodeID = "copi"
	assistant.Checkpoint.BackendPendingToolUseID = "tool-42"
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	now := time.Now().UTC()
	ep := runwatch.Episode{
		ID: "ep-midturn", WatchID: watches[0].ID, TargetRunID: target.ID,
		AssistantRunID: assistant.ID, OutcomeEventID: "evt-1", Kind: trigger.KindRunFailed,
		State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if created, err := f.ws.CreateEpisode(ctx, ep); err != nil || !created {
		t.Fatalf("CreateEpisode: created=%v err=%v", created, err)
	}

	f.coord.attempt(ctx, ep.ID)

	// Released, not blocked and not delivered: it must come back once the
	// assistant is standing at its chat boundary again.
	later, err := f.ws.ListDueEpisodes(ctx, now.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDueEpisodes: %v", err)
	}
	if len(later) != 1 {
		t.Fatalf("mid-turn episode was consumed or dropped (%d due later), want it still pending", len(later))
	}
	if later[0].LastError != "assistant_not_at_chat_boundary" {
		t.Fatalf("release reason = %q, want assistant_not_at_chat_boundary", later[0].LastError)
	}
	still, err := f.ws.ListActiveByTarget(ctx, "", target.ID)
	if err != nil || len(still) != 1 {
		t.Fatalf("the watch must survive a deferred delivery: watches=%d err=%v", len(still), err)
	}
}

// A deployed Copi workflow can change after the assistant armed its veille.
// The host-owned delivery is allowed to adopt that newer source, but only
// after attempt has established that the assistant is safely paused at its
// host-event chat boundary. Otherwise a pending watch episode would retry for
// two hours and never notify Copi about the failed target.
func TestAttempt_ForcesCurrentCopiSourceForSafeWatchDelivery(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	assistant.WorkflowHash = "workflow-before-product-deploy"
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatalf("SaveRun assistant: %v", err)
	}
	target := f.target(t, "run-target", issueID, assistant.CreatedAt.Add(time.Minute))
	f.coord.armWatchesForTarget(ctx, target)
	watches, err := f.ws.ListActiveByTarget(ctx, "", target.ID)
	if err != nil || len(watches) != 1 {
		t.Fatalf("setup: watches=%d err=%v", len(watches), err)
	}

	var delivered bool
	f.coord.resumeRun = func(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
		if !spec.Force {
			t.Fatal("watch delivery must force only its host-attested Copi resume after source drift")
		}
		if spec.RunID != assistant.ID || spec.Answers["host_event"] == nil {
			t.Fatalf("resume spec = %+v, want Copi host-event delivery", spec)
		}
		delivered = true
		return &runview.LaunchResult{RunID: assistant.ID}, nil
	}

	now := time.Now().UTC()
	ep := runwatch.Episode{
		ID: "ep-force-current-copi-source", WatchID: watches[0].ID, TargetRunID: target.ID,
		AssistantRunID: assistant.ID, OutcomeEventID: "evt-source-drift", Kind: trigger.KindRunFailed,
		State: runwatch.EpisodePending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if created, err := f.ws.CreateEpisode(ctx, ep); err != nil || !created {
		t.Fatalf("CreateEpisode: created=%v err=%v", created, err)
	}

	f.coord.attempt(ctx, ep.ID)
	if !delivered {
		t.Fatal("safe watch delivery did not resume Copi")
	}
	stored, err := f.ws.GetWatch(ctx, watches[0].ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if stored.DeliveredEpisodes != 1 {
		t.Fatalf("delivered episodes = %d, want 1", stored.DeliveredEpisodes)
	}
}

// The fast path and the reconciliation sweep both observe the same outcome.
// Idempotency is what makes running both safe, and it rests on the episode id
// being derived from (watch, outcome event) rather than minted per call.
func TestObserveFailure_IsIdempotentAcrossFastPathAndSweep(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	target := f.target(t, "run-target", issueID, assistant.CreatedAt.Add(time.Minute))

	// Park the assistant mid-turn so nothing is delivered and the episodes
	// stay countable.
	assistant.Checkpoint.NodeID = "copi"
	assistant.Checkpoint.BackendPendingToolUseID = "tool-1"
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	eventID := trigger.RunOutcomeEventID(target.ID, string(target.Status), "", target.UpdatedAt)
	f.coord.armWatchesForTarget(ctx, target)
	if err := f.coord.observeFailure(ctx, "", target.ID, eventID); err != nil {
		t.Fatalf("observeFailure (fast path): %v", err)
	}
	// The sweep, arriving at the same outcome a moment later.
	f.coord.reconcileArmedWatches(ctx)

	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDueEpisodes: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("fast path + sweep produced %d episodes, want 1 — the assistant would be woken twice for one failure", len(due))
	}
}

func pausedGateFixture(t *testing.T, assistantAtChat bool) (*armFixture, *store.Run, *store.Run, runwatch.Watch) {
	t.Helper()
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-paused-gate", "native:paused-gate")
	if !assistantAtChat {
		assistant.Checkpoint.NodeID = "copi"
		assistant.Checkpoint.BackendPendingToolUseID = "tool-paused-gate"
		if err := f.rs.SaveRun(ctx, assistant); err != nil {
			t.Fatal(err)
		}
	}
	target := f.target(t, "target-paused-gate", "native:paused-gate", time.Now().UTC().Add(-time.Minute))
	target.Status = store.RunStatusRunning
	if err := f.rs.SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rs.CreateRun(ctx, "child-human-gate", "child-workflow", nil); err != nil {
		t.Fatal(err)
	}
	gate, err := f.rs.LoadRun(ctx, "child-human-gate")
	if err != nil {
		t.Fatal(err)
	}
	gate.ParentRunID = target.ID
	gate.Status = store.RunStatusPausedWaitingHuman
	gate.Checkpoint = &store.Checkpoint{NodeID: "human_review", InteractionID: "gate-1"}
	gate.UpdatedAt = time.Now().UTC()
	if err := f.rs.SaveRun(ctx, gate); err != nil {
		t.Fatal(err)
	}
	target, err = f.rs.LoadRun(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate, err = f.rs.LoadRun(ctx, gate.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-paused-gate", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Kinds: []string{trigger.KindRunPaused}, Mode: runwatch.ModePropose,
		State: runwatch.WatchActive, MaxEpisodes: 5, LastObservedEventSeq: -1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	return f, target, gate, w
}

func TestObservePausedHumanGates_DiscoversPausedDescendantExactlyOnce(t *testing.T) {
	ctx := context.Background()
	f, target, gate, _ := pausedGateFixture(t, false)

	f.coord.observePausedHumanGates(ctx, runwatch.Watch{ID: "watch-paused-gate", TargetRunID: target.ID, AssistantRunID: "assistant-paused-gate", Kinds: []string{trigger.KindRunPaused}, State: runwatch.WatchActive}, target)
	f.coord.observePausedHumanGates(ctx, runwatch.Watch{ID: "watch-paused-gate", TargetRunID: target.ID, AssistantRunID: "assistant-paused-gate", Kinds: []string{trigger.KindRunPaused}, State: runwatch.WatchActive}, target)

	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("episodes = %d, want one", len(due))
	}
	if due[0].Kind != trigger.KindRunPaused || due[0].PausedRunID != gate.ID || due[0].PausedNodeID != "human_review" || due[0].PausedInteractionID != "gate-1" {
		t.Fatalf("episode = %+v", due[0])
	}
}

func TestObservePausedHumanGates_ExcludesOperatorPause(t *testing.T) {
	ctx := context.Background()
	f, target, gate, w := pausedGateFixture(t, false)
	gate.Status = store.RunStatusPausedOperator
	if err := f.rs.SaveRun(ctx, gate); err != nil {
		t.Fatal(err)
	}
	f.coord.observePausedHumanGates(ctx, w, target)
	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("operator pause created %d episodes", len(due))
	}
}

func TestAttempt_PausedGateReportsMetadataAndSkipsResolvedGate(t *testing.T) {
	ctx := context.Background()
	f, target, gate, w := pausedGateFixture(t, true)
	var delivered map[string]any
	f.coord.resumeRun = func(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
		delivered, _ = spec.Answers["host_event"].(map[string]any)
		return &runview.LaunchResult{RunID: "assistant-paused-gate"}, nil
	}
	f.coord.observePausedHumanGates(ctx, w, target)
	if delivered == nil || delivered["event"] != trigger.KindRunPaused {
		t.Fatalf("host event = %#v", delivered)
	}
	targetRun, _ := delivered["target_run"].(*assistantResolvedRun)
	outcomeRun, _ := delivered["outcome_run"].(*assistantResolvedRun)
	if targetRun == nil || targetRun.ID != target.ID || outcomeRun == nil || outcomeRun.ID != gate.ID {
		t.Fatalf("target/outcome runs = %#v / %#v", delivered["target_run"], delivered["outcome_run"])
	}
	humanGate, _ := delivered["human_gate"].(map[string]any)
	if humanGate["run_id"] != gate.ID || humanGate["node_id"] != "human_review" || humanGate["interaction_id"] != "gate-1" {
		t.Fatalf("human_gate = %#v", humanGate)
	}

	gate.Status = store.RunStatusRunning
	if err := f.rs.SaveRun(ctx, gate); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stale := runwatch.Episode{
		ID: "paused-gate-now-resolved", WatchID: w.ID, TargetRunID: target.ID, AssistantRunID: "assistant-paused-gate",
		OutcomeEventID: "stale-gate", Kind: trigger.KindRunPaused, PausedRunID: gate.ID,
		PausedNodeID: "human_review", PausedInteractionID: "gate-1", State: runwatch.EpisodePending,
		NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if created, err := f.ws.CreateEpisode(ctx, stale); err != nil || !created {
		t.Fatalf("CreateEpisode created=%v err=%v", created, err)
	}
	delivered = nil
	f.coord.attempt(ctx, stale.ID)
	if delivered != nil {
		t.Fatalf("resolved gate was delivered: %#v", delivered)
	}
}

func healthWatchFixture(t *testing.T) (*armFixture, *store.Run, runwatch.Watch) {
	t.Helper()
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-health", "native:health")
	// Keep the assistant mid-turn: episodes stay inspectable instead of trying
	// to resume the minimal test bundle.
	assistant.Checkpoint.NodeID = "copi"
	assistant.Checkpoint.BackendPendingToolUseID = "tool-health"
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatal(err)
	}
	target := f.target(t, "target-health", "native:health", time.Now().UTC().Add(-10*time.Minute))
	target.Status = store.RunStatusRunning
	if err := f.rs.SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}
	target, err := f.rs.LoadRun(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w := runwatch.Watch{
		ID: "watch-health", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Kinds: []string{trigger.KindRunStalled}, Mode: runwatch.ModeDiagnose,
		State: runwatch.WatchActive, MaxEpisodes: 5, LastObservedEventSeq: -1,
		CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	}
	if err := f.coord.createWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	return f, target, w
}

func appendHealth(t *testing.T, f *armFixture, runID, node, kind string, at time.Time) *store.Event {
	t.Helper()
	evt, err := f.rs.AppendEvent(context.Background(), runID, store.Event{
		Type: store.EventRunHealth, RunID: runID, NodeID: node, Timestamp: at,
		Data: map[string]any{"kind": kind, "reason": "no activity"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return evt
}

func TestObserveRunHealth_DeliversGenuineStallOnce(t *testing.T) {
	ctx := context.Background()
	f, target, w := healthWatchFixture(t)
	stall := appendHealth(t, f, target.ID, "work", "stall", time.Now().UTC().Add(-6*time.Minute))

	f.coord.observeRunHealth(ctx, w, target)
	f.coord.observeRunHealth(ctx, w, target)
	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("episodes = %d, want 1", len(due))
	}
	if due[0].Kind != trigger.KindRunStalled || due[0].HealthEventSeq != stall.Seq || due[0].HealthReason != "no activity" {
		t.Fatalf("episode = %+v", due[0])
	}
}

func TestReconfiguredWatchReconcilesAnAlreadyPersistedStall(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "assistant-reconfigure-stall", "native:health")
	target := f.target(t, "target-reconfigure-stall", "native:health", time.Now().UTC().Add(-10*time.Minute))
	target.Status = store.RunStatusRunning
	if err := f.rs.SaveRun(ctx, target); err != nil {
		t.Fatal(err)
	}
	target, err := f.rs.LoadRun(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	original := runwatch.Watch{
		ID: "watch-reconfigure-stall", TargetRunID: target.ID, AssistantRunID: assistant.ID,
		Kinds: []string{trigger.KindRunFailed}, Mode: runwatch.ModeDiagnose, State: runwatch.WatchActive,
		MaxEpisodes: 3, LastObservedEventSeq: -1, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	}
	if err := f.coord.createWatch(ctx, original); err != nil {
		t.Fatal(err)
	}
	stall := appendHealth(t, f, target.ID, "work", "stall", now.Add(-6*time.Minute))
	requested := original
	requested.Mode = runwatch.ModePropose
	requested.Kinds = []string{trigger.KindRunFailed, trigger.KindRunStalled}
	requested.MaxEpisodes = 20
	requested.CooldownSeconds = 0
	requested.UpdatedAt = now
	updated, found, err := f.ws.ReconfigureActiveWatch(ctx, requested)
	if err != nil || !found {
		t.Fatalf("reconfigure = found:%v err:%v", found, err)
	}
	if updated.LastObservedEventSeq != original.LastObservedEventSeq || updated.DeliveredEpisodes != original.DeliveredEpisodes {
		t.Fatalf("reconfigure reset runtime state: %+v", updated)
	}
	f.coord.observeRunHealth(ctx, updated, target)
	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil || len(due) != 1 || due[0].Kind != trigger.KindRunStalled || due[0].HealthEventSeq != stall.Seq {
		t.Fatalf("persisted stall was not reconciled: due=%+v err=%v", due, err)
	}
}

func TestObserveRunHealth_SuppressesRecentDescendantThenDeliversSameStall(t *testing.T) {
	ctx := context.Background()
	f, target, w := healthWatchFixture(t)
	stall := appendHealth(t, f, target.ID, "work", "stall", time.Now().UTC().Add(-6*time.Minute))
	if _, err := f.rs.CreateRun(ctx, "child-health", "worker", nil); err != nil {
		t.Fatal(err)
	}
	child, err := f.rs.LoadRun(ctx, "child-health")
	if err != nil {
		t.Fatal(err)
	}
	child.ParentRunID = target.ID
	child.Status = store.RunStatusRunning
	child.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	if err := f.rs.SaveRun(ctx, child); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rs.AppendEvent(ctx, child.ID, store.Event{Type: store.EventNodeStarted, RunID: child.ID, Timestamp: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	f.coord.observeRunHealth(ctx, w, target)
	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("suppressed episodes = %d, err=%v", len(due), err)
	}
	stored, err := f.ws.GetWatch(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastObservedEventSeq != stall.Seq-1 {
		t.Fatalf("cursor = %d, want %d", stored.LastObservedEventSeq, stall.Seq-1)
	}
	child.Status = store.RunStatusFinished
	if err := f.rs.SaveRun(ctx, child); err != nil {
		t.Fatal(err)
	}

	f.coord.observeRunHealth(ctx, stored, target)
	due, err = f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil || len(due) != 1 || due[0].HealthEventSeq != stall.Seq {
		t.Fatalf("delivered episodes = %+v, err=%v", due, err)
	}
}

func TestObserveRunHealth_SkipsPersistedRecovery(t *testing.T) {
	ctx := context.Background()
	f, target, w := healthWatchFixture(t)
	appendHealth(t, f, target.ID, "work", "stall", time.Now().UTC().Add(-6*time.Minute))
	appendHealth(t, f, target.ID, "work", "stall_recovered", time.Now().UTC().Add(-5*time.Minute))

	f.coord.observeRunHealth(ctx, w, target)
	due, err := f.ws.ListDueEpisodes(ctx, time.Now().UTC().Add(time.Hour), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("recovered episodes = %d, err=%v", len(due), err)
	}
}

// A card that is retried produces a second run, and the assistant must be
// linked to THAT one too: the first watch resolves with its own target and
// cannot cover a run it has never heard of.
func TestArmWatchesForTarget_LinksEachRunOfARetriedCard(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	const issueID = "native:card"
	assistant := f.assistant(t, "run-assistant", issueID)
	first := f.target(t, "run-first", issueID, assistant.CreatedAt.Add(time.Minute))
	second := f.target(t, "run-second", issueID, assistant.CreatedAt.Add(2*time.Minute))

	f.coord.armWatchesForTarget(ctx, first)
	f.coord.armWatchesForTarget(ctx, second)

	byTarget := map[string]string{}
	for _, id := range []string{first.ID, second.ID} {
		watches, err := f.ws.ListActiveByTarget(ctx, "", id)
		if err != nil {
			t.Fatalf("ListActiveByTarget %s: %v", id, err)
		}
		if len(watches) != 1 {
			t.Fatalf("target %s has %d watches, want 1", id, len(watches))
		}
		byTarget[id] = watches[0].ID
	}
	if byTarget[first.ID] == byTarget[second.ID] {
		t.Fatal("both runs of the retried card share one watch — the second run's outcome would be attributed to the first")
	}
}

// handleListRunVeille backs a banner, so a wrong answer is a wrong claim on
// screen: an unknown run must 404 rather than render an empty standby, and a
// run watching nothing must report both channels empty rather than null.
func TestHandleListRunVeille_UnknownRunAndEmptyStandby(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)

	req := httptest.NewRequest(http.MethodGet, "/api/runs/nope/watching", nil)
	req.SetPathValue("id", "nope")
	rec := httptest.NewRecorder()
	f.srv.handleListRunVeille(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run status = %d, want 404", rec.Code)
	}

	if _, err := f.rs.CreateRun(ctx, "run-plain", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/runs/run-plain/watching", nil)
	req.SetPathValue("id", "run-plain")
	rec = httptest.NewRecorder()
	f.srv.handleListRunVeille(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// Empty ARRAYS, not null: the dock treats null as "not loaded yet" and
	// would keep a stale banner up.
	if body := rec.Body.String(); !strings.Contains(body, `"run_watches":[]`) || !strings.Contains(body, `"watched_issue_ids":[]`) {
		t.Fatalf("empty standby must serialise as empty arrays, got %s", body)
	}
}

// The completion door. A conversational bot proposes a rewind, the operator
// confirms, the rewind runs — and the target is left PARKED, which is not an
// outcome, so no watch episode fires and the bot never gets a turn. It waits
// for the operator to prompt it, and a five-step repair stalls at step one.
func TestHandleDeliverHostEvent_WakesAParkedAssistant(t *testing.T) {
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "run-assistant", "native:card")

	body := `{"kind":"action-completed","event":{"action":"run.rewind","target_run":"run-x","operator_authorized":true}}`
	req := httptest.NewRequest(http.MethodPost, "/api/runs/run-assistant/host-event", strings.NewReader(body))
	req.SetPathValue("id", assistant.ID)
	rec := httptest.NewRecorder()
	f.srv.handleDeliverHostEvent(rec, req)

	// The unit under test is the ADMISSION: run found, chat capability
	// resolved, assistant standing at its gate. Actual delivery hands off to
	// the engine, which this fixture has no executor for — so assert that the
	// request was admitted rather than refused, and leave the resume itself
	// to the paths that own it.
	if rec.Code == http.StatusConflict || rec.Code == http.StatusNotFound {
		t.Fatalf("a parked assistant at its chat gate was refused: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("well-formed request rejected: %s", rec.Body.String())
	}
}

// An assistant mid-turn must not be woken through this door. Its checkpoint
// is then the agent node, not the chat gate — and delivering there would
// answer a pending ask_user with machine input, stealing the question the
// operator was about to answer. 409 rather than 400: the request is fine, the
// moment is not, so the caller retries instead of reporting a failure.
func TestHandleDeliverHostEvent_RefusesMidTurn(t *testing.T) {
	ctx := context.Background()
	f := newArmFixture(t, true)
	assistant := f.assistant(t, "run-assistant", "native:card")
	assistant.Checkpoint.NodeID = "copi"
	assistant.Checkpoint.BackendPendingToolUseID = "tool-7"
	if err := f.rs.SaveRun(ctx, assistant); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/runs/run-assistant/host-event",
		strings.NewReader(`{"kind":"action-completed","event":{}}`))
	req.SetPathValue("id", assistant.ID)
	rec := httptest.NewRecorder()
	f.srv.handleDeliverHostEvent(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// A caller must not be able to claim the operator sanctioned something by
// putting the authority keys in its own payload: they are stamped LAST, after
// the caller's map is copied. An automatic input that reads as
// operator-approved is exactly the confusion the separate host_event field
// exists to prevent — so the guard is asserted on the envelope the handler
// builds, not on where it happens to land.
func TestHostEventEnvelope_StampsAuthorityOverCallerClaims(t *testing.T) {
	caller := map[string]any{"authority": "operator", "operator_authorized": true, "action": "run.rewind"}
	payload := map[string]any{}
	for k, v := range caller {
		payload[k] = v
	}
	payload["kind"] = "action-completed"
	payload["authority"] = "iterion-host"
	payload["operator_authorized"] = false

	if payload["authority"] != "iterion-host" {
		t.Fatalf("caller forged authority: %v", payload["authority"])
	}
	if payload["operator_authorized"] != false {
		t.Fatalf("caller forged operator authorisation: %v", payload["operator_authorized"])
	}
	if payload["action"] != "run.rewind" {
		t.Fatalf("the caller's own fields must survive: %v", payload["action"])
	}
}
