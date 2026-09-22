package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// matchingTestWorkflow declares the three shapes the pattern gate has to
// tell apart: a pattern alone, a pattern next to an enum, and a var with
// no constraint at all.
func matchingTestWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "matching_gate_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "a", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{
			// The shape #1350 is about: a value that must survive as one
			// shell word, with no leading dash.
			"agent": {
				Name:       "agent",
				Type:       ir.VarString,
				Matching:   `^[A-Za-z0-9._][A-Za-z0-9._-]*$|^$`,
				HasDefault: true,
				Default:    "",
			},
			// Both constraints on one var: the two are independent, so a
			// value inside the enum can still be off the pattern.
			"mode": {
				Name:       "mode",
				Type:       ir.VarString,
				EnumValues: []string{"fast", "slow-and-careful"},
				Matching:   `^[a-z]+$`,
			},
			"free": {Name: "free", Type: ir.VarString},
		},
		Loops: map[string]*ir.Loop{},
	}
}

// TestVarMatchingRefusesAnOperatorValueAtLaunch is the ticket's own case:
// `--var agent=" codeX --some-flag"` must not reach the run. Mutation that
// reddens it: drop the pattern arm from validateVarConstraints — the value
// is then accepted and the run walks.
func TestVarMatchingRefusesAnOperatorValueAtLaunch(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	eng := New(matchingTestWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()))

	err := eng.Run(ctx, "run-matching-bad", map[string]any{"agent": " codeX --some-flag"})
	if err == nil {
		t.Fatal("Run accepted a var value its declared pattern refuses")
	}
	// The refusal has to be actionable: the var, the offending value and
	// the pattern it failed. An operator reads this at the keyboard.
	for _, want := range []string{`var "agent"`, `" codeX --some-flag"`, `^[A-Za-z0-9._]`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err.Error(), want)
		}
	}
	r, lerr := s.LoadRun(ctx, "run-matching-bad")
	if lerr != nil {
		t.Fatalf("LoadRun: %v", lerr)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("run status = %s, want %s", r.Status, store.RunStatusFailed)
	}
	// Refused before any work: the point of checking at launch is that it
	// costs nothing, so no node may have started.
	evs, _ := s.LoadEvents(ctx, "run-matching-bad")
	if hasEventType(evs, store.EventNodeStarted) {
		t.Error("a node started even though the launch gate refused the run")
	}
}

// TestVarMatchingAcceptsAConformingValue is the control, and it is the one
// that reddens if the gate ever anchors or rewrites a pattern on the
// author's behalf: every value here is admissible as the pattern is
// written, including the empty string the `|^$` branch allows.
func TestVarMatchingAcceptsAConformingValue(t *testing.T) {
	ctx := context.Background()
	for _, value := range []string{"codex", "claude_code", "a.b-c", ""} {
		s := tmpStore(t)
		eng := New(matchingTestWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()))
		runID := "run-matching-ok-" + value
		if err := eng.Run(ctx, runID, map[string]any{"agent": value}); err != nil {
			t.Fatalf("value %q satisfies the pattern but Run failed: %v", value, err)
		}
		r, _ := s.LoadRun(ctx, runID)
		if r.Status != store.RunStatusFinished {
			t.Errorf("value %q: run status = %s, want finished", value, r.Status)
		}
	}
}

// TestVarMatchingIsCheckedOnTheValueThatFlowsIntoTheRun pins the two halves
// of one guarantee: the gate judges the ${...}-EXPANDED value, and that is
// the same value resolveVars puts into the run.
//
// The fixture is arranged so the two readings disagree — the raw text
// `${ITERION_TEST_MATCH_AGENT}` is refused by the pattern (it carries `$`,
// `{`, `}`) while its expansion is admitted. A gate reading the raw text
// therefore refuses a launch the run would have served, and a gate reading
// a differently-expanded value would report a value nobody can find in the
// run. Mutation that reddens it: check `s` instead of the expanded value in
// validateVarConstraints, or change one side of expandVarOverride.
func TestVarMatchingIsCheckedOnTheValueThatFlowsIntoTheRun(t *testing.T) {
	eng := New(matchingTestWorkflow(), tmpStore(t), newStubExecutor(), WithWorkDir(t.TempDir()))

	t.Setenv("ITERION_TEST_MATCH_AGENT", "codex")
	inputs := map[string]any{"agent": "${ITERION_TEST_MATCH_AGENT}"}
	if err := eng.validateVarConstraints(inputs); err != nil {
		t.Fatalf("the gate must judge the expanded value, not the raw text: %v", err)
	}
	// The other half: what the gate admitted is what the run receives.
	if got := eng.resolveVars(inputs)["agent"]; got != "codex" {
		t.Fatalf("resolveVars gave %q; the gate judged the expansion %q — the two readers disagree", got, "codex")
	}

	t.Setenv("ITERION_TEST_MATCH_AGENT", "-codex")
	err := eng.validateVarConstraints(inputs)
	if err == nil {
		t.Fatal("an expansion that violates the pattern must be refused")
	}
	// The message quotes the EXPANDED value: an operator told that
	// `${VAR}` is invalid learns nothing.
	if !strings.Contains(err.Error(), `"-codex"`) {
		t.Errorf("refusal %q must quote the expanded value, not the reference", err.Error())
	}
}

