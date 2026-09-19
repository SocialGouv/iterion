package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

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
}
