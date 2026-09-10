package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Async human interaction (ADR-081): the await_answers node is the
// deterministic sync point for questions posted via ask_user_async.
// These tests drive it against the real engine + store — the questions
// are seeded/answered directly through the store (the same records the
// executor's StoreAsyncAskBinder writes), no LLM.

// seedAsyncInteraction writes a pending Kind=async interaction the way
// ask_user_async's binder does.
func seedAsyncInteraction(t *testing.T, s store.RunStore, runID, nodeID, id, question string) {
	t.Helper()
	err := s.WriteInteraction(context.Background(), &store.Interaction{
		ID:          id,
		RunID:       runID,
		NodeID:      nodeID,
		Kind:        store.InteractionKindAsync,
		RequestedAt: time.Now().UTC(),
		Questions:   map[string]any{delegate.AskUserQuestionKey: question},
	})
	if err != nil {
		t.Fatalf("seed async interaction: %v", err)
	}
}

// TestAwaitAnswersAlreadyAnswered: every question answered before the
// await node runs → it passes immediately (level-triggered predicate,
// no park).
func TestAwaitAnswersAlreadyAnswered(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "async_await_mini.bot")
	s := tmpStore(t)
	runID := "e2e-async-answered"

	seedAsyncInteraction(t, s, runID, "asker", runID+"_asker_async_1", "which color?")
	if _, err := store.AnswerInteraction(context.Background(), s, runID, runID+"_asker_async_1", map[string]any{delegate.AskUserQuestionKey: "blue"}); err != nil {
		t.Fatalf("answer interaction: %v", err)
	}

	eng := newEngine(t, wf, s, newScenarioExecutor())
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
}

// TestAwaitAnswersReleasedByAnswer: the run starts with a PENDING async
// question — the await branch parks while the sibling branch completes;
// answering the question (+ ringing the doorbell, as
// AnswerInteractionCtx / the CLI do) releases the branch and the run
// converges. Also proves branch isolation: the sibling compute finished
// while the question was still pending.
// Serial: its ceiling is read against the await_answers level-poll's DEFAULT
// cadence, and TestAwaitAnswersDoorbellNeverMissed retunes that cadence
// through a pkg/runtime package global — in parallel the two contradict each
// other (both rows failed together at -parallel 32).
func TestAwaitAnswersReleasedByAnswer(t *testing.T) {
	wf := compileFixture(t, "async_await_mini.bot")
	s := tmpStore(t)
	runID := "e2e-async-released"
	iid := runID + "_asker_async_1"

	seedAsyncInteraction(t, s, runID, "asker", iid, "ship it?")

	eng := newEngine(t, wf, s, newScenarioExecutor())

	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background(), runID, nil) }()

	// Give the run time to park the gate branch, then answer + ring.
	time.Sleep(300 * time.Millisecond)
	if _, err := store.AnswerInteraction(context.Background(), s, runID, iid, map[string]any{delegate.AskUserQuestionKey: "yes"}); err != nil {
		t.Fatalf("answer interaction: %v", err)
	}
	eng.NotifyInteractionAnswered()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	// Ceiling ABOVE the await_answers 5s level-poll: if the doorbell rings
	// before the gate branch parks (the 300ms sleep lost the race — seen
	// under -race on CI), the level-triggered poll still releases the
	// branch; only a miss of BOTH mechanisms is a real failure.
	case <-time.After(15 * time.Second):
		t.Fatal("run did not converge after the answer (neither doorbell nor level-poll released the gate)")
	}

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
	// The sibling branch's compute must have produced its output — it was
	// never blocked by the pending question.
	art, err := s.LoadLatestArtifact(context.Background(), runID, "gather")
	if err != nil {
		t.Fatalf("load gather artifact: %v", err)
	}
	if got := toInt(art.Data["n"]); got != 42 {
		t.Errorf("gather.n = %d, want 42", got)
	}
}

// TestAwaitAnswersDoorbellNeverMissed asserts the in-process doorbell
// is a RELIABLE release mechanism (arm-before-check invariant) — not a
// happy-path best-effort layered over the level-poll. The fixture pins
// a large poll (via awaitAnswersPollInterval override) AND a large node
// timeout so ONLY the doorbell can release the parked gate branch in
// the test's 3s ceiling: if a ring landed in the [check … arm] gap and
// were dropped, the test would deadline instead of converging. Repeat
// the launch/answer cycle many times to stress every possible
// interleaving of the ring with the branch scheduler.
// doorbellFallbackPoll is where this row parks the await_answers level-poll,
// and doorbellFloor is the earliest a poll-released run could appear (one
// poll period minus the slack a loaded scheduler adds to a timer). Between
// "converged" and doorbellFloor there is no mechanism but the doorbell, which
// is what makes the assertion a statement about the product rather than about
// the machine.
const (
	doorbellFallbackPoll = 60 * time.Second
	doorbellFloor        = 50 * time.Second
)