// TestEnumAndMatchingAreBothEnforced: the two constraints are independent
// conjuncts. "slow-and-careful" is a declared enum value AND violates the
// var's pattern, so a gate that returns after the enum check lets it
// through. Mutation that reddens it: `continue` after the enum arm.
func TestEnumAndMatchingAreBothEnforced(t *testing.T) {
	eng := New(matchingTestWorkflow(), tmpStore(t), newStubExecutor(), WithWorkDir(t.TempDir()))

	if err := eng.validateVarConstraints(map[string]any{"mode": "fast"}); err != nil {
		t.Fatalf("a value satisfying both constraints was refused: %v", err)
	}
	err := eng.validateVarConstraints(map[string]any{"mode": "slow-and-careful"})
	if err == nil {
		t.Fatal("a value inside the enum but off the pattern must still be refused")
	}
	if !strings.Contains(err.Error(), "pattern") {
		t.Errorf("refusal %q must say which constraint failed", err.Error())
	}
	// The mirror case: off the enum, on the pattern.
	err = eng.validateVarConstraints(map[string]any{"mode": "quick"})
	if err == nil {
		t.Fatal("a value matching the pattern but outside the enum must still be refused")
	}
	if !strings.Contains(err.Error(), "allowed values") {
		t.Errorf("refusal %q must say which constraint failed", err.Error())
	}

	// A non-string can satisfy neither, and both reasons are reported
	// rather than the first one found.
	err = eng.validateVarConstraints(map[string]any{"mode": 5})
	if err == nil {
		t.Fatal("a non-string value on a constrained string var must be refused")
	}
	for _, want := range []string{"allowed values", "pattern"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q omits the %q constraint", err.Error(), want)
		}
	}
}

// TestUnconstrainedVarsAreUntouchedByTheMatchingGate guards the other
// direction: adding the pattern arm must not start refusing values on vars
// that declare nothing. Without it, a widened predicate is invisible.
func TestUnconstrainedVarsAreUntouchedByTheMatchingGate(t *testing.T) {
	eng := New(matchingTestWorkflow(), tmpStore(t), newStubExecutor(), WithWorkDir(t.TempDir()))
	inputs := map[string]any{"free": " anything --at all ", "undeclared": "x"}
	if err := eng.validateVarConstraints(inputs); err != nil {
		t.Errorf("an unconstrained var must accept any value, got %v", err)
	}
}

// TestResumeDoesNotReRefuseStoredValuesUnderATightenedPattern is the
// ticket's second design question, locked: the constraint is checked on
// values an operator supplies, at the moment they are supplied — never on
// values re-read from the store. A run admitted at launch stays resumable
// when its declaration is tightened afterwards.
//
// Mutation that reddens it: call validateVarConstraints from
// resumeFromPause / resumeFromFailure (or from resolveVars, which both
// resume paths cross) — the resume then fails on a value that was legal
// when the operator typed it.
//
// Scope note: this locks "already-accepted stored values are not
// re-refused", NOT "the resume path never validates anything". Closing the
// fork hole (operator-supplied NewInputs reaching a run through Resume)
// must stay possible.
//
// The engines carry no workflow hash, so the source-change guard
// (checkWorkflowHash) stays out of the way — in the CLI a tightened source
// additionally needs --force, which is an orthogonal guard, not this one.
func TestResumeDoesNotReRefuseStoredValuesUnderATightenedPattern(t *testing.T) {
	ctx := context.Background()
	const runID = "run-matching-resume"
	s := tmpStore(t)

	loose := matchingTestWorkflow()
	loose.Nodes["a"] = &ir.HumanNode{
		BaseNode:          ir.BaseNode{ID: "a"},
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
		Publish:           "approval",
	}
	// Admitted at launch: "legacy_agent" satisfies the pattern as declared.
	eng := New(loose, s, newStubExecutor(), WithWorkDir(t.TempDir()))
	if err := eng.Run(ctx, runID, map[string]any{"agent": "legacy_agent"}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}

	// The declaration is tightened afterwards — underscores are out.
	tightened := matchingTestWorkflow()
	tightened.Nodes["a"] = loose.Nodes["a"]
	tightened.Vars["agent"].Matching = `^[a-z]+$`
	if ok, _ := ir.ValueMatchesPattern(tightened.Vars["agent"].Matching, "legacy_agent"); ok {
		t.Fatal("fixture is inert: the tightened pattern still admits the stored value")
	}

	eng2 := New(tightened, s, newStubExecutor(), WithWorkDir(t.TempDir()))
	if err := eng2.Resume(ctx, runID, map[string]any{"decision": "approve"}); err != nil {
		t.Fatalf("a stored value admitted at launch must stay resumable after the pattern is tightened: %v", err)
	}
	r, _ := s.LoadRun(ctx, runID)
	if r.Status != store.RunStatusFinished {
		t.Errorf("run status = %s, want finished", r.Status)
	}
}
