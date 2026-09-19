package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/workspacetrack"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResumeFromFailure_AdvancesPastAnsweredHumanNode asserts that a
// failed_resumable run whose failed node is a human node with a
// previously-answered non-retired interaction on it does NOT re-pause
// on that node — it consumes the recorded answers, seeds the node's
// output, PUBLISHES the human node's declared artifact and advances to
// the next edge.
//
// Reproduces the third facet of #1435: on a `paused_waiting_human`
// resume, recordHumanAnswers ran before startSandbox blew up on the
// docker mount, so the interaction file on disk carries the answers
// but the run flipped to failed_resumable. The current path re-executes
// the paused human node and asks a second time; this test pins the
// fix that reuses the recorded answer instead.
//
// The human node declares a `publish:` so the test also proves the
// materialise/finish path runs — without it a downstream reader of
// `{{artifacts.approval}}` sees nothing (RVA round-1 F1).
//
// Mutation: comment out the call to advancePastAnsweredHumanNodeOnResume
// in resumeFromFailure → this test reddens with ErrRunPaused because
// the human node re-pauses. Alternative mutation: skip the
// materializeHumanArtifact call in the helper → the artifact load
// below reddens.
func TestResumeFromFailure_AdvancesPastAnsweredHumanNode(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "answered_human_advance",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
				Publish:           "approval",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-answered-human-advance"

	// First launch: pauses on the human node.
	eng := New(wf, s, newStubExecutor())
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil || r.Checkpoint.InteractionID == "" {
		t.Fatalf("checkpoint must carry the pause pointer; got %+v", r.Checkpoint)
	}
	interactionID := r.Checkpoint.InteractionID

	// Simulate a first resume that recorded the answers, then failed
	// downstream: write the answered interaction, keep the checkpoint
	// on `gate`, flip the run to failed_resumable. Matches what the
	// sandbox-mount failure leaves behind (#1435 first-attempt state).
	ans := map[string]any{"decision": "approve", "reviewer": "Ada"}
	interaction, err := s.LoadInteraction(ctx, runID, interactionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	now := time.Now().UTC()
	interaction.AnsweredAt = &now
	interaction.Answers = ans
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	// Consume the pause pointer as the real failed first-resume does
	// (claimForResume clears it), and mark the run failed_resumable
	// with the same checkpoint node.
	cp := *r.Checkpoint
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	if err := s.SaveCheckpoint(ctx, runID, &cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &cp, "sandbox start", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	// Second resume: no answers passed — the fix must find the
	// recorded ones and advance to done, NOT re-pause on gate.
	eng2 := New(wf, s, newStubExecutor())
	if err := eng2.Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume: %v (should have advanced past the answered human node)", err)
	}
	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun post-resume: %v", err)
	}
	if got.Status != store.RunStatusFinished {
		t.Fatalf("run status = %s, want finished — the answered human node was NOT reused and the run re-paused", got.Status)
	}
	// F1: the declared artifact must have been published. A resume
	// that seeded outputs but skipped materialise would silently
	// starve any downstream reader of `{{artifacts.approval}}`. The
	// store keys artifacts by NodeID ("gate"), version 0 — the same
	// path resumeFromPause writes.
	artifact, err := s.LoadArtifact(ctx, runID, "gate", 0)
	if err != nil || artifact == nil {
		t.Fatalf("LoadArtifact(gate v0): err=%v artifact=%v — the advance path must publish the human node's declared artifact", err, artifact)
	}
	if v, _ := artifact.Data["decision"].(string); v != "approve" {
		t.Fatalf("artifact.Data[decision]=%v, want %q — the artifact must carry the recorded answers", artifact.Data["decision"], "approve")
	}
}

// TestRecordHumanAnswers_EmptyDoesNotWipe pins the guard added to
// recordHumanAnswers: a resume that ships empty answers (a failed
// resume's second attempt, for instance) must not overwrite the
// answers already recorded on the interaction. Empty means "no new
// answers"; the store's copy stays.
//
// Mutation: remove the len(answers)==0 && len(interaction.Answers)>0
// guard → this test reddens because interaction.Answers becomes nil.
func TestRecordHumanAnswers_EmptyDoesNotWipe(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "answers_preserve",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-answers-preserve"

	eng := New(wf, s, newStubExecutor())
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	// Record answers as a first attempt would.
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	now := time.Now().UTC()
	interaction.AnsweredAt = &now
	interaction.Answers = map[string]any{"reviewer": "Ada"}
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}

	// Call recordHumanAnswers directly with an empty map — the guard
	// must preserve the prior Ada answer.
	if err := eng.recordHumanAnswers(ctx, r, r.Checkpoint, nil); err != nil {
		t.Fatalf("recordHumanAnswers empty: %v", err)
	}
	back, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction post-record: %v", err)
	}
	if v, ok := back.Answers["reviewer"].(string); !ok || v != "Ada" {
		t.Fatalf("interaction.Answers = %v, want the prior Ada answer preserved (empty resume must not wipe)", back.Answers)
	}
}

