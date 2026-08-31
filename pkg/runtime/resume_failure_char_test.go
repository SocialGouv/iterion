package runtime

// Characterization tests for resumeFromFailure ahead of its decomposition
// (backlog B2). They pin CURRENT observable behavior — status routing, the
// run_resumed event payload, checkpoint-state restoration (outputs, loop
// counters, budget spend, backend rehydration), map-aliasing isolation, and
// the hash-check-before-claim ordering — so a later extract-function
// refactor that changes behavior fails here. Built on the same seams as
// engine_test.go: tmpStore + stubExecutor + hand-assembled ir.Workflow.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// charResumeWF builds the canonical linear fixture: step_a -> step_b -> done.
func charResumeWF() *ir.Workflow {
	return &ir.Workflow{
		Name:  "resume_char",
		Entry: "step_a",
		Nodes: map[string]ir.Node{
			"step_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step_a"}},
			"step_b": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step_b"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "step_a", To: "step_b"},
			{From: "step_b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// charFindEvent returns the LAST event of the given type, or nil.
func charFindEvent(t *testing.T, s store.RunStore, runID string, typ store.EventType) *store.Event {
	t.Helper()
	evs, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var found *store.Event
	for _, e := range evs {
		if e.Type == typ {
			found = e
		}
	}
	return found
}

// TestResumeFromFailure_ResumableStatusTable pins the three statuses that
// route through resumeFromFailure with a checkpoint: failed_resumable,
// cancelled, and paused_operator. Each must restart AT the checkpoint node
// (upstream not replayed), finish, and emit run_resumed with
// {resumed_from: failed, restart_node: <cp node>} and NO from_entry key.
func TestResumeFromFailure_ResumableStatusTable(t *testing.T) {
	cases := []struct {
		name             string
		status           store.RunStatus
		viaFailResumable bool
	}{
		{"failed_resumable", store.RunStatusFailedResumable, true},
		{"cancelled", store.RunStatusCancelled, false},
		{"paused_operator", store.RunStatusPausedOperator, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			runID := "run-char-" + tc.name
			if _, err := s.CreateRun(ctx, runID, "resume_char", nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			cp := &store.Checkpoint{
				NodeID:  "step_b",
				Outputs: map[string]map[string]any{"step_a": {"result": "ok"}},
			}
			if tc.viaFailResumable {
				if err := s.FailRunResumable(ctx, runID, cp, "boom"); err != nil {
					t.Fatalf("FailRunResumable: %v", err)
				}
			} else {
				if err := s.SaveCheckpoint(ctx, runID, cp); err != nil {
					t.Fatalf("SaveCheckpoint: %v", err)
				}
				if err := s.UpdateRunStatus(ctx, runID, tc.status, "interrupted"); err != nil {
					t.Fatalf("UpdateRunStatus: %v", err)
				}
			}

			var aCalls, bCalls int
			exec := newStubExecutor()
			exec.on("step_a", func(_ map[string]any) (map[string]any, error) {
				aCalls++
				return map[string]any{"result": "ok"}, nil
			})
			exec.on("step_b", func(_ map[string]any) (map[string]any, error) {
				bCalls++
				return map[string]any{"result": "resumed"}, nil
			})

			eng := New(charResumeWF(), s, exec)
			if err := eng.Resume(ctx, runID, nil); err != nil {
				t.Fatalf("Resume from %s: %v", tc.status, err)
			}
			if aCalls != 0 {
				t.Errorf("step_a replayed %d times, want 0", aCalls)
			}
			if bCalls != 1 {
				t.Errorf("step_b executed %d times, want 1", bCalls)
			}
			r, err := s.LoadRun(ctx, runID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if r.Status != store.RunStatusFinished {
				t.Errorf("status = %s, want finished", r.Status)
			}

			ev := charFindEvent(t, s, runID, store.EventRunResumed)
			if ev == nil {
				t.Fatal("no run_resumed event emitted")
			}
			if got := ev.Data["resumed_from"]; got != "failed" {
				t.Errorf("run_resumed data.resumed_from = %v, want %q", got, "failed")
			}
			if got := ev.Data["restart_node"]; got != "step_b" {
				t.Errorf("run_resumed data.restart_node = %v, want %q", got, "step_b")
			}
			if _, has := ev.Data["from_entry"]; has {
				t.Error("run_resumed must not carry from_entry when a checkpoint exists")
			}
		})
	}
}

// TestResumeFromFailure_NoCheckpointRestartsFromEntry pins the checkpoint-
// less path (a run that failed before its first save_checkpoint): the resume
// restarts from the workflow entry, re-running everything, and the
// run_resumed event carries from_entry=true with restart_node=<entry>.
func TestResumeFromFailure_NoCheckpointRestartsFromEntry(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-char-nocp"
	if _, err := s.CreateRun(ctx, runID, "resume_char", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, nil, "pre-first-node boom"); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	var aCalls, bCalls int
	exec := newStubExecutor()
	exec.on("step_a", func(_ map[string]any) (map[string]any, error) {
		aCalls++
		return map[string]any{"result": "ok"}, nil
	})
	exec.on("step_b", func(_ map[string]any) (map[string]any, error) {
		bCalls++
		return map[string]any{"result": "ok"}, nil
	})

	eng := New(charResumeWF(), s, exec)
	if err := eng.Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if aCalls != 1 || bCalls != 1 {
		t.Errorf("(step_a, step_b) executions = (%d, %d), want (1, 1) — full restart from entry", aCalls, bCalls)
	}
	r, _ := s.LoadRun(ctx, runID)
	if r.Status != store.RunStatusFinished {
		t.Errorf("status = %s, want finished", r.Status)
	}

	ev := charFindEvent(t, s, runID, store.EventRunResumed)
	if ev == nil {
		t.Fatal("no run_resumed event emitted")
	}
	if got := ev.Data["restart_node"]; got != "step_a" {
		t.Errorf("run_resumed data.restart_node = %v, want the entry node", got)
	}
	if got, has := ev.Data["from_entry"]; !has || got != true {
		t.Errorf("run_resumed data.from_entry = %v (present=%v), want true", got, has)
	}
}

// TestResumeFromFailure_HashMismatchPreservesResumableState pins the
// ordering guarantee that the workflow-hash gate runs BEFORE the CAS claim:
// a rejected resume must leave the run exactly as it was — status still
// failed_resumable, checkpoint intact, and NO run_resumed event — so a
// corrected resume (--force or restored source) still works afterwards.
func TestResumeFromFailure_HashMismatchPreservesResumableState(t *testing.T) {
	wf := charResumeWF()
	s := tmpStore(t)
	ctx := context.Background()

	fail := true
	exec := newStubExecutor()
	exec.on("step_a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"result": "ok"}, nil
	})
	exec.on("step_b", func(_ map[string]any) (map[string]any, error) {
		if fail {
			return nil, fmt.Errorf("transient failure")
		}
		return map[string]any{"result": "ok"}, nil
	})

	const runID = "run-char-hash"
	if err := New(wf, s, exec, WithWorkflowHash("hash-one")).Run(ctx, runID, nil); err == nil {
		t.Fatal("expected the seeded run to fail")
	}
	fail = false

	// Mismatched hash, no force → rejected.
	err := New(wf, s, exec, WithWorkflowHash("hash-two")).Resume(ctx, runID, nil)
	if err == nil {
		t.Fatal("expected hash-mismatch rejection")
	}
	if !strings.Contains(err.Error(), "workflow source has changed") {
		t.Fatalf("unexpected rejection: %v", err)
	}

	r, _ := s.LoadRun(ctx, runID)
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status after rejected resume = %s, want failed_resumable (untouched)", r.Status)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "step_b" {
		t.Errorf("checkpoint after rejected resume = %+v, want intact at step_b", r.Checkpoint)
	}
	if ev := charFindEvent(t, s, runID, store.EventRunResumed); ev != nil {
		t.Error("run_resumed emitted for a rejected resume — hash gate must precede the claim")
	}

	// Same mismatch WITH force → proceeds to completion.
	if err := New(wf, s, exec, WithWorkflowHash("hash-two"), WithForceResume(true)).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("forced resume: %v", err)
	}
	r, _ = s.LoadRun(ctx, runID)
	if r.Status != store.RunStatusFinished {
		t.Errorf("status after forced resume = %s, want finished", r.Status)
	}
}

