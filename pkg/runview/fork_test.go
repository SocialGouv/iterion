package runview

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestFork_HappyPath exercises Service.Fork end-to-end against a
// filesystem-backed run with one captured turn checkpoint. Asserts
// the child run is minted with the expected fork anchor, status,
// and rehydrated backend conversation.
func TestFork_HappyPath(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	parentID := "run-fork-parent"
	if _, err := st.CreateRun(context.Background(), parentID, "wf", map[string]any{"x": 1}); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	// Park the parent with a checkpoint shaped like the engine would
	// have left after running a couple of nodes.
	parent, err := st.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	parent.Checkpoint = &store.Checkpoint{
		NodeID:           "step2",
		ArtifactVersions: map[string]int{"source": 1, "step1": 1, "legacy": 1},
		ArtifactRevisions: map[string]store.ArtifactRevisionRef{
			"analysis": {NodeID: "step1", Version: 0},
		},
		Outputs: map[string]map[string]any{
			"step1":  {"value": "alpha"},
			"legacy": {"value": "old-checkpoint"},
		},
		Vars: map[string]any{"workflow_var": "v"},
	}
	parent.WorkflowHash = "hash-abc"
	parent.Status = store.RunStatusCancelled
	// The parent was dispatched from a board issue: the fork must carry
	// the same source edge, or the pipeline card keeps pointing at the
	// dead parent with no way to re-attach the live fork. The schedule
	// fields are deliberately set too — to prove they do NOT travel:
	// ScheduleID feeds the schedgate overlap gate, so inheriting it
	// would wire the recovery fork into the schedule's skip/supersede
	// decisions.
	parent.Source = &store.RunSource{
		Kind:            store.RunSourceKindDispatcher,
		IssueID:         "native:issue-1",
		IssueIdentifier: "issue-1",
		IssueTitle:      "Ship it",
		ScheduleID:      "nightly",
		ScheduleName:    "Nightly",
	}
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}
	if err := st.WriteArtifact(context.Background(), &store.Artifact{
		RunID: parentID, NodeID: "source", Version: 0, Data: map[string]any{"seed": true},
	}); err != nil {
		t.Fatalf("write source artifact: %v", err)
	}
	if err := st.WriteArtifact(context.Background(), &store.Artifact{
		RunID: parentID, NodeID: "step1", Version: 0, Data: map[string]any{"value": "alpha"},
		Contract: &store.ArtifactContract{
			LogicalRef: "analysis", ProducerNode: "step1", Version: 0,
			Dependencies: []store.ArtifactDependency{{LogicalRef: "source", NodeID: "source", Version: 0, Required: true}},
		},
	}); err != nil {
		t.Fatalf("write parent artifact: %v", err)
	}
	if err := st.WriteArtifact(context.Background(), &store.Artifact{
		RunID: parentID, NodeID: "legacy", Version: 0, Data: map[string]any{"value": "old-checkpoint"},
	}); err != nil {
		t.Fatalf("write legacy parent artifact: %v", err)
	}
	// Write a turn checkpoint that the Fork resolver picks up.
	turnCP := &store.TurnCheckpoint{
		RunID:        parentID,
		NodeID:       "step2",
		LoopIter:     0,
		TurnIndex:    3,
		Backend:      "claw",
		FinishReason: "tool_use",
		MessagesRef:  "step2/0/3.messages.json",
		Messages:     json.RawMessage(`[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]`),
		WrittenAt:    time.Now().UTC(),
	}
	if err := st.WriteTurn(context.Background(), turnCP); err != nil {
		t.Fatalf("write turn: %v", err)
	}

	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := svc.Fork(context.Background(), ForkSpec{
		RunID:     parentID,
		NodeID:    "step2",
		TurnIndex: 3,
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if result.NewRunID == "" {
		t.Fatal("expected non-empty new_run_id")
	}
	if result.ParentRunID != parentID {
		t.Errorf("parent_run_id = %q, want %q", result.ParentRunID, parentID)
	}
	if result.ForkAnchor == nil || result.ForkAnchor.NodeID != "step2" || result.ForkAnchor.TurnIndex != 3 {
		t.Errorf("fork_anchor = %+v, want node=step2 turn=3", result.ForkAnchor)
	}
	child, err := st.LoadRun(context.Background(), result.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.ForkedFrom != parentID {
		t.Errorf("child.ForkedFrom = %q, want %q", child.ForkedFrom, parentID)
	}
	if child.ParentRunID != parentID {
		t.Errorf("child.ParentRunID = %q, want %q", child.ParentRunID, parentID)
	}
	if child.Source == nil {
		t.Fatal("child.Source = nil, want inherited from the parent so the board card follows the fork")
	}
	if child.Source.IssueID != "native:issue-1" {
		t.Errorf("child.Source = %+v, want issue provenance on native:issue-1", child.Source)
	}
	if child.Source.Kind != "" {
		t.Errorf("child.Source.Kind = %q, want empty — Kind is the parent's trigger classification; the fork's own is \"fork\" via ForkedFrom", child.Source.Kind)
	}
	if child.Source.ScheduleID != "" || child.Source.ScheduleName != "" {
		t.Errorf("child.Source inherited the schedule identity (%q/%q) — the schedgate overlap gate must not see the fork",
			child.Source.ScheduleID, child.Source.ScheduleName)
	}
	// The parent's own record must survive the inheritance untouched.
	// Comparing pointers here could never fail — Fork loads its own copy
	// of the parent from the store — so re-read the persisted parent.
	parentAfter, err := st.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if parentAfter.Source == nil || parentAfter.Source.Kind != store.RunSourceKindDispatcher ||
		parentAfter.Source.IssueID != "native:issue-1" || parentAfter.Source.ScheduleID != "nightly" {
		t.Errorf("parent.Source = %+v, want the parent's own source left intact", parentAfter.Source)
	}
	children, err := svc.ListChildren(context.Background(), parentID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 1 || children[0].ID != child.ID {
		t.Fatalf("ListChildren(%q) = %+v, want child %q", parentID, children, child.ID)
	}
	if child.SourceHash != "hash-abc" {
		t.Errorf("child.SourceHash = %q, want hash-abc", child.SourceHash)
	}
	if child.Status != store.RunStatusCancelled {
		t.Errorf("child.Status = %q, want cancelled (ready for Resume)", child.Status)
	}
	if child.Checkpoint == nil {
		t.Fatal("expected non-nil checkpoint on child")
	}
	if child.Checkpoint.NodeID != "step2" {
		t.Errorf("child checkpoint NodeID = %q, want step2", child.Checkpoint.NodeID)
	}
	if len(child.Checkpoint.BackendConversation) == 0 {
		t.Error("expected child.Checkpoint.BackendConversation populated from turn messages")
	}
	// step2's stale output (parent had none here) should be absent so
	// re-execution starts fresh.
	if _, ok := child.Checkpoint.Outputs["step2"]; ok {
		t.Error("expected child checkpoint Outputs to not carry the anchor node's stale output")
	}
	// step1's upstream output is preserved.
	if v := child.Checkpoint.Outputs["step1"]["value"]; v != "alpha" {
		t.Errorf("child upstream output step1.value = %v, want alpha", v)
	}
	for _, revision := range []store.ArtifactRevisionRef{{NodeID: "step1", Version: 0}, {NodeID: "source", Version: 0}, {NodeID: "legacy", Version: 0}} {
		artifact, err := st.LoadArtifact(context.Background(), child.ID, revision.NodeID, revision.Version)
		if err != nil {
			t.Fatalf("load copied child artifact %s/%d: %v", revision.NodeID, revision.Version, err)
		}
		if artifact.RunID != child.ID {
			t.Fatalf("copied artifact run id = %q, want %q", artifact.RunID, child.ID)
		}
	}
}

type recordingArtifactStore struct {
	store.RunStore
	childRunID string
	writes     []store.ArtifactRevisionRef
}

func (s *recordingArtifactStore) WriteArtifact(ctx context.Context, artifact *store.Artifact) error {
	if artifact.RunID == s.childRunID {
		s.writes = append(s.writes, store.ArtifactRevisionRef{NodeID: artifact.NodeID, Version: artifact.Version})
	}
	return s.RunStore.WriteArtifact(ctx, artifact)
}

func TestCopyForkArtifactsWritesEachNodeInVersionOrder(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const parentID, childID = "fork-order-parent", "fork-order-child"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, childID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []*store.Artifact{
		{RunID: parentID, NodeID: "a", Version: 0, Data: map[string]any{"version": 0}},
		{RunID: parentID, NodeID: "a", Version: 1, Data: map[string]any{"version": 1}},
		{
			RunID: parentID, NodeID: "z", Version: 0, Data: map[string]any{"version": 0},
			Contract: &store.ArtifactContract{
				LogicalRef: "z", ProducerNode: "z", Version: 0,
				Dependencies: []store.ArtifactDependency{{LogicalRef: "a", NodeID: "a", Version: 0, Required: true}},
			},
		},
	} {
		if err := st.WriteArtifact(ctx, artifact); err != nil {
			t.Fatal(err)
		}
	}
	recording := &recordingArtifactStore{RunStore: st, childRunID: childID}
	if err := copyForkArtifacts(ctx, recording, parentID, childID, []store.ArtifactRevisionRef{
		{NodeID: "a", Version: 1},
		{NodeID: "z", Version: 0},
	}, nil); err != nil {
		t.Fatal(err)
	}
	want := []store.ArtifactRevisionRef{{NodeID: "a", Version: 0}, {NodeID: "a", Version: 1}, {NodeID: "z", Version: 0}}
	if len(recording.writes) != len(want) {
		t.Fatalf("copy order = %+v, want %+v", recording.writes, want)
	}
	for i := range want {
		if recording.writes[i] != want[i] {
			t.Fatalf("copy order = %+v, want %+v", recording.writes, want)
		}
	}
}

func TestCopyForkArtifactsResolvesLogicalOnlyDependency(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const parentID, childID = "fork-logical-parent", "fork-logical-child"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, childID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteArtifact(ctx, &store.Artifact{
		RunID: parentID, NodeID: "planner", Version: 0, Data: map[string]any{"ok": true},
		Contract: &store.ArtifactContract{LogicalRef: "plan", ProducerNode: "planner", Version: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteArtifact(ctx, &store.Artifact{
		RunID: parentID, NodeID: "writer", Version: 0, Data: map[string]any{"ok": true},
		Contract: &store.ArtifactContract{
			LogicalRef: "report", ProducerNode: "writer", Version: 0,
			Dependencies: []store.ArtifactDependency{{LogicalRef: "plan", Version: 0, Required: true}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	cp := &store.Checkpoint{ArtifactRevisions: map[string]store.ArtifactRevisionRef{
		"plan":   {NodeID: "planner", Version: 0},
		"report": {NodeID: "writer", Version: 0},
	}}
	exact, inferred := forkArtifactRevisions(cp)
	if err := copyForkArtifacts(ctx, st, parentID, childID, exact, inferred); err != nil {
		t.Fatalf("logical-only dependency was not resolved: %v", err)
	}
	if _, err := st.LoadArtifact(ctx, childID, "planner", 0); err != nil {
		t.Fatalf("dependency was not copied to child: %v", err)
	}
}

func TestCopyForkArtifactsTreatsOnlyInferredRootsAsOptional(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const parentID, childID = "fork-optional-parent", "fork-optional-child"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, childID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	missing := []store.ArtifactRevisionRef{{NodeID: "legacy", Version: 0}}
	if err := copyForkArtifacts(ctx, st, parentID, childID, nil, missing); err != nil {
		t.Fatalf("inferred legacy root should fall back to checkpoint outputs: %v", err)
	}
	if err := copyForkArtifacts(ctx, st, parentID, childID, missing, nil); err == nil {
		t.Fatal("exact checkpoint revision unexpectedly became optional")
	}
}

func TestCopyForkArtifactsDoesNotCacheFailedOptionalRoot(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const parentID, childID = "fork-optional-dependency-parent", "fork-optional-dependency-child"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRun(ctx, childID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteArtifact(ctx, &store.Artifact{
		RunID: parentID, NodeID: "consumer", Version: 0, Data: map[string]any{"value": "derived"},
		Contract: &store.ArtifactContract{
			LogicalRef: "consumer", ProducerNode: "consumer", Version: 0,
			Dependencies: []store.ArtifactDependency{{LogicalRef: "missing", NodeID: "missing", Version: 0, Required: true}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	inferred := []store.ArtifactRevisionRef{
		{NodeID: "missing", Version: 0},
		{NodeID: "consumer", Version: 0},
	}
	if err := copyForkArtifacts(ctx, st, parentID, childID, nil, inferred); err == nil {
		t.Fatal("consumer was copied after its previously skipped required dependency remained unavailable")
	}
	if _, err := st.LoadArtifact(ctx, childID, "consumer", 0); err == nil {
		t.Fatal("consumer artifact was written despite its missing required dependency")
	}
}

func TestForkBeforeFirstCheckpointKeepsLegacyArtifactStateUnknown(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const parentID = "fork-before-first-checkpoint"
	if _, err := st.CreateRun(ctx, parentID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteTurn(ctx, &store.TurnCheckpoint{
		RunID: parentID, NodeID: "first", TurnIndex: 0, Backend: "claude_code", WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := svc.Fork(ctx, ForkSpec{RunID: parentID, NodeID: "first", TurnIndex: 0})
	if err != nil {
		t.Fatalf("Fork before first completed node: %v", err)
	}
	child, err := st.LoadRun(ctx, result.NewRunID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Checkpoint == nil {
		t.Fatal("fork did not create its synthetic checkpoint")
	}
	if child.Checkpoint.ArtifactRevisionsKnown {
		t.Fatal("fork invented authoritative artifact provenance without a parent checkpoint")
	}
}

// TestFork_LatestTurn confirms that passing turn_index=-1 picks the
// most-recent turn captured for the node.
func TestFork_LatestTurn(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	parentID := "run-fork-latest"
	if _, err := st.CreateRun(context.Background(), parentID, "wf", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	parent, _ := st.LoadRun(context.Background(), parentID)
	parent.Checkpoint = &store.Checkpoint{NodeID: "nodeA", Outputs: map[string]map[string]any{}}
	parent.Status = store.RunStatusCancelled
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatalf("save: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		t.Helper()
		if err := st.WriteTurn(context.Background(), &store.TurnCheckpoint{
			RunID:     parentID,
			NodeID:    "nodeA",
			LoopIter:  0,
			TurnIndex: i,
			Backend:   "claw",
			WrittenAt: now.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("write turn %d: %v", i, err)
		}
	}
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := svc.Fork(context.Background(), ForkSpec{
		RunID:     parentID,
		NodeID:    "nodeA",
		TurnIndex: -1,
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if result.ForkAnchor.TurnIndex != 2 {
		t.Errorf("latest turn anchor = %d, want 2", result.ForkAnchor.TurnIndex)
	}
}

// TestFork_IssueLessParentMintsNoSource: a parent whose Source carries no
// IssueID (e.g. schedule-launched: Kind + ScheduleID only) gives the fork
// NO Source at all. Inheriting a Kind-only shell would relabel the fork
// as a scheduled run — provenance it does not have — and buy nothing for
// the board fix, which keys on IssueID alone.
func TestFork_IssueLessParentMintsNoSource(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	parentID := "run-fork-sched"
	if _, err := st.CreateRun(context.Background(), parentID, "wf", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	parent, _ := st.LoadRun(context.Background(), parentID)
	parent.Checkpoint = &store.Checkpoint{NodeID: "nodeA", Outputs: map[string]map[string]any{}}
	parent.Status = store.RunStatusCancelled
	parent.Source = &store.RunSource{
		Kind:         store.RunSourceKindSchedule,
		ScheduleID:   "nightly",
		ScheduleName: "Nightly",
	}
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.WriteTurn(context.Background(), &store.TurnCheckpoint{
		RunID:     parentID,
		NodeID:    "nodeA",
		Backend:   "claw",
		WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("write turn: %v", err)
	}
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	result, err := svc.Fork(context.Background(), ForkSpec{
		RunID:     parentID,
		NodeID:    "nodeA",
		TurnIndex: -1,
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, err := st.LoadRun(context.Background(), result.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.Source != nil {
		t.Errorf("child.Source = %+v, want nil for an issue-less parent (no phantom schedule provenance)", child.Source)
	}
}

// A repo-targeted (cloud) parent forks into a repo-targeted child: the
// clone coordinates and pinned secret overrides travel, and the child
// NEVER inherits the parent's WorkDir — that path is another pod's
// already-wiped clone, and inheriting it kills the fork's resume in
// "populate workspace: no such file or directory" (measured live on
// 2026-08-20/21, twice).
func TestFork_RepoTargetedParentCarriesCloneCoordinates(t *testing.T) {
	dir := t.TempDir()
	logger := iterlog.Nop()
	st, err := store.New(dir, store.WithLogger(logger))
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	parentID := "run-fork-repo-parent"
	if _, err := st.CreateRun(context.Background(), parentID, "wf", nil); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	parent, err := st.LoadRun(context.Background(), parentID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	parent.Checkpoint = &store.Checkpoint{NodeID: "judge"}
	parent.Status = store.RunStatusFinished
	parent.Worktree = false
	parent.WorkDir = "/tmp/iterion/repos/" + parentID // the dead pod's path
	parent.RepoURL = "https://forge.example/org/app"
	parent.RepoSHA = "feature/branch"
	parent.ProjectPath = "org/app"
	parent.BotID = "golden-master"
	parent.SecretOverrides = map[string]string{"forge_token": "sec-123"}
	if err := st.SaveRun(context.Background(), parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}
	turnCP := &store.TurnCheckpoint{
		RunID: parentID, NodeID: "judge", LoopIter: 0, TurnIndex: 0,
		Backend: "claude_code", WrittenAt: time.Now().UTC(),
	}
	if err := st.WriteTurn(context.Background(), turnCP); err != nil {
		t.Fatalf("write turn: %v", err)
	}
	svc, err := NewService(dir, WithLogger(logger))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Fork(context.Background(), ForkSpec{RunID: parentID, NodeID: "judge", TurnIndex: -1})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	child, err := st.LoadRun(context.Background(), res.NewRunID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.RepoURL != parent.RepoURL || child.RepoSHA != parent.RepoSHA || child.ProjectPath != parent.ProjectPath {
		t.Errorf("clone coordinates did not travel: %q/%q/%q", child.RepoURL, child.RepoSHA, child.ProjectPath)
	}
	if child.BotID != "golden-master" {
		t.Errorf("BotID = %q, want golden-master", child.BotID)
	}
	if child.SecretOverrides["forge_token"] != "sec-123" {
		t.Errorf("SecretOverrides did not travel: %v", child.SecretOverrides)
	}
	if child.WorkDir != "" {
		t.Errorf("child inherited the dead pod's WorkDir %q — must stay empty for a repo-targeted fork", child.WorkDir)
	}
}
