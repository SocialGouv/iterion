package model

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The tool-step bound is the node execution's, not the attempt's (#3283,
// F3). Measured on a review-pr reviewer with `tool_max_steps: 30`: steps
// 1..26, a stream retry, 1..2, a retry, 1..30 — 58 tool turns, each retry
// handed the whole bound again while the session store replayed the dead
// attempt's history. Every test below drives a claw node through Execute on
// a scripted model and counts the tool turns that RAN (the probe tool's own
// executions), never the requests.

const budgetSchema = `{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"]}`

// budgetProbe is the node's one tool; ran counts its executions.
func budgetProbe() (delegate.ToolDef, *atomic.Int64) {
	ran := &atomic.Int64{}
	return delegate.ToolDef{
		Name:        "probe",
		Description: "read the workspace",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(context.Context, json.RawMessage) (string, error) {
			ran.Add(1)
			return "observed", nil
		},
	}, ran
}

// offersTool reports whether a request offers the named tool: a tool-loop
// request offers the node's tools, the structured recovery pass offers only
// its schema tool.
func offersTool(req api.CreateMessageRequest, name string) bool {
	return slices.ContainsFunc(req.Tools, func(t api.Tool) bool { return t.Name == name })
}

// cutStream is a response the stream cuts before it finishes: the transient
// failure a retry re-issues.
func cutStream() []api.StreamEvent {
	return []api.StreamEvent{
		{Type: api.EventMessageStart, InputTokens: 10},
		{Type: api.EventError, ErrorMessage: "unexpected EOF"},
	}
}

// budgetModel answers by request kind: a loop request calls probe, a
// recovery request answers through the schema tool. cut lists the 1-based
// requests the stream cuts, narrate the loop requests answered with prose
// and no tool call.
func budgetModel(cut, narrate map[int]bool) *funcClient {
	return &funcClient{fn: func(n int, req api.CreateMessageRequest) ([]api.StreamEvent, error) {
		switch {
		case cut[n]:
			return cutStream(), nil
		case offersTool(req, "probe") && narrate[n]:
			return textEvents("I will read the diff next.", 10, 5), nil
		case offersTool(req, "probe"):
			return toolUseEvents(fmt.Sprintf("tu_%d", n), "probe", `{}`, 10, 5), nil
		}
		return toolUseEvents("so_1", "structured_output", `{"verdict":"done"}`, 10, 5), nil
	}}
}

// requestKinds renders the requests a client received, in order: "L" for a
// tool-loop request, "R" for a recovery request.
func requestKinds(c *funcClient) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	kinds := make([]byte, len(c.calls))
	for i, req := range c.calls {
		kinds[i] = 'R'
		if offersTool(req, "probe") {
			kinds[i] = 'L'
		}
	}
	return string(kinds)
}

// budgetBackend is a claw backend on the scripted client; its own retry loop
// takes attempts attempts on a transient failure.
func budgetBackend(client api.APIClient, attempts int) *ClawBackend {
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return client, nil })
	return NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: attempts, BackoffBase: time.Millisecond})
}

func budgetTask(probe delegate.ToolDef, maxSteps int) delegate.Task {
	return delegate.Task{
		NodeID: "judge", Model: "test/test-model", UserPrompt: "Judge the diff.",
		HasTools: true, ToolDefs: []delegate.ToolDef{probe}, ToolMaxSteps: maxSteps,
		OutputSchema: json.RawMessage(budgetSchema),
	}
}

// budgetCtx wires the run's session store, so a retry replays the dead
// attempt's history the way a run does.
func budgetCtx() context.Context {
	return withRuntimeContext(context.Background(), "run-budget", newNodeSessionStore())
}

func requireVerdict(t *testing.T, res delegate.Result, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("the node must recover an answer: %v", err)
	}
	if got, _ := res.Output["verdict"].(string); got != "done" {
		t.Fatalf("output = %v, want the recovery pass's verdict", res.Output)
	}
}

// Two stream cuts mid-loop: 2 turns then a cut, 2 turns then a cut, and
// the third attempt is handed the 2 turns left — not the bound again — after
// which the loop is spent and the recovery pass answers, once.
//
// Mutant: hand every attempt the node's whole bound (a counter per attempt)
// and the third attempt runs 6 more turns: 10 for a bound of 6.
func TestClawStepBudget_StreamRetriesShareTheBound(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{3: true, 6: true}, nil)
	res, err := budgetBackend(client, 3).Execute(budgetCtx(), budgetTask(probe, 6))
	requireVerdict(t, res, err)
	if got := ran.Load(); got != 6 {
		t.Fatalf("%d tool turns ran for tool_max_steps 6 — want 2 + 2 + the 2 left (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLLLLLLR" {
		t.Errorf("requests = %s, want 3 loop requests and a cut twice, then the 2 turns left and ONE recovery pass", got)
	}
	if res.ToolTurns != 6 {
		t.Errorf("Result.ToolTurns = %d, want the 6 every attempt completed", res.ToolTurns)
	}
}

// A cut before the first turn completes spends nothing: the retry gets the
// whole bound. Only COMPLETED tool turns count.
//
// Mutant: charge the request the stream cut as a turn, and the retry runs
// one turn short of the bound.
func TestClawStepBudget_ACutBeforeTheFirstTurnSpendsNothing(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{1: true}, nil)
	res, err := budgetBackend(client, 3).Execute(budgetCtx(), budgetTask(probe, 4))
	requireVerdict(t, res, err)
	if got := ran.Load(); got != 4 {
		t.Fatalf("%d tool turns ran, want the whole bound of 4 after a cut that completed nothing (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLLLR" {
		t.Errorf("requests = %s, want the cut, the 4 turns, then one recovery pass", got)
	}
}

// Every allowed turn completes, then the recovery pass is cut and retried:
// the retry finds the bound spent and goes straight to the recovery pass,
// answering from the replayed conversation. It never calls the loop with
// nothing left — MaxSteps 0, which the loop reads as its own default of 10.
//
// Mutants: call the loop with the 0 left (10 more turns), or hand the retry
// the bound again (3 more turns) — either runs loop requests after the cut.
func TestClawStepBudget_AFailureAfterTheLastTurnGoesStraightToRecovery(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{4: true}, nil)
	res, err := budgetBackend(client, 3).Execute(budgetCtx(), budgetTask(probe, 3))
	requireVerdict(t, res, err)
	if got := ran.Load(); got != 3 {
		t.Fatalf("%d tool turns ran for tool_max_steps 3 (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLRR" {
		t.Fatalf("requests = %s, want the 3 turns, the cut recovery pass, and the retry's recovery pass alone", got)
	}
	// The retry's recovery answers from the conversation the loop left: the
	// session store replays all three tool results into it.
	last := client.calls[len(client.calls)-1]
	results := 0
	for _, m := range last.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				results++
			}
		}
	}
	if results != 3 {
		t.Errorf("the retry's recovery pass carries %d tool results, want the 3 the loop completed", results)
	}
}

