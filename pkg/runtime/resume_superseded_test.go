package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gateReplayWorkflow is a human gate that publishes, then done.
func gateReplayWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "superseded_gate",
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
}

// parkedAtGate runs the gate workflow to its pause and parks the run
// failed_resumable on the gate's checkpoint, its pause pointer consumed —
// answered: the gate's interaction carries a recorded answer, the shape a
// resume replays through the pause path.
func parkedAtGate(t *testing.T, s store.RunStore, runID string, answered bool) {
	t.Helper()
	ctx := context.Background()
	if err := New(gateReplayWorkflow(), s, newStubExecutor()).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if answered {
		interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC()
		interaction.AnsweredAt = &at
		interaction.Answers = map[string]any{"decision": "recorded"}
		if err := s.WriteInteraction(ctx, interaction); err != nil {
			t.Fatal(err)
		}
	}
	cp := *r.Checkpoint
	cp.InteractionID, cp.InteractionQuestions = "", nil
	if err := s.SaveCheckpoint(ctx, runID, &cp); err != nil {
		t.Fatal(err)
	}
	if err := s.FailRunResumable(ctx, runID, &cp, "sandbox start", store.FailureUsageLimitBlocked); err != nil {
		t.Fatal(err)
	}
}

// queueAttempt flips the run to queued from `from` — a publisher's flip —
// and returns a publication time after it.
func queueAttempt(t *testing.T, s store.RunStore, runID string, from store.RunStatus) time.Time {
	t.Helper()
	if ok, err := s.UpdateRunStatusIf(context.Background(), runID, store.RunStatusQueued, "", []store.RunStatus{from}); err != nil || !ok {
		t.Fatalf("queued flip from %s: %v %v", from, ok, err)
	}
	time.Sleep(2 * time.Millisecond)
	published := time.Now().UTC()
	time.Sleep(2 * time.Millisecond)
	return published
}

// requeueOnce cancels the run and queues it again: a newer attempt.
func requeueOnce(t *testing.T, s store.RunStore, runID string) {
	t.Helper()
	ctx := context.Background()
	if ok, err := s.UpdateRunStatusIf(ctx, runID, store.RunStatusCancelled, "operator", []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
		t.Fatalf("cancel: %v %v", ok, err)
	}
	time.Sleep(2 * time.Millisecond)
	if ok, err := s.UpdateRunStatusIf(ctx, runID, store.RunStatusQueued, "", []store.RunStatus{store.RunStatusCancelled}); err != nil || !ok {
		t.Fatalf("queued flip 2: %v %v", ok, err)
	}
}

// assertSuperseded: the delivery's resume returned ErrResumeSuperseded,
// claimed nothing, and left the newer attempt queued, unresumed.
func assertSuperseded(t *testing.T, s store.RunStore, runID string, err error, claimed bool, queuedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if !errors.Is(err, ErrResumeSuperseded) || claimed {
		t.Fatalf("a delivery of an attempt the run was queued past: err=%v claimed=%v, want ErrResumeSuperseded and no claim", err, claimed)
	}
	doc, lerr := s.LoadRun(ctx, runID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if doc.Status != store.RunStatusQueued || doc.QueuedAt == nil || !doc.QueuedAt.Equal(queuedAt) {
		t.Fatalf("the newer attempt: status %s queued_at %v, want queued at %v, untouched", doc.Status, doc.QueuedAt, queuedAt)
	}
	evs, lerr := s.LoadEvents(ctx, runID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, ev := range evs {
		if ev.Type == store.EventRunResumed {
			t.Fatalf("the superseded delivery resumed the run: %v", ev.Data)
		}
	}
}

// TestResume_aSupersededDeliveryTouchesNothing: the delivery of an attempt
// the run was queued past — a cancel, then a newer resume — is refused
// ErrResumeSuperseded before anything of its resume happens, whether it
// would resume a failure or replay an answered gate through its pause. The
// newer attempt's own delivery replays and runs.
func TestResume_aSupersededDeliveryTouchesNothing(t *testing.T) {
	for _, answered := range []bool{false, true} {
		name := map[bool]string{false: "a failure", true: "an answered gate"}[answered]
		t.Run(name, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			runID := "run-superseded-" + map[bool]string{false: "failure", true: "gate"}[answered]
			parkedAtGate(t, s, runID, answered)
			published1 := queueAttempt(t, s, runID, store.RunStatusFailedResumable)
			requeueOnce(t, s, runID)
			doc, err := s.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			queuedAt := *doc.QueuedAt
			claimed := false
			err = New(gateReplayWorkflow(), s, newStubExecutor(), WithQueuedAttempt(published1), WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, runID, nil)
			assertSuperseded(t, s, runID, err, claimed, queuedAt)

			current := time.Now().UTC()
			err = New(gateReplayWorkflow(), s, newStubExecutor(), WithQueuedAttempt(current), WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, runID, map[string]any{"decision": "fresh"})
			// Claimed, then run: through the replayed answer to done, or back
			// to the gate's pause when no answer was recorded.
			if !claimed || (err != nil && !errors.Is(err, ErrRunPaused)) {
				t.Fatalf("the newer attempt's own delivery: err=%v claimed=%v, want it claimed and run", err, claimed)
			}
		})
	}
}

// requeueAfterFirstLoad is a store that queues the run again right after the
// engine's first read of it: a newer attempt lands after the delivery's
// early check, before its move out of queued.
type requeueAfterFirstLoad struct {
	store.RunStore
	t     *testing.T
	runID string
	once  sync.Once
}

func (s *requeueAfterFirstLoad) Unwrap() store.RunStore { return s.RunStore }

func (s *requeueAfterFirstLoad) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	r, err := s.RunStore.LoadRun(ctx, id)
	if err == nil && id == s.runID {
		s.once.Do(func() { requeueOnce(s.t, s.RunStore, id) })
	}
	return r, err
}

