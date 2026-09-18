package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The plan hand-off cycle's exhaustion exits (C145, #1293), driven to the
// chat pause on the real engine with a stub executor. Both exits go through
// compose, a compute whose output types its fields: the gate found a first
// draft whose edge literals ("false", "[]") reached it as text and died
// SCHEMA_VALIDATION there — the constants come typed from the hand-off's
// expr, and these drives are what would have caught that.

func copilotHandoffDrive(t *testing.T, costUSD float64) (*ir.Workflow, *scenarioExecutor) {
	t.Helper()
	wf := compileFixtureStubSafe(t, "copilot/main.bot")
	exec := newScenarioExecutor()
	priced := func(usd float64, out map[string]any) map[string]any {
		out["_tokens"] = 100
		out["_cost_usd"] = usd
		return out
	}
	// Every Terra turn asks for a reflection: the cycle re-enters until the
	// back-edge is declined.
	exec.on("copi", func(input map[string]any) (map[string]any, error) {
		active := exec.callCount("copi") > 1
		return priced(costUSD, map[string]any{
			"reply": "", "close": false, "mode": "debug", "context_brief": "cause to investigate",
			"quick_replies": []any{}, "draft_bot": "", "has_draft": false,
			"editor_session_id": "", "editor_revision": 0, "editor_apply_intent": "none", "editor_save_intent": "none",
			"assistant_actions": []any{}, "file_changes": []any{}, "file_changes_intent": "none",
			"needs_reflection": true, "reflection_request": "Find and repair the recovered-run failure without restarting it.", "requires_authoring_context": false,
			"authoring_context":     "",
			"implementation_active": active, "_session_id": "terra-session", "_session_fingerprint": "claw:openai",
		}), nil
	})
	exec.on("reflect", func(map[string]any) (map[string]any, error) {
		return priced(costUSD, map[string]any{"implementation_plan": "inspect checkpoint, apply the narrow repair, then resume", "context_brief": "the plan"}), nil
	})
	exec.on("judge", func(map[string]any) (map[string]any, error) {
		return priced(costUSD, map[string]any{"critique": ""}), nil
	})
	wf.Vars["initial_message"].Default = "Répare le run sans le recommencer."
	return wf, exec
}

func composeOutput(t *testing.T, s store.RunStore, runID string) (map[string]any, string) {
	t.Helper()
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var out map[string]any
	var instructions string
	for _, e := range events {
		if string(e.Type) == "node_finished" && e.NodeID == "compose" {
			out, _ = e.Data["output"].(map[string]any)
		}
		if e.NodeID == "chat" && string(e.Type) == "human_input_requested" {
			instructions, _ = e.Data["instructions"].(string)
		}
	}
	if out == nil {
		t.Fatal("compose never finished: the exit did not reach the delivery projection")
	}
	return out, instructions
}

// A $1 ceiling and $0.30 a turn for Terra, Sol and the judge: the first
// hand-off's crossing would land past 90% of the ceiling, the guard
// declines it, and the `else` exit returns the reviewed plan to the chat
// pause, kept active for the resume the notice names.
func TestCopilot_BudgetDeclinedHandoffKeepsThePlanAtTheChat(t *testing.T) {
	t.Parallel()
	wf, exec := copilotHandoffDrive(t, 0.30)
	if wf.Budget == nil {
		wf.Budget = &ir.Budget{}
	}
	wf.Budget.MaxCostUSD = 1.0
	s := tmpStore(t)
	const runID = "e2e-copi-handoff-budget"
	err := runtime.New(wf, s, exec).Run(context.Background(), runID, nil)
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected the chat pause after the declined hand-off, got: %v", err)
	}
	if got := exec.callCount("copi"); got != 1 {
		t.Errorf("copi ran %d times, want 1 — the hand-off must not have re-entered the cycle", got)
	}
	out, instructions := composeOutput(t, s, runID)
	if got, ok := out["implementation_active"].(bool); !ok || !got {
		t.Errorf("compose implementation_active = %#v, want the bool true (the plan waits for the resume)", out["implementation_active"])
	}
	if got := fmt.Sprint(out["implementation_plan"]); !strings.Contains(got, "inspect checkpoint") {
		t.Errorf("compose implementation_plan = %q, want the reviewed plan carried", got)
	}
	if _, ok := out["quick_replies"].([]any); !ok {
		t.Errorf("compose quick_replies = %#v, want an array, not text", out["quick_replies"])
	}
	if !strings.Contains(instructions, "budget du run") {
		t.Errorf("chat instructions = %q, want the budget notice", instructions)
	}
}

// The cycle's cap, lowered to one hand-off on the compiled program: the
// second hand-off finds the loop spent, the `when` on the counter fires,
// and the plan is dropped at the chat pause so the next turn starts fresh.
func TestCopilot_SpentHandoffCycleDropsThePlanAtTheChat(t *testing.T) {
	t.Parallel()
	wf, exec := copilotHandoffDrive(t, 0.001)
	wf.Loops["terra_plan_execution_cycle"].MaxIterations = 1
	s := tmpStore(t)
	const runID = "e2e-copi-handoff-cap"
	err := runtime.New(wf, s, exec).Run(context.Background(), runID, nil)
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected the chat pause after the spent cycle, got: %v", err)
	}
	if got := exec.callCount("copi"); got != 2 {
		t.Errorf("copi ran %d times, want 2 (the opening turn + the one granted hand-off)", got)
	}
	out, instructions := composeOutput(t, s, runID)
	if got, ok := out["implementation_active"].(bool); !ok || got {
		t.Errorf("compose implementation_active = %#v, want the bool false (the plan is dropped)", out["implementation_active"])
	}
	if got := fmt.Sprint(out["implementation_plan"]); got != "" {
		t.Errorf("compose implementation_plan = %q, want empty (the next turn starts fresh)", got)
	}
	if _, ok := out["quick_replies"].([]any); !ok {
		t.Errorf("compose quick_replies = %#v, want an array, not text", out["quick_replies"])
	}
	if !strings.Contains(instructions, "limite de tours") {
		t.Errorf("chat instructions = %q, want the cap notice", instructions)
	}
}