// TestResumeFromFailure_RefusesStaleAnswersAfterRewind pins the layer-2
// freshness proof #1435 gate finding Rac891d asks for: after a rewind,
// an answered blocking-pause interaction whose AnsweredAt is at or
// before Run.LastRewindAt must NOT be reused — the operator is
// rewinding precisely to re-decide. The retire step in rewind is the
// sibling refusal; this test isolates the timestamp check so a future
// edit dropping the retire cannot silently unblock the class.
//
// Mutation: remove the LastRewindAt guard in
// advancePastAnsweredHumanNodeOnResume → the run finishes instead of
// re-pausing and this test reddens.
func TestResumeFromFailure_RefusesStaleAnswersAfterRewind(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "answered_human_rewind_stale",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
				Publish:           "approval",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-answered-rewind-stale"

	eng := New(wf, s, newStubExecutor())
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	// Record the pre-rewind answers.
	ans := map[string]any{"decision": "approve"}
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	answeredAt := time.Now().UTC()
	interaction.AnsweredAt = &answeredAt
	interaction.Answers = ans
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	// Simulate a rewind AFTER the answer: LastRewindAt > AnsweredAt.
	cp := *r.Checkpoint
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	if err := s.SaveCheckpoint(ctx, runID, &cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &cp, "operator rewound", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
	r2, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun post-fail: %v", err)
	}
	rewoundAt := answeredAt.Add(1 * time.Second)
	r2.LastRewindAt = &rewoundAt
	if err := s.SaveRun(ctx, r2); err != nil {
		t.Fatalf("SaveRun with LastRewindAt: %v", err)
	}

	// Resume: the stale answer sits on-disk, LastRewindAt is AFTER
	// AnsweredAt. The helper MUST refuse to advance — the human node
	// re-pauses and asks the operator again.
	eng2 := New(wf, s, newStubExecutor())
	if err := eng2.Resume(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Resume: want ErrRunPaused (should have re-paused, not reused stale answers), got %v", err)
	}
}

// TestResumeFromFailure_CallerAnswersOverwriteStored pins gate finding
// R60aa7e: a non-empty `--answer` on a failed-run resume must overwrite
// the stored interaction and be consumed on the advance.
//
// Mutation: return `in.Answers` unconditionally from the helper's
// `source` decision → the callerAnswers.decision would never reach the
// artifact and the test reddens on Data["decision"].
func TestResumeFromFailure_CallerAnswersOverwriteStored(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "answered_human_overwrite",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
				Publish:           "approval",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-answered-overwrite"

	eng := New(wf, s, newStubExecutor())
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	// Store a STALE answer the operator wants to correct.
	stale := map[string]any{"decision": "reject"}
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	answeredAt := time.Now().UTC()
	interaction.AnsweredAt = &answeredAt
	interaction.Answers = stale
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	cp := *r.Checkpoint
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	if err := s.SaveCheckpoint(ctx, runID, &cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &cp, "sandbox start", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	// Resume with a CORRECTED answer.
	corrected := map[string]any{"decision": "approve", "note": "corrected"}
	eng2 := New(wf, s, newStubExecutor())
	if err := eng2.Resume(ctx, runID, corrected); err != nil {
		t.Fatalf("Resume with corrected answers: %v", err)
	}
	// The artifact must carry the CORRECTED answer, not the stale one.
	artifact, err := s.LoadArtifact(ctx, runID, "gate", 0)
	if err != nil || artifact == nil {
		t.Fatalf("LoadArtifact: err=%v artifact=%v", err, artifact)
	}
	if v, _ := artifact.Data["decision"].(string); v != "approve" {
		t.Fatalf("artifact.Data[decision]=%v, want %q — the corrected --answer must overwrite the stored one (gate finding R60aa7e)", artifact.Data["decision"], "approve")
	}
	// The stored interaction must also reflect the correction so a
	// second resume with no --answer inherits the new value.
	back, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction post-resume: %v", err)
	}
	if v, _ := back.Answers["decision"].(string); v != "approve" {
		t.Fatalf("interaction.Answers[decision]=%v after corrected resume, want %q", back.Answers["decision"], "approve")
	}
	// The overwrite path must emit human_answers_recorded so the audit
	// trail carries the correction (gate finding Q6 on PR #1490).
	events := readEventTypes(t, s, runID)
	var recorded int
	for _, ev := range events {
		if ev == store.EventHumanAnswersRecorded {
			recorded++
		}
	}
	if recorded < 1 {
		t.Fatalf("expected at least one human_answers_recorded event on the corrected-answers path (got events: %v)", events)
	}
}

