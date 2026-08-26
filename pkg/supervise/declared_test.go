package supervise

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestMonitorSpecGrammarInSync pins the contract between the compiler's
// syntax check (ir.CheckMonitorSpec, which can only WARN at C191) and
// the runtime parser (ParseMonitorSpecs, which decides what actually
// arms). A spec the compiler accepts must parse, and a spec the
// compiler rejects must fail to parse — otherwise validate lies in one
// direction or the other.
func TestMonitorSpecGrammarInSync(t *testing.T) {
	specs := []string{
		"event_type=tool_error,tool_name=Bash",
		"text_contains=impossible",
		"node_id=campaign",
		"cost_gt=2.5",
		"event_type=budget_warning, cost_gt=10",
		"",
		"bogus_key=x",
		"cost_gt=abc",
		"no-equals-sign",
		"text_contains=a=b", // value containing '=' stays valid (Cut splits once)
		// A cost_gt that parses as a float but cannot constrain must be
		// refused on BOTH sides — NaN/negative used to slip through the
		// matcher's `> 0` branch into a match-everything wildcard.
		"cost_gt=NaN",
		"cost_gt=-1",
		"cost_gt=0",
		"cost_gt=+Inf",
		// Zero-field / empty-value specs arm a monitor that can never
		// fire; both sides refuse them.
		" ",
		",,,",
		"text_contains=",
		"cost_gt=1e3",
	}
	for _, spec := range specs {
		_, parseErr := ParseMonitorSpecs([]string{spec})
		checkErr := ir.CheckMonitorSpec(spec)
		if (parseErr == nil) != (checkErr == nil) {
			t.Errorf("grammar drift on %q: ParseMonitorSpecs err=%v, ir.CheckMonitorSpec err=%v", spec, parseErr, checkErr)
		}
	}
}

// TestSpecsFromWorkflow covers the IR→Spec conversion: prompt-body
// resolution, monitor parsing with per-monitor degradation (one bad
// spec must not drop its siblings), and knob passthrough.
func TestSpecsFromWorkflow(t *testing.T) {
	wf := &ir.Workflow{
		Prompts: map[string]*ir.Prompt{
			"policy": {Name: "policy", Body: "stay silent"},
		},
		Supervisors: []*ir.Supervisor{{
			Name:     "persy",
			Watches:  []string{"campaign"},
			System:   "policy",
			Cooldown: 5 * time.Minute,
			MaxEvals: 7,
			Monitors: []string{
				"text_contains=impossible",
				"bogus_key=x", // dropped, must not take the others with it
				"event_type=budget_warning",
			},
		}},
	}
	specs := SpecsFromWorkflow(wf, nil)
	if len(specs) != 1 {
		t.Fatalf("SpecsFromWorkflow returned %d specs; want 1", len(specs))
	}
	sp := specs[0]
	if sp.System != "stay silent" {
		t.Errorf("system prompt not resolved: %q", sp.System)
	}
	if sp.Cooldown != 5*time.Minute || sp.MaxEvals != 7 {
		t.Errorf("knobs not passed through: cooldown=%s max_evals=%d", sp.Cooldown, sp.MaxEvals)
	}
	if len(sp.Monitors) != 2 {
		t.Fatalf("want 2 parsed monitors (bad one dropped alone), got %d: %+v", len(sp.Monitors), sp.Monitors)
	}
	if sp.Monitors[0].TextContains != "impossible" || sp.Monitors[1].EventType != "budget_warning" {
		t.Errorf("monitors mis-parsed: %+v", sp.Monitors)
	}
}
