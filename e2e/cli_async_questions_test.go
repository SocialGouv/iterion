package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The operator surface of ADR-081's async questions:
//
//	iterion runs questions <run-id>
//	iterion runs answer <run-id> <interaction-id> "<answer>"
//
// Both are cross-process by design — the CLI only touches the run store,
// and a live engine picks the answer up through the store. These tests
// drive the real commands against a real store and assert the effect on
// the SYSTEM: the answer is recorded, a node-scoped delivery is queued,
// and a parked await_answers gate releases so the run converges.

// jsonPrinter returns a Printer capturing machine output into buf.
func jsonPrinter(buf *bytes.Buffer) *cli.Printer {
	return &cli.Printer{W: buf, Format: cli.OutputJSON}
}

// waitForRun polls until the run record exists (the engine writes it at
// Run() start) or the deadline expires. Polling the store — not a fixed
// sleep — is the synchronization: `iterion runs answer` loads the run,
// so it must not race the engine's first write.
func waitForRun(t *testing.T, s store.RunStore, runID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := s.LoadRun(context.Background(), runID); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s never appeared in the store", runID)
}

// TestRunsQuestionsThenAnswerReleasesAwaitGate is the end-to-end contract
// of the two commands: while a run is parked on an unanswered async
// question, `runs questions` must SHOW that question, and `runs answer`
// must make the run converge.
//
// Mutation check: cut any wire and this fails. If RunQuestions stopped
// reading the pending list, the listing assertion fails. If RunAnswer
// stopped recording the answer, the gate never releases and the run
// times out. If it recorded the answer but stopped queueing the
// node-scoped delivery, the queued-message assertion fails (that
// delivery is what carries the reply back to the asking agent).
func TestRunsQuestionsThenAnswerReleasesAwaitGate(t *testing.T) {
	storeDir := t.TempDir()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	wf := compileFixture(t, "async_await_cli.bot")

	runID := "e2e-cli-async-release"
	iid := runID + "_asker_async_1"
	seedAsyncInteraction(t, s, runID, "asker", iid, "ship it?")

	eng := runtime.New(wf, s, newScenarioExecutor())
	done := make(chan error, 1)
	go func() { done <- eng.Run(context.Background(), runID, nil) }()
	waitForRun(t, s, runID)

	// --- iterion runs questions -------------------------------------
	var qbuf bytes.Buffer
	if err := cli.RunQuestions(cli.QuestionsOptions{StoreDir: storeDir, RunID: runID}, jsonPrinter(&qbuf)); err != nil {
		t.Fatalf("RunQuestions: %v", err)
	}
	var listed struct {
		Interactions []store.Interaction `json:"interactions"`
	}
	if err := json.Unmarshal(qbuf.Bytes(), &listed); err != nil {
		t.Fatalf("decode questions output %q: %v", qbuf.String(), err)
	}
	if len(listed.Interactions) != 1 {
		t.Fatalf("listed %d pending questions, want exactly the one that is blocking the run: %s", len(listed.Interactions), qbuf.String())
	}
	if got := listed.Interactions[0].ID; got != iid {
		t.Errorf("listed interaction id = %q, want %q", got, iid)
	}
	if got := listed.Interactions[0].NodeID; got != "asker" {
		t.Errorf("listed interaction node = %q, want asker", got)
	}
	if !strings.Contains(qbuf.String(), "ship it?") {
		t.Errorf("questions output does not carry the question text: %s", qbuf.String())
	}

	// --- iterion runs answer ----------------------------------------
	var abuf bytes.Buffer
	if err := cli.RunAnswer(cli.AnswerOptions{
		StoreDir:      storeDir,
		RunID:         runID,
		InteractionID: iid,
		Answer:        "yes — ship it",
	}, jsonPrinter(&abuf)); err != nil {
		t.Fatalf("RunAnswer: %v", err)
	}

	// The answer is recorded on the interaction itself.
	in, err := s.LoadInteraction(context.Background(), runID, iid)
	if err != nil {
		t.Fatalf("load interaction: %v", err)
	}
	if in.AnsweredAt == nil {
		t.Fatalf("interaction %s still has no AnsweredAt after `runs answer`", iid)
	}

	// ... and a node-scoped delivery is queued for the asking node, so
	// the agent receives the reply at its next turn boundary.
	msgs, err := s.ListQueuedMessages(context.Background(), runID)
	if err != nil {
		t.Fatalf("list queued messages: %v", err)
	}
	var delivered *store.QueuedUserMessage
	for i := range msgs {
		if msgs[i].InteractionID == iid {
			delivered = &msgs[i]
			break
		}
	}
	if delivered == nil {
		t.Fatalf("no queued delivery for interaction %s (queued: %+v)", iid, msgs)
	}
	if delivered.NodeID != "asker" {
		t.Errorf("queued delivery NodeID = %q, want asker (a message that is not node-scoped can leak into the next node)", delivered.NodeID)
	}
	if !strings.Contains(delivered.Text, "yes — ship it") {
		t.Errorf("queued delivery text = %q, want it to carry the operator's answer", delivered.Text)
	}

	// --- the run converges ------------------------------------------
	// No in-process doorbell is rung: the CLI is cross-process, so the
	// gate must release on its own level-poll of the interaction store.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("run did not converge after `iterion runs answer` — the gate never saw the answer")
	}

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
}