// TestResumeFromFailure_RestoresLoopCounters pins that a checkpoint's loop
// counters carry into the resumed execution: with retry(max 3) already at 1,
// the resumed loop may only traverse twice more before the back-edge is
// skipped as exhausted — a fresh counter would have allowed three.
func TestResumeFromFailure_RestoresLoopCounters(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "resume_char_loop",
		Entry: "fix",
		Nodes: map[string]ir.Node{
			"fix":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "fix"}},
			"judge": &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "judge"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "fix", To: "judge"},
			{From: "judge", To: "done", Condition: "pass"},
			{From: "judge", To: "fix", Condition: "pass", Negated: true, LoopName: "retry"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"retry": {Name: "retry", MaxIterations: 3},
		},
	}

	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-char-loop"
	if _, err := s.CreateRun(ctx, runID, "resume_char_loop", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cp := &store.Checkpoint{
		NodeID:       "judge",
		Outputs:      map[string]map[string]any{"fix": {"attempt": 1}},
		LoopCounters: map[string]int{"retry": 1},
	}
	if err := s.FailRunResumable(ctx, runID, cp, "boom"); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	var fixCalls int
	exec := newStubExecutor()
	exec.on("fix", func(_ map[string]any) (map[string]any, error) {
		fixCalls++
		return map[string]any{"attempt": fixCalls}, nil
	})
	exec.on("judge", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"pass": false}, nil
	})

	err := New(wf, s, exec).Resume(ctx, runID, nil)
	if err == nil {
		t.Fatal("expected the exhausted loop to fail the run")
	}
	// With the exhausted back-edge skipped and pass=false, no edge matches.
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeNoOutgoingEdge {
		t.Errorf("error = %v, want RuntimeError NO_OUTGOING_EDGE", err)
	}
	// Restored counter 1 → traversals 2 and 3 only. A lost counter would
	// have produced 3 fix executions.
	if fixCalls != 2 {
		t.Errorf("fix executed %d times after resume, want 2 (counter restored at 1/3)", fixCalls)
	}
}

