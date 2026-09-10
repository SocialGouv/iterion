package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// newRecoveryGateWorkflow is the silent-green shape: an agent whose work a
// downstream deterministic gate then judges. If the agent is skipped, the
// gate judges an untouched tree.
func newRecoveryGateWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "recovery_gate_test",
		Entry: "agent_a",
		Nodes: map[string]ir.Node{
			"agent_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent_a"}},
			"gate":    &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}, Command: "true"},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "agent_a", To: "gate"},
			{From: "gate", To: "done"},
		},
	}
}

func pauseForHumanOn(code ErrorCode) RecoveryDispatch {
	return func(_ context.Context, _ error, _ func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		return RecoveryAction{Kind: RecoveryPauseForHuman, Reason: "provider rejected the credential"}, code
	}
}

// TestRecoveryPause_ResumeReExecutesTheFailedNode pins the contract the
// pause's own question states: acknowledging a recovery pause RETRIES the
// failed node. The acknowledgement is audit (recorded on the interaction),
// never the node's output, and the node re-executes BEFORE any successor
// starts — a gate downstream must never judge a tree the agent never
// touched (issue #688).
func TestRecoveryPause_ResumeReExecutesTheFailedNode(t *testing.T) {
	exec := &flakyExecutor{
		target:   "agent_a",
		failErr:  &RuntimeError{Code: ErrCodeAuthFailed, Message: "authentication failed: Not logged in"},
		failures: 1,
	}
	s := tmpStore(t)
	eng := New(newRecoveryGateWorkflow(), s, exec, WithRecoveryDispatch(pauseForHumanOn(ErrCodeAuthFailed)))
	ctx := context.Background()
	const runID = "run-recovery-retry"

	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint == nil || !r.Checkpoint.RecoveryPause || r.Checkpoint.RecoveryCode != string(ErrCodeAuthFailed) {
		t.Fatalf("checkpoint must carry the recovery marker, got %+v", r.Checkpoint)
	}
	interaction, err := s.LoadInteraction(ctx, runID, r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	if interaction.Kind != store.InteractionKindRecovery {
		t.Fatalf("interaction kind = %q, want %q", interaction.Kind, store.InteractionKindRecovery)
	}

	answers := map[string]any{"acknowledge_recovery": "ceiling raised, slot available — retry"}
	if err := eng.Resume(ctx, runID, answers); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if exec.calls != 2 {
		t.Fatalf("agent_a executions = %d, want 2 (the acknowledged pause must re-execute the node)", exec.calls)
	}

	r, err = s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun after resume: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %q, want finished (error: %s)", r.Status, r.Error)
	}
	out := r.Checkpoint.Outputs["agent_a"]
	if out["ok"] != true {
		t.Fatalf("agent_a output = %v, want the node's real output {ok:true}", out)
	}
	if _, leaked := out["acknowledge_recovery"]; leaked {
		t.Fatalf("the acknowledgement leaked into the node's output: %v", out)
	}

	// The answer stays on the record: it is the operator's audit trail of
	// what was fixed, and the human_answers_recorded event names it.
	interaction, err = s.LoadInteraction(ctx, runID, interaction.ID)
	if err != nil {
		t.Fatalf("LoadInteraction after resume: %v", err)
	}
	if interaction.AnsweredAt == nil || interaction.Answers["acknowledge_recovery"] != answers["acknowledge_recovery"] {
		t.Fatalf("acknowledgement not recorded on the interaction: %+v", interaction)
	}

	// Trace shape: human_answers_recorded → run_resumed{resumed_from:
	// recovery_pause, restart_node: agent_a} → node_started agent_a →
	// … → node_started gate. No node_finished for agent_a may precede the
	// resume: that would be the skip being blessed as completion.
	events, err := s.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	resumedAt := int64(-1)
	for _, evt := range events {
		if evt.Type == store.EventRunResumed {
			resumedAt = evt.Seq
			if evt.Data["resumed_from"] != "recovery_pause" || evt.Data["restart_node"] != "agent_a" || evt.Data["recovery_code"] != string(ErrCodeAuthFailed) {
				t.Fatalf("run_resumed data = %v, want {resumed_from: recovery_pause, restart_node: agent_a, recovery_code: AUTH_FAILED}", evt.Data)
			}
		}
	}
	if resumedAt < 0 {
		t.Fatal("missing run_resumed event")
	}
	agentRestart, gateStart := int64(-1), int64(-1)
	for _, evt := range events {
		switch {
		case evt.Type == store.EventNodeFinished && evt.NodeID == "agent_a" && evt.Seq < resumedAt:
			t.Fatalf("node_finished for agent_a at seq %d precedes run_resumed at seq %d: the pause was blessed as completion", evt.Seq, resumedAt)
		case evt.Type == store.EventNodeStarted && evt.NodeID == "agent_a" && evt.Seq > resumedAt && agentRestart < 0:
			agentRestart = evt.Seq
		case evt.Type == store.EventNodeStarted && evt.NodeID == "gate" && gateStart < 0:
			gateStart = evt.Seq
		}
	}
	if agentRestart < 0 {
		t.Fatal("no node_started for agent_a after run_resumed — the node was not re-executed")
	}
	if gateStart < 0 || gateStart < agentRestart {
		t.Fatalf("gate started at seq %d, before agent_a re-executed at seq %d", gateStart, agentRestart)
	}
}

