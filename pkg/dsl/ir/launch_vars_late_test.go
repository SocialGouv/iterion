package ir

import "testing"

// Round-1 verifier probes — reproduce the reviewer findings by execution
// before any fix. Delete none silently: the survivors become the witnesses
// they name.

// Finding 1 (high): an override whose text consults an engine-supplied name
// must not leave the declared default's answer standing in the view. The
// engine expands override texts through varExpandFn, which ANSWERS the five
// engine names from run state (engine_resolve.go) — a value this process
// cannot reproduce — so the name must be absent (undecided), never the
// default's answer.
func TestVerifyR1EngineNamedOverrideDoesNotStandInTheDefault(t *testing.T) {
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarString, HasDefault: true, Default: "claw"},
	}}
	view, late := LaunchVarsView[string](w, map[string]string{"b": "${PROJECT_DIR}/scratch"})
	if got, present := view["b"]; present {
		t.Errorf("view[b] = %q, want absent — the run expands the override to the engine's workDir path; answering the default certifies a route the run never resolves", got)
	}
	if late["b"] {
		t.Errorf("late[b] = true, want absent — an absent name is undecided already; late is for names the view answers")
	}
	// Control at the default layer: the same engine-named text as the
	// declared default keeps the name out of the view.
	w2 := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarString, HasDefault: true, Default: "${PROJECT_DIR}/scratch"},
	}}
	view2, _ := LaunchVarsView[string](w2, nil)
	if _, present := view2["b"]; present {
		t.Errorf("control view[b] present — the default layer's own omission works; only the override layer leaks the default's answer")
	}
}

// Finding 2 (high) + 9 (medium): the late set is the FINAL reading's, not the
// defaults' — a launch override that replaces an env-consulting default with a
// literal leaves nothing for the reading process to decide, so the stale mark
// must go; an override whose own text consults the env (or fails coercion)
// stays late.
func TestVerifyR1LateSetIsTheFinalReading(t *testing.T) {
	w := &Workflow{Vars: map[string]*Var{
		"b": {Name: "b", Type: VarString, HasDefault: true, Default: "${E2RV_STALE:-claude_code}"},
	}}
	view, late := LaunchVarsView[string](w, map[string]string{"b": "claw"})
	if got := view["b"]; got != "claw" {
		t.Fatalf("view[b] = %v, want claw", got)
	}
	if late["b"] {
		t.Errorf("late[b] = true, want absent — the override replaced the default with a literal the run reads unchanged; the reading process decided nothing")
	}
	// Control 1: the same default with NO override stays late (the env decided it).
	_, late2 := LaunchVarsView[string](w, nil)
	if !late2["b"] {
		t.Errorf("control late[b] = false, want true — the env-funded default is the reading process's word")
	}
	// Control 2: an override whose text consults the env stays late.
	t.Setenv("E2RV_STALE_OVERRIDE", "kimi")
	_, late3 := LaunchVarsView[string](w, map[string]string{"b": "${E2RV_STALE_OVERRIDE}"})
	if !late3["b"] {
		t.Errorf("late[b] = false, want true — the override text consults the environment")
	}
	// Control 3: an override whose text consults an engine-supplied name is
	// absent (undecided), not late-and-answered.
	_, late4 := LaunchVarsView[string](w, map[string]string{"b": "${PROJECT_DIR}/scratch"})
	if _, answered := late4["b"]; answered {
		t.Errorf("late[b] present for an engine-named override — the name is absent from the view; late must describe answered names only")
	}
}

// Finding 3 (low): LaunchVarsView guards a nil workflow like its sibling
// DeclaredVarsOnly does.
func TestVerifyR1NewExportsGuardNilWorkflow(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("LaunchVarsView panicked on a nil workflow: %v", r)
		}
	}()
	view, late := LaunchVarsView[string](nil, map[string]string{"b": "x"})
	if view != nil || late != nil {
		t.Errorf("LaunchVarsView(nil, …) = %v, %v, want nil, nil", view, late)
	}
	view2 := DeclaredVarsOnly(nil, map[string]string{"b": "x"})
	if view2 != nil {
		t.Errorf("DeclaredVarsOnly(nil, …) = %v, want nil", view2)
	}
}