// TestGateReplay_ClosesParkedWindow pins the class fix for gate
// finding R62a836 on PR #1490: a failed-resume gate replay must close
// the parked-window boundary AT THE HUMAN NODE so the files the
// operator wrote while parked do NOT land in the next node's rewind
// scope. The replay routes through resumeFromPause UNMODIFIED
// (answeredHumanGateReplay is a predicate; there is no second
// transition to drift), so this test wires a REAL workspace tracker —
// the seam the CLI resume wires (`runview.WorkspaceTrackerFor`) — and
// witnesses the pause path's OWN boundary write: the
// `pre:gate:0` label exists on the tracker after the replay.
//
// Mutation: remove `e.markPreNodeBoundary(rs, humanNodeID)` from
// resumeFromPause → the label is never written → Resolve misses →
// red.
func TestGateReplay_ClosesParkedWindow(t *testing.T) {
	storeRoot := t.TempDir()
	tracker := workspacetrack.NewNative(storeRoot)
	wf := &ir.Workflow{
		Name:  "answered_human_boundary",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
				Publish:           "approval",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-answered-boundary"

	eng := New(wf, s, newStubExecutor(), WithWorkDir(t.TempDir()), WithWorkspaceTracker(tracker))
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	ans := map[string]any{"decision": "approve"}
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	answeredAt := time.Now().UTC()
	interaction.AnsweredAt = &answeredAt
	interaction.Answers = ans
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	cp := *r.Checkpoint
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	if err := s.SaveCheckpoint(ctx, runID, &cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &cp, "sandbox start", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	eng2 := New(wf, s, newStubExecutor(), WithWorkDir(t.TempDir()), WithWorkspaceTracker(tracker))
	if err := eng2.Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume (gate replay): %v", err)
	}
	// The discriminating witness: aliasWorkspacePre on a RESUMED run
	// writes a RESUME-phase label for the node whose parked window is
	// closing (engine_exec.go — "THIS is the boundary that closes the
	// interval it was stopped in"). The launch never writes
	// resume:* labels, so this label exists only if the replay's
	// boundary ran.
	if _, ok := tracker.Resolve(runID, workspacetrack.Label(workspacetrack.PhaseResume, "gate", 0)); !ok {
		labels := tracker.Labels(runID)
		t.Fatalf("gate replay did not close the parked-window boundary — resume:gate:0 missing on the tracker (gate finding R62a836); labels: %v", labels)
	}
}

// readEventTypes reads every event on a run's stream and returns just
// the types, for assertion helpers that care about presence/order.
func readEventTypes(t *testing.T, s store.RunStore, runID string) []store.EventType {
	t.Helper()
	evs, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents(%s): %v", runID, err)
	}
	types := make([]store.EventType, 0, len(evs))
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	return types
}

