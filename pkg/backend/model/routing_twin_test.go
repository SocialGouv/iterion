package model

import (
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The launch-time fallback screen drills `{{vars.cfg.<path>}}` into a json
// var's document with a TWIN of drillTemplatePath (ir.drillVarsView — the
// layering forbids the import). Twins drift silently: mutating one leaves
// the other's package green. This table runs one semantic surface through
// BOTH resolutions — the executor's own resolveRoutingField over the vars
// resolveVars would store, and the screen's LaunchBackendName over the same
// declared document — so a drill that changes on either side reddens here.
//
// wantScreen is the screen's decided backend ("" = undecided OR
// decided-empty); wantDispatch is the executor's expanded text, the span
// kept as written wherever the drill finds nothing.
func TestVarsDrill_ScreenAndDispatchAgree(t *testing.T) {
	cases := []struct {
		name         string
		doc          string
		path         string
		wantScreen   string
		wantDispatch string
	}{
		{"nested maps", `{"a":{"b":{"c":"claw"}}}`, "a.b.c", "claw", "claw"},
		{"map-in-list stops the drill", `{"a":[{"b":"claw"}]}`, "a.0.b", "", "{{vars.cfg.a.0.b}}"},
		{"numeric keys are just keys", `{"0":"claw"}`, "0", "claw", "claw"},
		{"a null leaf is empty, not missing", `{"a":null}`, "a", "", ""},
		{"a missing member keeps the span", `{"a":"claw"}`, "nope", "", "{{vars.cfg.nope}}"},
		{"a non-map segment keeps the span", `{"a":"claw"}`, "a.b", "", "{{vars.cfg.a.b}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field := "{{vars.cfg." + tc.path + "}}"
			// The screen's reading, over the declared document.
			w := &ir.Workflow{Vars: map[string]*ir.Var{
				"cfg": {Name: "cfg", Type: ir.VarJSON, HasDefault: true, Default: tc.doc},
			}}
			if got := ir.LaunchBackendName(w, nil, field); got != tc.wantScreen {
				t.Errorf("screen: LaunchBackendName(%s) = %q, want %q", field, got, tc.wantScreen)
			}
			// Dispatch's reading, over the vars resolveVars would store —
			// ResolveVarText is the shared reading of the var's text.
			v, err := ir.ResolveVarText(tc.doc, ir.VarJSON, os.Getenv)
			if err != nil {
				t.Fatalf("the fixture does not coerce: %v", err)
			}
			e := &ClawExecutor{}
			e.vars = map[string]any{"cfg": v}
			if got := e.resolveRoutingField(field); got != tc.wantDispatch {
				t.Errorf("dispatch: resolveRoutingField(%s) = %q, want %q", field, got, tc.wantDispatch)
			}
		})
	}
}