// Serial: it retunes the await_answers level-poll through a pkg/runtime
// package global, which its sibling rows read at its default value.
func TestAwaitAnswersDoorbellNeverMissed(t *testing.T) {
	// Push the fallback poll far out, so a run that converges QUICKLY can
	// only have been released by the doorbell. The assertion below reads
	// that gap rather than a hand-picked ceiling.
	prev := runtime.SetAwaitAnswersPollInterval(doorbellFallbackPoll)
	t.Cleanup(func() { runtime.SetAwaitAnswersPollInterval(prev) })

	wf := compileFixture(t, "async_await_mini.bot")

	// Many iterations: each one is a fresh engine + fresh store + fresh
	// run, exercising the arm-check-select window against the ring in
	// every scheduler interleaving. A missed ring on ANY iteration
	// hangs that iteration and trips the ceiling.
	const iters = 10
	for i := 0; i < iters; i++ {
		s := tmpStore(t)
		runID := "e2e-async-doorbell"
		iid := runID + "_asker_async_1"
		seedAsyncInteraction(t, s, runID, "asker", iid, "ship it?")

		eng := newEngine(t, wf, s, newScenarioExecutor())
		done := make(chan error, 1)
		go func() { done <- eng.Run(context.Background(), runID, nil) }()

		// Small sleep so the gate branch has a chance to reach the arm
		// (best case exercises the doorbell wake). No sleep would still
		// pass — the pre-arm answer is caught by the store check — but
		// this is the interleaving the observed slow-pod failure hits.
		time.Sleep(50 * time.Millisecond)
		if _, err := store.AnswerInteraction(context.Background(), s, runID, iid, map[string]any{delegate.AskUserQuestionKey: "yes"}); err != nil {
			t.Fatalf("iter %d: answer interaction: %v", i, err)
		}
		eng.NotifyInteractionAnswered()

		// What this row asserts is a MECHANISM, so it measures the
		// mechanism instead of racing a hand-picked ceiling. The engine
		// lifecycle around the wake costs seconds on a loaded runner, and a
		// 15s window it could reach turned a load spike into "doorbell
		// missed" — a merge-queue ejector that named the wrong culprit.
		// The gap is what discriminates: the fallback poll and the node
		// timeout are both far out, so a run that converges before
		// doorbellFloor can only have been released by the doorbell, and
		// one that converges at the poll proves the doorbell WAS missed.
		started := time.Now()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iter %d: run: %v", i, err)
			}
			if elapsed := time.Since(started); elapsed >= doorbellFloor {
				t.Fatalf("iter %d: converged after %s, at or past the %s fallback poll — the doorbell was MISSED and the level-poll released the branch; this is the product invariant, not a slow machine",
					i, elapsed.Round(time.Millisecond), doorbellFallbackPoll)
			} else if elapsed > 15*time.Second {
				t.Logf("iter %d: converged in %s — released by the doorbell, but the lifecycle around it is slow", i, elapsed.Round(time.Millisecond))
			}
		case <-time.After(waitBudget(t, doorbellFallbackPoll+30*time.Second)):
			t.Fatalf("iter %d: run never converged, not even at the %s fallback poll — the branch is parked for good", i, doorbellFallbackPoll)
		}

		r, err := s.LoadRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("iter %d: load run: %v", i, err)
		}
		if r.Status != store.RunStatusFinished {
			t.Fatalf("iter %d: status = %s, want finished", i, r.Status)
		}
	}
}

// TestAwaitAnswersTimeout: an unanswered question bounds the wait at
// the node's mandatory timeout and fails the run with an explicit
// timeout error naming the pending interaction.
func TestAwaitAnswersTimeout(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "async_await_timeout.bot")
	s := tmpStore(t)
	runID := "e2e-async-timeout"
	iid := runID + "_asker_async_1"

	seedAsyncInteraction(t, s, runID, "asker", iid, "never answered")

	eng := newEngine(t, wf, s, newScenarioExecutor())
	err := eng.Run(context.Background(), runID, nil)
	if err == nil {
		t.Fatal("run succeeded, want timeout failure")
	}
	if !strings.Contains(err.Error(), "unanswered") {
		t.Errorf("error = %v, want a message naming the unanswered question(s)", err)
	}

	r, lerr := s.LoadRun(context.Background(), runID)
	if lerr != nil {
		t.Fatalf("load run: %v", lerr)
	}
	if r.Status == store.RunStatusFinished {
		t.Fatalf("status = %s, want a failure status", r.Status)
	}
}

// TestAwaitAnswersNoQuestions: an await_answers node with nothing
// pending (no questions were ever posted) passes straight through — a
// workflow with an unconditional sync point does not hang when the
// agent had nothing to ask.
func TestAwaitAnswersNoQuestions(t *testing.T) {
	t.Parallel()
	wf := compileFixture(t, "async_await_mini.bot")
	s := tmpStore(t)
	runID := "e2e-async-noq"

	eng := newEngine(t, wf, s, newScenarioExecutor())
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
}