// TestRunsQuestionsListsNothingWhenAnswered: once answered, the question
// drops off the pending list — the operator's "what still needs me?"
// view must not keep showing settled questions.
func TestRunsQuestionsListsNothingWhenAnswered(t *testing.T) {
	storeDir := t.TempDir()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	runID := "e2e-cli-async-settled"
	iid := runID + "_asker_async_1"
	seedAsyncInteraction(t, s, runID, "asker", iid, "which color?")
	if err := s.SaveRun(context.Background(), &store.Run{ID: runID, WorkflowName: "async_await_cli", Status: store.RunStatusRunning}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	var before bytes.Buffer
	if err := cli.RunQuestions(cli.QuestionsOptions{StoreDir: storeDir, RunID: runID}, jsonPrinter(&before)); err != nil {
		t.Fatalf("RunQuestions (before): %v", err)
	}
	if !strings.Contains(before.String(), iid) {
		t.Fatalf("pending question %s not listed before answering: %s", iid, before.String())
	}

	if err := cli.RunAnswer(cli.AnswerOptions{StoreDir: storeDir, RunID: runID, InteractionID: iid, Answer: "blue"}, jsonPrinter(&bytes.Buffer{})); err != nil {
		t.Fatalf("RunAnswer: %v", err)
	}

	var after bytes.Buffer
	if err := cli.RunQuestions(cli.QuestionsOptions{StoreDir: storeDir, RunID: runID}, jsonPrinter(&after)); err != nil {
		t.Fatalf("RunQuestions (after): %v", err)
	}
	if strings.Contains(after.String(), iid) {
		t.Errorf("answered question %s still listed as pending: %s", iid, after.String())
	}
}

// TestRunsAnswerRejectsBadInput covers the guards the command is
// specified to enforce — an operator typo must produce a clear error,
// never a silently dropped answer.
func TestRunsAnswerRejectsBadInput(t *testing.T) {
	storeDir := t.TempDir()
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	runID := "e2e-cli-async-guards"
	iid := runID + "_asker_async_1"
	seedAsyncInteraction(t, s, runID, "asker", iid, "ok?")

	// A blocking (non-async) interaction: answering it here must be
	// refused and point at `iterion resume --answer`.
	blockingID := runID + "_gate_pause"
	if err := s.WriteInteraction(context.Background(), &store.Interaction{
		ID:          blockingID,
		RunID:       runID,
		NodeID:      "gate",
		RequestedAt: time.Now().UTC(),
		Questions:   map[string]any{"approve": "?"},
	}); err != nil {
		t.Fatalf("seed blocking interaction: %v", err)
	}

	cases := []struct {
		name          string
		interactionID string
		answer        string
		wantContains  string
	}{
		{"unknown interaction", "no-such-id", "yes", "not found"},
		{"empty answer", iid, "", "answer text is required"},
		{"blocking interaction", blockingID, "yes", "resume --answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cli.RunAnswer(cli.AnswerOptions{
				StoreDir:      storeDir,
				RunID:         runID,
				InteractionID: tc.interactionID,
				Answer:        tc.answer,
			}, jsonPrinter(&bytes.Buffer{}))
			if err == nil {
				t.Fatalf("RunAnswer accepted %s, want an error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantContains)
			}
		})
	}

	// The real question must still be pending — a rejected call may not
	// have half-answered anything.
	in, err := s.LoadInteraction(context.Background(), runID, iid)
	if err != nil {
		t.Fatalf("load interaction: %v", err)
	}
	if in.AnsweredAt != nil {
		t.Errorf("interaction %s was answered by a rejected call", iid)
	}
}
