// E2E coverage for the whats-next v2 conversational bot + the board →
// dispatcher smoke loop.
//
// whats-next v2 is ONE agent (`nexie`) in a chat loop (seed → nexie ⇄
// chat, gate compute, explicit-close exit). The tests here:
//   - pin the v2 graph contract statically (worktree: none, the
//     LOAD-BEARING `_session_id` mapping on the loop edge, interaction
//     enabled on nexie) — TestWhatsNextV2_GraphContract;
//   - drive the chat loop with the stub executor: pause at chat, resume
//     with an operator message, assert the second turn receives BOTH the
//     message and the prior turn's session id, then explicit-close —
//     TestWhatsNextV2_ChatLoop_PauseResumeClose;
//   - drive a mid-turn ask_user pause with structured options through
//     the engine (pause envelope persisted, resume re-invokes the same
//     node with the answer) — TestWhatsNextV2_AskUserOptions_PauseResume.
//
// Board/dispatcher regression guards (bot-agnostic, kept from v1):
//   - commit 45eafe28 — dispatcher MUST auto-transition in_progress →
//     review on a clean run finish (otherwise the issue stays eligible
//     and gets re-dispatched on the next tick)
//     (TestWhatsNext_Loop_DispatchAutoTransitionsNoReloop).
//   - commit c134af2e — findings moved from PROJECT_MEMORY_DIR/findings/
//     *.md files into board issues in a non-eligible `inbox` state. That
//     change dropped the old .md survival guard; this file re-adds it in
//     board form — a dispatched bot's inbox finding survives the dispatch
//     lifecycle and is itself never dispatched
//     (TestWhatsNext_Loop_FindingsInboxSurvivesDispatch).

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestWhatsNextV2_GraphContract pins the v2 shape statically:
//   - `worktree: none` — Nexie mutates the board + memory only; the v1
//     default (auto) produced phantom storage branches and aimed
//     workspace_dir at a tree without .iterion.
//   - the chat → nexie loop edge MUST map `_session_id` from
//     outputs.nexie — session continuity resolves ONLY from the input
//     map, so dropping the mapping silently degrades every turn to an
//     amnesiac one-shot (the v1 failure mode).
//   - nexie keeps `interaction` enabled (ask_user) and an inheriting
//     session mode.
func TestWhatsNextV2_GraphContract(t *testing.T) {
	wf := compileFixture(t, "whats-next/main.bot")

	if wf.Worktree != "none" {
		t.Errorf("workflow worktree = %q, want \"none\" (Nexie must not run in a git worktree)", wf.Worktree)
	}

	nexie, ok := wf.Nodes["nexie"].(*ir.AgentNode)
	if !ok {
		t.Fatal("nexie agent node missing from whats-next/main.bot")
	}
	if nexie.Interaction != ir.InteractionHuman {
		t.Errorf("nexie interaction = %v, want human (ask_user must be armed)", nexie.Interaction)
	}
	if nexie.Session != ir.SessionInheritIfAvailable {
		t.Errorf("nexie session = %v, want inherit_if_available", nexie.Session)
	}

	var loopEdge *ir.Edge
	for _, e := range wf.Edges {
		if e.From == "chat" && e.To == "nexie" {
			loopEdge = e
			break
		}
	}
	if loopEdge == nil {
		t.Fatal("chat -> nexie loop edge missing")
	}
	if loopEdge.LoopName == "" {
		t.Error("chat -> nexie edge must carry a loop tag (bounded conversation)")
	}
	mappings := make(map[string]string, len(loopEdge.With))
	for _, m := range loopEdge.With {
		mappings[m.Key] = m.Raw
	}
	sess, ok := mappings["_session_id"]
	if !ok {
		t.Fatal("chat -> nexie edge lost the _session_id mapping — session continuity silently degrades to amnesiac one-shot turns")
	}
	if want := "{{outputs.nexie._session_id}}"; sess != want {
		t.Errorf("_session_id mapping = %q, want %q", sess, want)
	}
	if _, ok := mappings["operator_message"]; !ok {
		t.Error("chat -> nexie edge must map operator_message from the chat answer")
	}
}