// TestRecoveryPause_ResumeKeepsAttemptBudget: the per-(node, code) attempt
// bucket survives the pause, so a second identical failure after the
// acknowledgement is judged as attempt two — the dispatcher's retry
// ceiling counts across the pause instead of restarting from zero.
func TestRecoveryPause_ResumeKeepsAttemptBudget(t *testing.T) {
	exec := &flakyExecutor{
		target:   "agent_a",
		failErr:  &RuntimeError{Code: ErrCodeAuthFailed, Message: "still not logged in"},
		failures: 2,
	}
	var priorSeen []int
	dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		priorSeen = append(priorSeen, prior(ErrCodeAuthFailed))
		return RecoveryAction{Kind: RecoveryPauseForHuman, Reason: "auth"}, ErrCodeAuthFailed
	})
	s := tmpStore(t)
	eng := New(newRecoveryGateWorkflow(), s, exec, WithRecoveryDispatch(dispatch))
	ctx := context.Background()
	const runID = "run-recovery-budget"

	if err := eng.Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	ack := map[string]any{"acknowledge_recovery": "retry"}
	if err := eng.Resume(ctx, runID, ack); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("first Resume: want a second ErrRunPaused (the node failed again), got %v", err)
	}
	r, _ := s.LoadRun(ctx, runID)
	if got := r.Checkpoint.NodeAttempts["agent_a"][string(ErrCodeAuthFailed)]; got != 2 {
		t.Fatalf("attempt bucket after the second failure = %d, want 2", got)
	}
	if err := eng.Resume(ctx, runID, ack); err != nil {
		t.Fatalf("second Resume: %v", err)
	}
	if len(priorSeen) != 2 || priorSeen[0] != 0 || priorSeen[1] != 1 {
		t.Fatalf("dispatcher prior view = %v, want [0 1]", priorSeen)
	}
	if exec.calls != 3 {
		t.Fatalf("agent_a executions = %d, want 3", exec.calls)
	}
	r, _ = s.LoadRun(ctx, runID)
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %q, want finished (error: %s)", r.Status, r.Error)
	}
}

// TestDelegatePauseIsNotARecoveryPause guards the other face: a pause
// raised by the agent itself (ask_user / _needs_interaction) is a delegate
// pause — no recovery marker, ordinary interaction kind — and keeps
// resuming through the backend re-invocation. The re-invocation itself is
// pinned by TestAskUserResumeRelaysPriorQA in engine_test.go.
func TestDelegatePauseIsNotARecoveryPause(t *testing.T) {
	wf := interactionWorkflow(ir.InteractionHuman)
	exec := newStubExecutor()
	exec.on("worker", func(_ map[string]any) (map[string]any, error) {
		return nil, &model.ErrNeedsInteraction{
			NodeID:    "worker",
			Questions: map[string]any{"approval": "Ship it?"},
			SessionID: "session-xyz",
			Backend:   "claude_code",
		}
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	ctx := context.Background()
	if err := eng.Run(ctx, "run-delegate-pause", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
	r, err := s.LoadRun(ctx, "run-delegate-pause")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Checkpoint.RecoveryPause || r.Checkpoint.RecoveryCode != "" {
		t.Fatalf("a delegate pause must not carry the recovery marker: %+v", r.Checkpoint)
	}
	if r.Checkpoint.BackendName != "claude_code" {
		t.Fatalf("delegate pause must keep its backend for re-invocation, got %q", r.Checkpoint.BackendName)
	}
	interaction, err := s.LoadInteraction(ctx, "run-delegate-pause", r.Checkpoint.InteractionID)
	if err != nil {
		t.Fatalf("LoadInteraction: %v", err)
	}
	if interaction.Kind != "" {
		t.Fatalf("delegate pause interaction kind = %q, want the blocking default", interaction.Kind)
	}
}
