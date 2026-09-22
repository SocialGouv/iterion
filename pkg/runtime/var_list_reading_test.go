package runtime

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// listVarWorkflow declares one var of each list type with a text default,
// plus the scalar shapes the same reading must not disturb.
func listVarWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "wf",
		Entry: "entry",
		Vars: map[string]*ir.Var{
			"tags":    {Name: "tags", Type: ir.VarStringArray, HasDefault: true, Default: "a,b"},
			"dialled": {Name: "dialled", Type: ir.VarStringArray, HasDefault: true, Default: "${VAR_LIST_UNSET:-a,b}"},
			"langs":   {Name: "langs", Type: ir.VarJSON, HasDefault: true, Default: "[1, 2]"},
			"invalid": {Name: "invalid", Type: ir.VarJSON, HasDefault: true, Default: "[]"},
			"doc":     {Name: "doc", Type: ir.VarJSON, HasDefault: true, Default: `{"cost":"$5"}`},
			"mode":    {Name: "mode", Type: ir.VarString, HasDefault: true, Default: "fast"},
			"rounds":  {Name: "rounds", Type: ir.VarInt, HasDefault: true, Default: int64(3)},
		},
		Nodes: map[string]ir.Node{},
		Loops: map[string]*ir.Loop{},
	}
}

// #1285, stated as the property it asks for: the same TEXT gives the same
// VALUE whether it arrived as the `.bot`'s default or as an override.
// Before, `tags: string[] = "a,b"` started the run as the string "a,b"
// while `--var tags=a,b` started it as ["a","b"].
func TestResolveVars_DefaultAndOverrideReadTheSameText(t *testing.T) {
	eng := &Engine{workflow: listVarWorkflow()}

	fromDefaults := eng.resolveVars(nil)
	fromOverrides := eng.resolveVars(map[string]any{
		"tags":    "a,b",
		"dialled": "${VAR_LIST_UNSET:-a,b}",
		"langs":   "[1, 2]",
		"invalid": "[]",
		"doc":     `{"cost":"$5"}`,
		"mode":    "fast",
		"rounds":  "3",
	})

	for name := range eng.workflow.Vars {
		if !reflect.DeepEqual(fromDefaults[name], fromOverrides[name]) {
			t.Errorf("var %q: default seeded %#v, override seeded %#v — the same text must read the same",
				name, fromDefaults[name], fromOverrides[name])
		}
	}
}

// What each reading actually produces, pinned value by value: a property
// test that both sides satisfy by being equally wrong would prove nothing.
func TestResolveVars_ReadsEachDeclaredType(t *testing.T) {
	eng := &Engine{workflow: listVarWorkflow()}
	vars := eng.resolveVars(nil)

	want := map[string]any{
		"tags":    []any{"a", "b"},
		"dialled": []any{"a", "b"}, // expanded BEFORE the split
		"langs":   []any{float64(1), float64(2)},
		"invalid": []any{},
		"doc":     map[string]any{"cost": "$5"}, // a `$` inside a document is data
		"mode":    "fast",
		"rounds":  int64(3),
	}
	for name, w := range want {
		if !reflect.DeepEqual(vars[name], w) {
			t.Errorf("var %q = %#v, want %#v", name, vars[name], w)
		}
	}
}

// The entry floor seeds `{{input.<var>}}` at the entry node. It read the
// compiler's raw text while `{{vars.<var>}}` read a list, so one
// declaration had two values in one run.
func TestBuildNodeInput_EntryFloorCarriesTheResolvedVar(t *testing.T) {
	eng := &Engine{workflow: listVarWorkflow()}
	vars := eng.resolveVars(nil)

	input := eng.buildNodeInputRS("entry", resolveScope{vars: vars})

	for _, name := range []string{"tags", "langs", "invalid", "doc", "mode", "rounds"} {
		if !reflect.DeepEqual(input[name], vars[name]) {
			t.Errorf("entry floor %q = %#v, vars %q = %#v — one declaration, two values",
				name, input[name], name, vars[name])
		}
	}
}

// A launch value still wins over the floor.
func TestBuildNodeInput_EntryFloorYieldsToTheLaunchPayload(t *testing.T) {
	eng := &Engine{workflow: listVarWorkflow()}
	vars := eng.resolveVars(map[string]any{"tags": "x,y"})

	input := eng.buildNodeInputRS("entry", resolveScope{
		vars:      vars,
		runInputs: map[string]any{"tags": []any{"x", "y"}},
	})

	if want := []any{"x", "y"}; !reflect.DeepEqual(input["tags"], want) {
		t.Errorf("entry floor tags = %#v, want %#v", input["tags"], want)
	}
}