// TestWhatsNextV2_ChatLoop_PauseResumeClose drives the conversation
// loop end-to-end with the stub executor: turn 1 pauses at chat; the
// operator's answer re-invokes nexie WITH the message and the prior
// turn's session id; turn 2 closes explicitly and the run finishes.
func TestWhatsNextV2_ChatLoop_PauseResumeClose(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whats-next/main.bot")
	exec := newScenarioExecutor()

	var secondTurnInput map[string]any
	exec.on("nexie", func(input map[string]any) (map[string]any, error) {
		turn := exec.callCount("nexie")
		if turn == 1 {
			return map[string]any{
				"reply":          "Board: 3 tickets. Je recommande `fix-doctor` (quick win).",
				"close":          false,
				"quick_replies":  []any{"Dispatche-le"},
				"dispatched_ids": []any{},
				// The real delegate stamps these; the loop edge maps them
				// back into turn 2's input.
				"_session_id":          "sess-nexie-1",
				"_session_fingerprint": "fp-anthropic",
			}, nil
		}
		secondTurnInput = input
		return map[string]any{
			"reply":          "Session archivée.",
			"close":          true,
			"quick_replies":  []any{},
			"dispatched_ids": []any{},
			"_session_id":    "sess-nexie-1",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-nexie-chat", nil)
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected ErrRunPaused at chat, got: %v", err)
	}
	run, _ := s.LoadRun(context.Background(), "e2e-nexie-chat")
	if run.Checkpoint == nil || run.Checkpoint.NodeID != "chat" {
		t.Fatalf("checkpoint node = %v, want chat", run.Checkpoint)
	}
	// Nexie's reply must ride the pause: the chat node's input IS the
	// questions payload the studio renders.
	if got := fmt.Sprint(run.Checkpoint.InteractionQuestions["reply"]); got == "" || got == "<nil>" {
		t.Errorf("chat pause lost Nexie's reply: questions=%v", run.Checkpoint.InteractionQuestions)
	}

	// Operator answers → loop re-invokes nexie with message + session id.
	err = eng.Resume(context.Background(), "e2e-nexie-chat", map[string]any{
		"message": "ok, ferme la session",
	})
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}

	if got := exec.callCount("nexie"); got != 2 {
		t.Fatalf("nexie called %d times, want 2", got)
	}
	if secondTurnInput == nil {
		t.Fatal("second nexie turn input not captured")
	}
	if got := secondTurnInput["operator_message"]; got != "ok, ferme la session" {
		t.Errorf("turn 2 operator_message = %v, want the chat answer", got)
	}
	if got := secondTurnInput["_session_id"]; got != "sess-nexie-1" {
		t.Errorf("turn 2 _session_id = %v, want sess-nexie-1 (conversation continuity broken)", got)
	}

	run, _ = s.LoadRun(context.Background(), "e2e-nexie-chat")
	if run.Status != store.RunStatusFinished {
		t.Errorf("status after explicit close = %s, want finished", run.Status)
	}
}

// TestWhatsNextV2_AskUserOptions_PauseResume drives a mid-turn ask_user
// pause with structured options through the engine: the executor
// signals ErrNeedsInteraction (as the claude_code/claw backends do when
// the LLM calls ask_user), the run pauses with the options envelope
// persisted, and the resume re-invokes the SAME node with the picked
// option riding the prior-interaction keys.
func TestWhatsNextV2_AskUserOptions_PauseResume(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whats-next/main.bot")
	exec := newScenarioExecutor()

	questions := map[string]any{
		delegate.AskUserQuestionKey: "Close these 4 stale tickets?",
	}
	delegate.AddAskUserOptionKeys(questions, []delegate.AskUserOption{
		{ID: "yes", Label: "Close all 4"},
		{ID: "no", Label: "Keep them"},
	}, false)

	var resumedInput map[string]any
	exec.on("nexie", func(input map[string]any) (map[string]any, error) {
		if exec.callCount("nexie") == 1 {
			return nil, &model.ErrNeedsInteraction{
				NodeID:    "nexie",
				Questions: questions,
				SessionID: "sess-ask-1",
				// Non-empty Backend marks this as a delegate pause so the
				// resume path re-invokes the node (reInvokeBackend) instead
				// of treating the answers as the node's output.
				Backend: "stub",
			}
		}
		resumedInput = input
		return map[string]any{
			"reply":          "Fermé les 4 tickets périmés.",
			"close":          true,
			"quick_replies":  []any{},
			"dispatched_ids": []any{},
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)

	err := eng.Run(context.Background(), "e2e-nexie-askuser", nil)
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected ErrRunPaused at ask_user, got: %v", err)
	}
	run, _ := s.LoadRun(context.Background(), "e2e-nexie-askuser")
	if run.Checkpoint == nil || run.Checkpoint.NodeID != "nexie" {
		t.Fatalf("checkpoint node = %v, want nexie (mid-turn pause)", run.Checkpoint)
	}
	// The structured-options presentation keys must survive persistence
	// verbatim — the studio detects them to render clickable choices.
	opts, ok := run.Checkpoint.InteractionQuestions[delegate.AskUserOptionsKey].([]any)
	if !ok || len(opts) != 2 {
		t.Fatalf("options envelope lost on pause: %v", run.Checkpoint.InteractionQuestions)
	}
	if run.Checkpoint.BackendSessionID != "sess-ask-1" {
		t.Errorf("BackendSessionID = %q, want sess-ask-1 (same-session resume anchor)", run.Checkpoint.BackendSessionID)
	}

	// Operator clicks "Close all 4" → answer is the option id.
	err = eng.Resume(context.Background(), "e2e-nexie-askuser", map[string]any{
		delegate.AskUserQuestionKey: "yes",
	})
	if err != nil {
		t.Fatalf("resume error: %v", err)
	}
	if got := exec.callCount("nexie"); got != 2 {
		t.Fatalf("nexie called %d times, want 2 (re-invoked after answer)", got)
	}
	if resumedInput == nil {
		t.Fatal("resumed nexie input not captured")
	}
	if got := resumedInput[delegate.PriorAskUserAnswerKey]; got != "yes" {
		t.Errorf("resumed input %s = %v, want \"yes\"", delegate.PriorAskUserAnswerKey, got)
	}

	run, _ = s.LoadRun(context.Background(), "e2e-nexie-askuser")
	if run.Status != store.RunStatusFinished {
		t.Errorf("status after resume+close = %s, want finished", run.Status)
	}
}

// newSmokeDispatcherFixture wires a dispatcher + native store +
// StubRunner the same way newDispatcherFixture does, but routes the
// Config through ApplyDefaults() so CompletedState defaults to "review"
// (matches the production path post-45eafe28). Kept as a sibling helper
// rather than modifying newDispatcherFixture to avoid changing the
// semantics other dispatcher_test.go cases rely on.
func newSmokeDispatcherFixture(t *testing.T, polling time.Duration) (
	*dispatcher.Dispatcher,
	*native.Store,
	*dispatcher.StubRunner,
	func(),
) {
	t.Helper()
	dir := t.TempDir()

	ns, err := native.NewStore(dir + "/dispatcher")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ws, err := dispatcher.NewWorkspaces(dir + "/dispatcher/workspaces")
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}

	cfg := &dispatcher.Config{
		Name:      "e2e-smoke-loop",
		Workflow:  dir + "/dummy.bot",
		Tracker:   dispatcher.TrackerConfig{Kind: "native"},
		Polling:   dispatcher.PollingConfig{IntervalMS: int(polling.Milliseconds())},
		Agent:     dispatcher.AgentConfig{MaxConcurrent: 2, MaxRetryBackoffMS: 500},
		Workspace: dispatcher.WorkspaceConfig{Root: dir + "/dispatcher/workspaces"},
	}
	cfg.ApplyDefaults()

	logger := iterlog.New(iterlog.LevelError, &bytes.Buffer{})
	runner := &dispatcher.StubRunner{}
	c, err := dispatcher.New(dispatcher.Options{
		Config:     cfg,
		Tracker:    native.NewAdapter(ns),
		Runner:     runner,
		Workspaces: ws,
		Logger:     logger,
		StoreDir:   dir,
		HostMarker: "e2e-smoke",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	return c, ns, runner, func() { cancel(); c.Stop() }
}

// TestWhatsNext_Loop_DispatchAutoTransitionsNoReloop drives the
// production board → dispatcher loop end-to-end with stubs:
//
//  1. Boot a dispatcher with ApplyDefaults() so CompletedState=review
//     mirrors production.
//  2. Create two ready issues via boardops (matches the production
//     path emit_action takes — boardops.Call create_issue per surviving
//     roadmap item).
//  3. StubRunner clean-finishes each dispatch; the actor must
//     auto-transition the issue in_progress → review (guard 45eafe28).
//  4. Wait several polling intervals and assert the dispatch counter
//     stays at 2 — without 45eafe28 the issues would remain in
//     in_progress + eligible and the actor would re-dispatch them.
//
// Wall-clock budget: dispatch loop finishes in ~3× polling (claim →
// finish → transition), then 5× polling for the no-reloop watch.
// At 50ms polling that's ~400ms; the deadline is 3s for slow CI.
func TestWhatsNext_Loop_DispatchAutoTransitionsNoReloop(t *testing.T) {
	const polling = 50 * time.Millisecond
	c, ns, runner, cleanup := newSmokeDispatcherFixture(t, polling)
	defer cleanup()

	var dispatchCount atomic.Int32
	runner.Handler = func(_ context.Context, _ dispatcher.DispatchSpec) error {
		dispatchCount.Add(1)
		return nil
	}

	caps := boardops.NewCapabilities("board.create,board.read,board.move,board.assign")
	mkIssue := func(title string) native.Issue {
		raw, err := boardops.Call(ns, caps, "create_issue", json.RawMessage(`{"title":"`+title+`","state":"ready","assignee":"feature_dev"}`))
		if err != nil {
			t.Fatalf("create_issue %q: %v", title, err)
		}
		var iss native.Issue
		if err := json.Unmarshal(raw, &iss); err != nil {
			t.Fatalf("unmarshal %q: %v", title, err)
		}
		return iss
	}
	issX := mkIssue("Refactor X")
	issY := mkIssue("Implement Y")

	// Wait for both issues to reach review state.
	inReview := func(id string) bool {
		iss, _ := ns.Get(id)
		return iss != nil && iss.State == native.StateReview
	}
	waitUntil(t, 10*time.Second, "both issues to auto-transition to review (guard 45eafe28)",
		func() bool { return inReview(issX.ID) && inReview(issY.ID) },
		func() string {
			x, _ := ns.Get(issX.ID)
			y, _ := ns.Get(issY.ID)
			return fmt.Sprintf("%q state=%v, %q state=%v",
				issX.Title, stateOf(x), issY.Title, stateOf(y))
		})
	if got := dispatchCount.Load(); got != 2 {
		t.Fatalf("expected exactly 2 dispatches before transition, got %d", got)
	}

	// No-reloop guard: dispatcher must not re-dispatch issues sitting
	// in review. Wait several polling intervals; counter must stay at 2.
	time.Sleep(5 * polling)
	if got := dispatchCount.Load(); got != 2 {
		t.Fatalf("re-dispatch detected after review transition: dispatchCount=%d (expected 2) — regression of 45eafe28", got)
	}
	if running := len(c.Snapshot().Running); running != 0 {
		t.Fatalf("running set not drained after clean finish: %d still running", running)
	}
}

func stateOf(iss *native.Issue) string {
	if iss == nil {
		return "<nil>"
	}
	return iss.State
}

// titlesOf collects issue titles for readable failure messages.
func titlesOf(issues []*native.Issue) []string {
	out := make([]string, 0, len(issues))
	for _, iss := range issues {
		out = append(out, iss.Title)
	}
	return out
}

// TestWhatsNext_Loop_FindingsInboxSurvivesDispatch drives the full
// board → dispatcher → bot → findings-inbox loop with stubs and asserts
// the findings dimension dropped in commit c134af2e (when findings moved
// from .md files to the board's `inbox` state) now holds as a board
// invariant:
//
//  1. Two `ready` work issues are created (mirrors assign_to_bots having
//     promoted operator-selected backlog items to ready).
//  2. The dispatcher claims each (ready → in_progress) and runs the bot.
//  3. Each dispatched bot records ONE out-of-scope observation into the
//     non-eligible `inbox` state (the findings flow), then clean-finishes.
//  4. The dispatcher auto-transitions each WORK issue in_progress → review
//     (guard 45eafe28) WITHOUT touching the inbox findings.
//  5. The inbox findings survive — same count, still in `inbox`, never
//     claimed, `findings` label intact — and are never themselves
//     dispatched (inbox is non-eligible), so the dispatch count holds at 2
//     across several further polls (no re-dispatch loop).
//
// Wall-clock budget: ~3× polling for dispatch→finish→transition, then
// 5× polling for the no-reloop watch. At 50ms polling that's well under
// the 3s deadline used for slow CI.
func TestWhatsNext_Loop_FindingsInboxSurvivesDispatch(t *testing.T) {
	const polling = 50 * time.Millisecond
	c, ns, runner, cleanup := newSmokeDispatcherFixture(t, polling)
	defer cleanup()

	botCaps := boardops.NewCapabilities("board.read,board.create")
	var dispatchCount atomic.Int32

	// Each dispatched bot posts one finding to the board's `inbox` state
	// (the c134af2e flow: create_issue state=inbox + `findings` label),
	// then signals a clean finish. It does NOT move its own work issue —
	// that's the dispatcher's job (and moving it would suppress the
	// auto-transition, cf. dispatcher's SkipsAutoTransitionWhenWorkflowMovedState).
	runner.Handler = func(_ context.Context, _ dispatcher.DispatchSpec) error {
		n := dispatchCount.Add(1)
		finding, _ := json.Marshal(map[string]any{
			"title":  fmt.Sprintf("Out-of-scope finding %d", n),
			"state":  native.StateInbox,
			"labels": []string{"findings", "kind:bug", "source:feature_dev"},
		})
		if _, err := boardops.Call(ns, botCaps, "create_issue", finding); err != nil {
			t.Errorf("bot posting finding to inbox: %v", err) // Errorf: safe off the test goroutine
		}
		return nil
	}

	caps := boardops.NewCapabilities("board.create,board.read,board.move,board.assign")
	mkReady := func(title string) native.Issue {
		raw, err := boardops.Call(ns, caps, "create_issue", json.RawMessage(`{"title":"`+title+`","state":"ready","assignee":"feature_dev"}`))
		if err != nil {
			t.Fatalf("create_issue %q: %v", title, err)
		}
		var iss native.Issue
		if err := json.Unmarshal(raw, &iss); err != nil {
			t.Fatalf("unmarshal %q: %v", title, err)
		}
		return iss
	}
	issX := mkReady("Refactor X")
	issY := mkReady("Implement Y")

	// Wait for both work issues to auto-transition to review.
	inReview := func(id string) bool {
		iss, _ := ns.Get(id)
		return iss != nil && iss.State == native.StateReview
	}
	waitUntil(t, 10*time.Second, "both work issues to auto-transition to review (guard 45eafe28)",
		func() bool { return inReview(issX.ID) && inReview(issY.ID) },
		func() string {
			x, _ := ns.Get(issX.ID)
			y, _ := ns.Get(issY.ID)
			return fmt.Sprintf("%q state=%v, %q state=%v",
				issX.Title, stateOf(x), issY.Title, stateOf(y))
		})
	if got := dispatchCount.Load(); got != 2 {
		t.Fatalf("expected exactly 2 work dispatches, got %d", got)
	}

	// Findings survive: both inbox issues persist untouched, with the
	// `findings` label, never claimed (inbox is non-eligible).
	inbox, err := ns.List(native.ListFilter{States: []string{native.StateInbox}})
	if err != nil {
		t.Fatalf("List inbox: %v", err)
	}
	if len(inbox) != 2 {
		t.Fatalf("expected 2 surviving inbox findings, got %d: %v", len(inbox), titlesOf(inbox))
	}
	for _, f := range inbox {
		if f.State != native.StateInbox {
			t.Errorf("finding %q drifted out of inbox: state=%s", f.Title, f.State)
		}
		if f.Claim != "" {
			t.Errorf("finding %q was claimed (%q) — inbox must never be dispatched", f.Title, f.Claim)
		}
		if !slices.Contains(f.Labels, "findings") {
			t.Errorf("finding %q lost its `findings` label: %v", f.Title, f.Labels)
		}
	}

	// No re-dispatch: inbox is non-eligible and review is not eligible, so
	// several more polls must not grow the dispatch count.
	time.Sleep(5 * polling)
	if got := dispatchCount.Load(); got != 2 {
		t.Fatalf("re-dispatch detected: dispatchCount=%d (expected 2) — inbox findings or review issues were re-picked", got)
	}
	if running := len(c.Snapshot().Running); running != 0 {
		t.Fatalf("running set not drained after clean finish: %d still running", running)
	}
}