// TestResume_aDeliveryOutrunMidResumeIsSuperseded: a newer attempt queued
// after the delivery's early check still wins — the move out of queued is
// the attempt's (the replay's flip back to the pause, the claim), so the
// delivery is refused ErrResumeSuperseded and the newer attempt stays queued.
func TestResume_aDeliveryOutrunMidResumeIsSuperseded(t *testing.T) {
	for _, answered := range []bool{false, true} {
		name := map[bool]string{false: "the claim", true: "the replay's flip"}[answered]
		t.Run(name, func(t *testing.T) {
			base := tmpStore(t)
			ctx := context.Background()
			runID := "run-outrun-" + map[bool]string{false: "claim", true: "replay"}[answered]
			parkedAtGate(t, base, runID, answered)
			published1 := queueAttempt(t, base, runID, store.RunStatusFailedResumable)
			s := &requeueAfterFirstLoad{RunStore: base, t: t, runID: runID}
			claimed := false
			err := New(gateReplayWorkflow(), s, newStubExecutor(), WithQueuedAttempt(published1), WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, runID, nil)
			doc, lerr := base.LoadRun(ctx, runID)
			if lerr != nil || doc.QueuedAt == nil {
				t.Fatalf("reload: %v", lerr)
			}
			assertSuperseded(t, base, runID, err, claimed, *doc.QueuedAt)
		})
	}
}

// TestResume_aSupersededDeliveryRecordsNoAnswer: a run queued from its pause
// keeps its pause pointer, and its resume records the delivery's answers
// before it claims. The delivery of an attempt the run was queued past
// records none: the newer attempt's answers are the ones that count.
func TestResume_aSupersededDeliveryRecordsNoAnswer(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-superseded-pause"
	if err := New(gateReplayWorkflow(), s, newStubExecutor()).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil || r.Checkpoint == nil || r.Checkpoint.InteractionID == "" {
		t.Fatalf("precondition: a pause with its pointer, got %v (%v)", r.Checkpoint, err)
	}
	interactionID := r.Checkpoint.InteractionID
	published1 := queueAttempt(t, s, runID, store.RunStatusPausedWaitingHuman)
	requeueOnce(t, s, runID)
	doc, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	claimed := false
	err = New(gateReplayWorkflow(), s, newStubExecutor(), WithQueuedAttempt(published1), WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, runID, map[string]any{"decision": "stale"})
	assertSuperseded(t, s, runID, err, claimed, *doc.QueuedAt)
	interaction, err := s.LoadInteraction(ctx, runID, interactionID)
	if err != nil {
		t.Fatal(err)
	}
	if interaction.AnsweredAt != nil || interaction.Answers["decision"] != nil {
		t.Fatalf("the superseded delivery recorded its answers on the gate: %v", interaction.Answers)
	}
}

// TestResume_aSupersededDeliveryIsSupersededFirst: a delivery the run was
// queued past is refused as superseded before any other refusal it would
// meet — here the scratch its last teardown could not bank. Refused for the
// scratch instead, it would read as a verdict on the newer attempt's run.
func TestResume_aSupersededDeliveryIsSupersededFirst(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-superseded-first"
	parkedAtGate(t, s, runID, false)
	if _, err := s.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
		"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
	}}); err != nil {
		t.Fatal(err)
	}
	published1 := queueAttempt(t, s, runID, store.RunStatusFailedResumable)
	requeueOnce(t, s, runID)
	doc, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	claimed := false
	err = New(gateReplayWorkflow(), s, newStubExecutor(), WithQueuedAttempt(published1), WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, runID, nil)
	assertSuperseded(t, s, runID, err, claimed, *doc.QueuedAt)
}