// TestPausedWaitingHuman_WedgedReplayRecovers pins the RVA-round-3
// HIGH fix: a gate replay that failed between the status flip and the
// claim parks the run paused_waiting_human with the pause pointer
// ALREADY consumed (claimForResume's consumePausePointer ran on the
// first attempt). A plain retry used to die on
// `LoadInteraction(runID, "")` — the dispatch now consults the replay
// predicate on this shape, re-finds the interaction, restores the
// pointer and finishes through the pause path.
//
// Mutation: remove the InteractionID=="" predicate consult from the
// paused_waiting_human dispatch case → Resume errors with "interaction
// ID must not be empty" → red.
func TestPausedWaitingHuman_WedgedReplayRecovers(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "answered_human_wedge",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
				Publish:           "approval",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-answered-wedge"

	eng := New(wf, s, newStubExecutor())
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	ans := map[string]any{"decision": "approve"}
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	answeredAt := time.Now().UTC()
	interaction.AnsweredAt = &answeredAt
	interaction.Answers = ans
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	// The WEDGED shape: status is paused_waiting_human (the flip
	// landed) but the pointer is already consumed (the first claim
	// ran consumePausePointer before the failure).
	cp := *r.Checkpoint
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	if err := s.SaveCheckpoint(ctx, runID, &cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	eng2 := New(wf, s, newStubExecutor())
	if err := eng2.Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume on the wedged shape: %v — the dispatch must consult the replay predicate when the pointer is consumed", err)
	}
	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun post-resume: %v", err)
	}
	if got.Status != store.RunStatusFinished {
		t.Fatalf("run status = %s, want finished — the wedged recovery did not recover", got.Status)
	}
}

// gateReplayFaultStore seals WriteInteraction while armed, parking the
// replay in the window between the status flip and the claim — the
// window the wedge recovery exists for, and the one place the flip's
// error text is observable on the parked run.
type gateReplayFaultStore struct {
	store.RunStore
	failWrites bool
	writes     int
}

func (f *gateReplayFaultStore) WriteInteraction(ctx context.Context, in *store.Interaction) error {
	if f.failWrites {
		f.writes++
		return fmt.Errorf("interaction store sealed for the probe")
	}
	return f.RunStore.WriteInteraction(ctx, in)
}

// TestGateReplay_StatusFlipWritesNoErrorNote pins gate finding Racbc5a
// on PR #1490: the flip into the gate replay must state NO error text.
// UpdateRunStatusIf writes its runErr verbatim into Run.Error on a
// transition while clearing FailureCode, so a note there surfaces in
// inspect/studio as the run's failure message the moment the replay
// dies between the flip and the claim — with the real diagnosis (the
// sandbox-mount refusal of #1435, say) already destroyed. Every other
// non-failure CAS in the tree passes "".
//
// The probe arms a store fault on WriteInteraction so the replay fails
// inside recordHumanAnswers — exactly that window — and asserts the
// parked run carries an EMPTY Error.
//
// Mutation: pass the "human gate replay: …" status note back into the
// flip's UpdateRunStatusIf in replayAnsweredGate → the parked run's
// Error carries it → this test reddens.
func TestGateReplay_StatusFlipWritesNoErrorNote(t *testing.T) {
	fs := &gateReplayFaultStore{RunStore: tmpStore(t)}
	wf := &ir.Workflow{
		Name:  "gate_flip_no_error_note",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
				Publish:           "approval",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	s := fs
	ctx := context.Background()
	const runID = "run-gate-flip-error-note"

	eng := New(wf, s, newStubExecutor())
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	// The #1435 first-attempt shape: answers recorded, pointer consumed,
	// run failed_resumable carrying a REAL diagnosis.
	ans := map[string]any{"decision": "approve"}
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	answeredAt := time.Now().UTC()
	interaction.AnsweredAt = &answeredAt
	interaction.Answers = ans
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	cp := *r.Checkpoint
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	if err := s.SaveCheckpoint(ctx, runID, &cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &cp, "docker mount refused", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	// The replay must get past the flip and die on the sealed write.
	fs.failWrites = true
	eng2 := New(wf, s, newStubExecutor())
	if err := eng2.Resume(ctx, runID, nil); err == nil {
		t.Fatalf("Resume: want the sealed interaction write to fail the replay, got success")
	}
	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun post-failed-replay: %v", err)
	}
	if got.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("run status = %s, want paused_waiting_human (parked in the flip-to-claim window)", got.Status)
	}
	if got.Error != "" {
		t.Fatalf("Run.Error = %q, want empty — the gate-replay flip must not forge a failure message (gate finding Racbc5a)", got.Error)
	}
}