// The enum gate judges the value that flows into the run. With an
// expander of its own it refused `${MODE:-fast}` — expanded to "" by
// os.Expand, which reads the whole `MODE:-fast` as a variable name — on a
// var the run would then have started with "fast".
func TestValidateVarEnums_ReadsThroughTheRunsOwnReading(t *testing.T) {
	eng := &Engine{workflow: &ir.Workflow{Vars: map[string]*ir.Var{
		"mode": {Name: "mode", Type: ir.VarString, EnumValues: []string{"fast", "slow"}},
	}}}

	if err := eng.validateVarEnums(map[string]any{"mode": "${VAR_ENUM_UNSET:-fast}"}); err != nil {
		t.Errorf("the gate refused a value the run reads as %q: %v", "fast", err)
	}
	if got := eng.resolveVars(map[string]any{"mode": "${VAR_ENUM_UNSET:-fast}"})["mode"]; got != "fast" {
		t.Errorf("the run reads %#v — the gate and the run must agree", got)
	}
	if err := eng.validateVarEnums(map[string]any{"mode": "${VAR_ENUM_UNSET:-yolo}"}); err == nil {
		t.Error("the gate accepted a value outside the enum")
	}
}

// The repo-root foot-gun remap: a var explicitly set to the main checkout
// points agents at a tree whose `.git` is mounted but whose working files
// are not. It read one string per var, which a `string[]` or `json` var no
// longer is — the run reads its default and its override alike as a list
// (#1285), so the foot-gun walked straight past the guard.
func TestResolveVars_RemapsTheRepoRootInsideAList(t *testing.T) {
	eng := &Engine{
		repoRoot: "/repo",
		workDir:  "/repo/.iterion/worktrees/run-1",
		workflow: &ir.Workflow{
			Name: "wf",
			Vars: map[string]*ir.Var{
				"scan":  {Name: "scan", Type: ir.VarStringArray},
				"plain": {Name: "plain", Type: ir.VarString},
				"doc":   {Name: "doc", Type: ir.VarJSON},
			},
			Nodes: map[string]ir.Node{},
			Loops: map[string]*ir.Loop{},
		},
	}
	vars := eng.resolveVars(map[string]any{
		"scan":  "/repo,/repo/pkg",
		"plain": "/repo",
		// A `json` var is the shape most likely to carry a path, and it
		// carries it at depth.
		"doc": `{"roots":["/repo","untouched"],"nested":{"at":"/repo"}}`,
	})

	wt := "/repo/.iterion/worktrees/run-1"
	if want := []any{wt, "/repo/pkg"}; !reflect.DeepEqual(vars["scan"], want) {
		t.Errorf("string[] var = %#v, want %#v", vars["scan"], want)
	}
	if vars["plain"] != wt {
		t.Errorf("string var = %#v, want %q", vars["plain"], wt)
	}
	want := map[string]any{
		"roots":  []any{wt, "untouched"},
		"nested": map[string]any{"at": wt},
	}
	if !reflect.DeepEqual(vars["doc"], want) {
		t.Errorf("json var = %#v, want %#v", vars["doc"], want)
	}
}

// A `json` var is a document, so a data mapping reading `{{vars.cfg.on}}`
// reads its member — the same thing an expression and a tool body read
// from the same text. Answering the whole document here made one
// reference mean different things on the three surfaces that resolve it.
func TestResolveRef_DrillsIntoAJSONVar(t *testing.T) {
	eng := &Engine{workflow: &ir.Workflow{
		Name: "wf",
		Vars: map[string]*ir.Var{"cfg": {Name: "cfg", Type: ir.VarJSON, HasDefault: true, Default: `{"name":"alpha","on":true}`}},
	}}
	sc := resolveScope{vars: eng.resolveVars(nil)}

	refs, err := ir.ParseRefs("{{vars.cfg.name}} {{vars.cfg}} {{vars.cfg.nope}}")
	if err != nil {
		t.Fatal(err)
	}
	if got := eng.resolveRef(refs[0], sc); got != "alpha" {
		t.Errorf("vars.cfg.name = %#v, want %q", got, "alpha")
	}
	whole, ok := eng.resolveRef(refs[1], sc).(map[string]any)
	if !ok || whole["name"] != "alpha" {
		t.Errorf("vars.cfg = %#v, want the whole document", eng.resolveRef(refs[1], sc))
	}
	if got := eng.resolveRef(refs[2], sc); got != nil {
		t.Errorf("an absent member resolved to %#v, want nil", got)
	}
}
