package runner

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/supervise"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The supervisor walk resolves a pin only against the vars the reading
// DECIDED (view.DecidedVars). A declared default whose text consults the
// environment is LATE: the value view.Vars carries is the reading process's
// own env answer, and the view's own field reading keeps the span as written
// for exactly that reason (ADR-129 §4, rule 1) — resolving the pin against
// view.Vars parks, or spares, the run on an answer the run may never
// resolve. The pin stays templated under the decided map,
// model.ParseModelSpec refuses it in supervisorKey, the route meters
// nothing: the walk answers "not metered" on an unresolvable reading, and
// stays off the park on a name the view itself keeps undecided.
//
// Mutation witness: reverting preflightSupervisors' argument to view.Vars
// reds the capped-env arm on its own park assertion.
func TestR3Probe_LateVarPinNeverParksOnTheReadingEnv(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	hard := usagecap.NewMemStore()
	weekCapped(t, hard, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	ctx := forfaitBesidePinnedZAI()

	wf := chainWorkflow(agentRoute("impl", delegate.BackendCodex, "", "openai/gpt-6"))
	wf.Vars = map[string]*ir.Var{"m": varString("m", "${ITERION_R3_PROBE_MODEL:-openai/gpt-6}")}
	wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "{{vars.m}}"}}

	// The reading, with the env answer on the wire: the default resolves to
	// it AND the name is late, while the field reading keeps the span as
	// written — the asymmetry the walk must not decide across. view.Vars
	// holds the env answer, which is where a park over it would come from.
	t.Setenv("ITERION_R3_PROBE_MODEL", "anthropic/claude-opus-5-5")
	view := model.RouteViewForDispatch(wf, model.ModelOverrides{}, nil, map[string]any(nil))
	if got := view.Vars["m"]; got != "anthropic/claude-opus-5-5" {
		t.Fatalf("the reading did not put the env answer in the view (got %q) — the probe does not exercise the divergence", got)
	}
	if !view.LateVars["m"] {
		t.Fatal("an env-consulting default is not late — the probe does not exercise the divergence")
	}
	if got := model.ResolveRoutingFieldWith(view.Vars, view.LateVars, "{{vars.m}}"); got != "{{vars.m}}" {
		t.Fatalf("the field reading resolved a late name to %q — the view's own doctrine broken", got)
	}
	specs := supervise.SpecsFromWorkflow(wf, view.Vars, nil)
	if len(specs) != 1 || specs[0].Model != "anthropic/claude-opus-5-5" {
		t.Fatalf("the raw view resolved no pin (%v) — the probe does not exercise the divergence", specs)
	}

	// The capped-env arm must NOT park: the run may resolve the pin to
	// anything, and this process's env answer is not the run's.
	r := capRunner(capTestPolicy(), hard, &capStatusStore{})
	if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-r3", Supervisors: ""}, iterlog.Nop()); err != nil {
		t.Errorf("parked on a supervisor pin whose only reading is a LATE var — the park rides the reading process's environment (the run may resolve the pin off the wire, and the view's own field reading keeps the span undecided): %v", err)
	}

	// The env answer off the wire does not park either — the same late name;
	// whether the pin resolves to openai or stays templated, nothing meters
	// on the capped wire. A walk that "hardened" undecided pins into
	// always-on-the-wire reddens here.
	t.Setenv("ITERION_R3_PROBE_MODEL", "")
	if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-r3b", Supervisors: ""}, iterlog.Nop()); err != nil {
		t.Errorf("parked with the env answer off the wire: %v", err)
	}
}