// TestResumeFromFailure_CheckpointMapsNotMutated pins the deep-copy
// isolation of the loaded checkpoint: the resumed execution writes outputs
// and bumps artifact versions on its own runState, and none of that may
// retroactively appear in the caller-held Checkpoint maps (a concurrent HTTP
// read could be iterating them).
func TestResumeFromFailure_CheckpointMapsNotMutated(t *testing.T) {
	wf := charResumeWF()
	// step_b publishes so the resumed run bumps artifactVersions.
	wf.Nodes["step_b"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step_b"}, Publish: "res"}

	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-char-alias"
	if _, err := s.CreateRun(ctx, runID, "resume_char", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cp := &store.Checkpoint{
		NodeID:           "step_b",
		Outputs:          map[string]map[string]any{"step_a": {"result": "ok"}},
		LoopCounters:     map[string]int{},
		ArtifactVersions: map[string]int{},
	}
	if err := s.FailRunResumable(ctx, runID, cp, "boom"); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	exec := newStubExecutor()
	exec.on("step_b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"result": "resumed"}, nil
	})

	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	// Keep the resume's skill/plugin mirroring inside a scratch dir.
	r.WorkDir = t.TempDir()

	eng := New(wf, s, exec)
	if err := eng.resumeFromFailure(ctx, r); err != nil {
		t.Fatalf("resumeFromFailure: %v", err)
	}

	// The held checkpoint must be exactly what we loaded.
	if len(r.Checkpoint.Outputs) != 1 {
		t.Errorf("checkpoint Outputs grew to %d entries: %v", len(r.Checkpoint.Outputs), r.Checkpoint.Outputs)
	}
	if _, has := r.Checkpoint.Outputs["step_b"]; has {
		t.Error("resumed node's output leaked into the held checkpoint (aliased map)")
	}
	if got := r.Checkpoint.Outputs["step_a"]["result"]; got != "ok" {
		t.Errorf("checkpoint Outputs[step_a] mutated: %v", got)
	}
	if len(r.Checkpoint.ArtifactVersions) != 0 {
		t.Errorf("checkpoint ArtifactVersions mutated: %v (publish bump aliased)", r.Checkpoint.ArtifactVersions)
	}
	if len(r.Checkpoint.LoopCounters) != 0 {
		t.Errorf("checkpoint LoopCounters mutated: %v", r.Checkpoint.LoopCounters)
	}
}

// TestResumeFromFailure_BackendRehydrationInjectedOnce pins the fork/pause
// rehydration seam: a checkpoint carrying a backend conversation + session
// id injects delegate.ResumeConversationKey / delegate.SessionIDKey into the
// RESTART node's input only — downstream nodes must not see them.
func TestResumeFromFailure_BackendRehydrationInjectedOnce(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "resume_char_rehydrate",
		Entry: "step_a",
		Nodes: map[string]ir.Node{
			"step_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step_a"}},
			"step_b": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step_b"}},
			"step_c": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step_c"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "step_a", To: "step_b"},
			{From: "step_b", To: "step_c"},
			{From: "step_c", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-char-rehydrate"
	if _, err := s.CreateRun(ctx, runID, "resume_char_rehydrate", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	conversation := json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"hi"}]}]`)
	cp := &store.Checkpoint{
		NodeID:              "step_b",
		Outputs:             map[string]map[string]any{"step_a": {"result": "ok"}},
		BackendConversation: conversation,
		BackendSessionID:    "sess-42",
	}
	if err := s.FailRunResumable(ctx, runID, cp, "boom"); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	inputs := map[string]map[string]any{}
	record := func(node string) func(map[string]any) (map[string]any, error) {
		return func(in map[string]any) (map[string]any, error) {
			cp := make(map[string]any, len(in))
			for k, v := range in {
				cp[k] = v
			}
			inputs[node] = cp
			return map[string]any{"result": node}, nil
		}
	}
	exec := newStubExecutor()
	exec.on("step_b", record("step_b"))
	exec.on("step_c", record("step_c"))

	if err := New(wf, s, exec).Resume(ctx, runID, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	bIn := inputs["step_b"]
	if bIn == nil {
		t.Fatal("step_b never executed")
	}
	conv, has := bIn[delegate.ResumeConversationKey]
	if !has {
		t.Error("restart node input missing the rehydrated conversation")
	} else {
		// The consumer (applyResumeContinuity) type-asserts json.RawMessage;
		// the injected value must satisfy it or the rehydration is
		// silently dropped — pin the exact dynamic type.
		raw, ok := conv.(json.RawMessage)
		if !ok {
			t.Fatalf("rehydrated conversation type = %T, want json.RawMessage (consumer's assertion)", conv)
		}
		// The store round-trip re-indents run.json, so the payload is only
		// SEMANTICALLY stable — compare compacted forms.
		var gotBuf, wantBuf bytes.Buffer
		if err := json.Compact(&gotBuf, raw); err != nil {
			t.Fatalf("rehydrated conversation is not valid JSON: %v", err)
		}
		if err := json.Compact(&wantBuf, conversation); err != nil {
			t.Fatalf("fixture conversation invalid: %v", err)
		}
		if gotBuf.String() != wantBuf.String() {
			t.Errorf("rehydrated conversation = %s, want %s", gotBuf.String(), wantBuf.String())
		}
	}
	if got := bIn[delegate.SessionIDKey]; got != "sess-42" {
		t.Errorf("restart node session id = %v, want sess-42", got)
	}

	cIn := inputs["step_c"]
	if cIn == nil {
		t.Fatal("step_c never executed")
	}
	if _, has := cIn[delegate.ResumeConversationKey]; has {
		t.Error("downstream node received the resume conversation — injection must be one-shot")
	}
	if _, has := cIn[delegate.SessionIDKey]; has {
		t.Error("downstream node received the resume session id — injection must be one-shot")
	}
}

// charEnvSpyExecutor records the env-restore calls resumeFromFailure makes
// on an executor that implements the optional setter seams.
type charEnvSpyExecutor struct {
	workDirs  []string
	repoRoots []string
	varsCalls int
}

func (x *charEnvSpyExecutor) Execute(_ context.Context, _ ir.Node, _ map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}
func (x *charEnvSpyExecutor) SetWorkDir(d string)      { x.workDirs = append(x.workDirs, d) }
func (x *charEnvSpyExecutor) SetRepoRoot(r string)     { x.repoRoots = append(x.repoRoots, r) }
func (x *charEnvSpyExecutor) SetVars(_ map[string]any) { x.varsCalls++ }

// TestResumeFromFailure_ExecutorEnvRestored pins restoreRunEnv +
// pushExecutorVars: the persisted WorkDir/RepoRoot are pushed onto an
// executor implementing the setter seams, and the re-resolved vars are
// pushed before execution.
func TestResumeFromFailure_ExecutorEnvRestored(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "resume_char_env",
		Entry: "step_a",
		Nodes: map[string]ir.Node{
			"step_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step_a"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "step_a", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-char-env"
	if _, err := s.CreateRun(ctx, runID, "resume_char_env", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &store.Checkpoint{NodeID: "step_a"}, "boom"); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	workDir := t.TempDir()
	repoRoot := t.TempDir()
	r.WorkDir = workDir
	r.RepoRoot = repoRoot

	spy := &charEnvSpyExecutor{}
	eng := New(wf, s, spy)
	if err := eng.resumeFromFailure(ctx, r); err != nil {
		t.Fatalf("resumeFromFailure: %v", err)
	}

	if len(spy.workDirs) == 0 || spy.workDirs[len(spy.workDirs)-1] != workDir {
		t.Errorf("SetWorkDir calls = %v, want last == %q", spy.workDirs, workDir)
	}
	if len(spy.repoRoots) == 0 || spy.repoRoots[len(spy.repoRoots)-1] != repoRoot {
		t.Errorf("SetRepoRoot calls = %v, want last == %q", spy.repoRoots, repoRoot)
	}
	if spy.varsCalls == 0 {
		t.Error("SetVars never called — resolved vars must be pushed on resume")
	}
}

// TestResumeFromFailure_BudgetSpendRestored pins restoreBudgetAccounting:
// the cost already spent (per the checkpoint) counts against the workflow
// budget on resume — a run whose checkpoint is over budget fails with
// BUDGET_EXCEEDED before re-executing the restart node.
func TestResumeFromFailure_BudgetSpendRestored(t *testing.T) {
	wf := charResumeWF()
	wf.Budget = &ir.Budget{MaxCostUSD: 1.0}

	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-char-budget"
	if _, err := s.CreateRun(ctx, runID, "resume_char", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cp := &store.Checkpoint{
		NodeID:        "step_b",
		Outputs:       map[string]map[string]any{"step_a": {"result": "ok"}},
		BudgetCostUSD: 5.0, // already over the 1.0 cap
	}
	if err := s.FailRunResumable(ctx, runID, cp, "boom"); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	var bCalls int
	exec := newStubExecutor()
	exec.on("step_b", func(_ map[string]any) (map[string]any, error) {
		bCalls++
		return map[string]any{"result": "resumed"}, nil
	})

	err := New(wf, s, exec).Resume(ctx, runID, nil)
	if err == nil {
		t.Fatal("expected the restored spend to trip the budget")
	}
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeBudgetExceeded {
		t.Fatalf("error = %v, want RuntimeError BUDGET_EXCEEDED", err)
	}
	if bCalls != 0 {
		t.Errorf("step_b executed %d times, want 0 (budget gate precedes execution)", bCalls)
	}
	r, _ := s.LoadRun(ctx, runID)
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable (raise the cap + resume)", r.Status)
	}
}

// TestResumeFromFailure_ExhaustedDurationFinalizesBeforeSandbox pins the
// redelivery guard: persisted active time that already exhausted the run's
// duration budget must produce the ordinary resumable budget death before
// the sandbox lifecycle is invoked.
func TestResumeFromFailure_ExhaustedDurationFinalizesBeforeSandbox(t *testing.T) {
	wf := charResumeWF()
	wf.Budget = &ir.Budget{MaxDuration: "10m"}
	// Make sandbox provisioning observable if the guard ever moves too late.
	wf.Sandbox = &ir.SandboxSpec{Mode: "inline", Image: "must-not-be-provisioned.invalid/test:latest"}

	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-char-duration-preflight"
	if _, err := s.CreateRun(ctx, runID, "resume_char", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cp := &store.Checkpoint{
		NodeID:          "step_b",
		Outputs:         map[string]map[string]any{"step_a": {"result": "ok"}},
		BudgetElapsedNS: (11 * time.Minute).Nanoseconds(),
	}
	if err := s.FailRunResumable(ctx, runID, cp, "queue backend interrupted the prior attempt"); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}

	var nodeCalls, sandboxStarts int
	exec := newStubExecutor()
	exec.on("step_b", func(_ map[string]any) (map[string]any, error) {
		nodeCalls++
		return map[string]any{"result": "unexpected"}, nil
	})
	err := New(wf, s, exec, WithSandboxRunObserver(func(sandbox.Run) {
		sandboxStarts++
	})).Resume(ctx, runID, nil)
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeBudgetExceeded {
		t.Fatalf("error = %v, want RuntimeError BUDGET_EXCEEDED", err)
	}
	if nodeCalls != 0 {
		t.Errorf("restart node executed %d times, want 0", nodeCalls)
	}
	if sandboxStarts != 0 {
		t.Errorf("sandbox provisioning invoked %d times, want 0", sandboxStarts)
	}

	r, loadErr := s.LoadRun(ctx, runID)
	if loadErr != nil {
		t.Fatalf("LoadRun: %v", loadErr)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable", r.Status)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "step_b" {
		t.Fatalf("checkpoint = %+v, want preserved at step_b", r.Checkpoint)
	}

	events, eventsErr := s.LoadEvents(ctx, runID)
	if eventsErr != nil {
		t.Fatalf("LoadEvents: %v", eventsErr)
	}
	var sawBudgetExceeded, sawBudgetRunFailed bool
	for _, evt := range events {
		switch evt.Type {
		case store.EventBudgetExceeded:
			sawBudgetExceeded = evt.Data["dimension"] == "duration"
		case store.EventRunFailed:
			if evt.Data["code"] == string(ErrCodeBudgetExceeded) {
				sawBudgetRunFailed = true
			}
		case store.EventSandboxHostStateMounted, store.EventSandboxDevboxProvisioned,
			store.EventSandboxStarted, store.EventNodeStarted:
			t.Errorf("preflight emitted post-provision event %s: %+v", evt.Type, evt.Data)
		}
	}
	if !sawBudgetExceeded || !sawBudgetRunFailed {
		t.Errorf("events missing normal budget death: budget_exceeded=%v run_failed=%v", sawBudgetExceeded, sawBudgetRunFailed)
	}
}