// The tool-use nudge spends from the same bound. Attempt 1 completes 3 of 4
// turns and is cut; attempt 2's first pass narrates without a tool call, so
// the nudge re-runs the loop — with the ONE turn left, after which the
// recovery pass answers.
//
// Mutant: hand the nudge the options' MaxSteps (the node's bound) instead of
// what is left, and it runs 4 turns: 7 for a bound of 4.
func TestClawStepBudget_TheNudgeSpendsFromTheSameBound(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{4: true}, map[int]bool{5: true})
	res, err := budgetBackend(client, 3).Execute(budgetCtx(), budgetTask(probe, 4))
	requireVerdict(t, res, err)
	if got := ran.Load(); got != 4 {
		t.Fatalf("%d tool turns ran for tool_max_steps 4 — want 3, then the nudge's 1 (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLLLLR" {
		t.Errorf("requests = %s, want 3 turns, the cut, the narration, the nudge's one turn, one recovery pass", got)
	}
	if res.ToolTurns != 4 {
		t.Errorf("Result.ToolTurns = %d, want 4 — the nudge's turn is the node's too", res.ToolTurns)
	}
}

// An in-place retry of the EXECUTOR re-enters Execute on the conversation
// the dead attempt left (the session store survives it), so the turns that
// attempt completed stay spent: claw's own retry loop takes one attempt
// here, the executor's takes the cut, and its second attempt is handed the
// 3 turns left of 5.
//
// Mutant: drop the carry at the dispatch (the attempt sees no
// ToolTurnsSpent) and the second attempt runs 5 turns: 7 for a bound of 5.
func TestClawStepBudget_AnExecutorRetryContinuesTheSpentTurns(t *testing.T) {
	t.Parallel()
	probe, ran := budgetProbe()
	client := budgetModel(map[int]bool{3: true}, nil)
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, budgetBackend(client, 1))
	e := newFallbackExecutor(reg, EventHooks{})
	build := e.newElementBuilder("judge", delegate.BackendClaw, nil,
		func(context.Context, string) (*delegate.Task, error) {
			task := budgetTask(probe, 5)
			return &task, nil
		})
	out, err := e.dispatchChain(budgetCtx(), "judge", []chainElement{{Label: "primary"}}, "test/test-model", build)
	requireVerdict(t, out.Result, err)
	if got := ran.Load(); got != 5 {
		t.Fatalf("%d tool turns ran for tool_max_steps 5 — want 2, then the 3 left (requests %s)", got, requestKinds(client))
	}
	if got := requestKinds(client); got != "LLLLLLR" {
		t.Errorf("requests = %s, want 2 turns and the cut, then the 3 turns left and one recovery pass", got)
	}
	if out.Result.ToolTurns != 5 {
		t.Errorf("Result.ToolTurns = %d, want the attempts' 2 + 3 folded", out.Result.ToolTurns)
	}
}

// A seeded execution is handed only what is left, and one with nothing left
// makes no loop request at all — the sandboxed runner's re-entry, where the
// seed is all the crossing carries.
func TestClawStepBudget_ASeededExecutionRunsOnlyWhatIsLeft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		spent, want int
		kinds       string
	}{
		{spent: 3, want: 2, kinds: "LLR"},
		{spent: 5, want: 0, kinds: "R"},
		{spent: 9, want: 0, kinds: "R"},
	} {
		t.Run(fmt.Sprintf("spent_%d", tc.spent), func(t *testing.T) {
			t.Parallel()
			probe, ran := budgetProbe()
			client := budgetModel(nil, nil)
			task := budgetTask(probe, 5)
			task.ToolTurnsSpent = tc.spent
			res, err := budgetBackend(client, 1).Execute(budgetCtx(), task)
			requireVerdict(t, res, err)
			if got := ran.Load(); got != int64(tc.want) {
				t.Fatalf("%d tool turns ran with %d of 5 spent, want %d", got, tc.spent, tc.want)
			}
			if got := requestKinds(client); got != tc.kinds {
				t.Errorf("requests = %s, want %s", got, tc.kinds)
			}
			if res.ToolTurns != tc.want {
				t.Errorf("Result.ToolTurns = %d, want this invocation's %d", res.ToolTurns, tc.want)
			}
		})
	}
}