// TestGateReplay_QueuedSkipsReplayUnderNarrowedClaim pins the queued
// branch's gateReplayAllowed guard (the verdict on d0d316a09, first
// "À confirmer"): a durable mission resume narrows its claim CAS to one
// exact source status, and the flip would move the status out from
// under that narrowed claim. The queued case therefore skips the replay
// exactly like the failed_resumable one, and the mission's own claim
// refuses instead — loudly, with the run doc untouched.
//
// Mutation: drop the gateReplayAllowed guard from the queued branch →
// the flip lands (queued → paused_waiting_human) before the narrowed
// claim refuses → the run doc no longer reads queued → red.
func TestGateReplay_QueuedSkipsReplayUnderNarrowedClaim(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "gate_replay_queued_guard",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
				Publish:           "approval",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-gate-replay-queued-guard"

	eng := New(wf, s, newStubExecutor())
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	// The gate-replay shape on a QUEUED run: answers recorded, pointer
	// consumed, run failed_resumable, then the publisher's queued flip.
	ans := map[string]any{"decision": "approve"}
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	answeredAt := time.Now().UTC()
	interaction.AnsweredAt = &answeredAt
	interaction.Answers = ans
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	cp := *r.Checkpoint
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	if err := s.SaveCheckpoint(ctx, runID, &cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &cp, "sandbox start", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, runID, store.RunStatusQueued, ""); err != nil {
		t.Fatalf("flip to queued: %v", err)
	}

	// A durable mission resume expecting failed_resumable: the queued
	// branch must SKIP the replay (no status flip), and the mission's
	// own narrowed claim refuses — loudly.
	eng2 := New(wf, s, newStubExecutor(), WithExpectedResumeStatus(store.RunStatusFailedResumable))
	if err := eng2.Resume(ctx, runID, nil); err == nil {
		t.Fatalf("Resume: want the narrowed claim to refuse the queued run, got success")
	}
	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun post-resume: %v", err)
	}
	if got.Status != store.RunStatusQueued {
		t.Fatalf("run status = %s, want queued — the gate-replay flip must not fire under a narrowed mission claim", got.Status)
	}
}

// TestGateReplay_RefusesAnswersOlderThanRewindEvent pins the
// upgrade-window guard (the verdict on 9878937e1, first "À confirmer"):
// a run rewound by a binary older than the LastRewindAt stamp carries
// neither the timestamp nor a retired interaction, so the run_rewound
// event in the append-only timeline is the only witness. A rewind AFTER
// the answer must refuse the replay — the operator is re-asked instead
// of silently continuing on the decision they rewound to reconsider.
//
// Mutation: remove the rewoundAfterAnswer scan from
// answeredHumanGateReplay → the predicate replays the pre-rewind
// answers and the run finishes → red.
func TestGateReplay_RefusesAnswersOlderThanRewindEvent(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "gate_replay_rewind_event",
		Entry: "gate",
		Nodes: map[string]ir.Node{
			"gate": &ir.HumanNode{
				BaseNode:          ir.BaseNode{ID: "gate"},
				InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
				Publish:           "approval",
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "gate", To: "done"}},
	}
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-gate-replay-rewind-event"

	eng := New(wf, s, newStubExecutor())
	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	// The pre-stamp shape: answers recorded BEFORE the rewind, and no
	// Run.LastRewindAt (the stamp the older binary never wrote).
	ans := map[string]any{"decision": "approve"}
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	answeredAt := time.Now().UTC().Add(-1 * time.Second)
	interaction.AnsweredAt = &answeredAt
	interaction.Answers = ans
	if err := s.WriteInteraction(ctx, interaction); err != nil {
		t.Fatalf("WriteInteraction: %v", err)
	}
	cp := *r.Checkpoint
	cp.InteractionID = ""
	cp.InteractionQuestions = nil
	if err := s.SaveCheckpoint(ctx, runID, &cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &cp, "operator rewound", store.FailureUsageLimitBlocked); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
	// The rewind's only witness on a pre-stamp run: the append-only
	// run_rewound event, timestamped AFTER the answer.
	if _, err := s.AppendEvent(ctx, runID, store.Event{
		Type:      store.EventRunRewound,
		RunID:     runID,
		NodeID:    "gate",
		Timestamp: time.Now().UTC(),
		Data:      map[string]any{"from_node": "gate", "to_node": "gate"},
	}); err != nil {
		t.Fatalf("AppendEvent(run_rewound): %v", err)
	}

	// Resume: the predicate must refuse — the run re-pauses on the gate
	// (fresh ask), it does NOT replay the pre-rewind answers.
	eng2 := New(wf, s, newStubExecutor())
	if err := eng2.Resume(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Resume: want ErrRunPaused (re-ask after a rewind event newer than the answer), got %v", err)
	}
}
