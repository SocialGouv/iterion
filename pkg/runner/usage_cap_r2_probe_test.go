package runner

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The pre-flight's supervisor walk (preflightSupervisors) must read the same
// launch reading the wire walk and the edge filter read — the run's vars as
// the spawn path resolves them (launchReading folded through the dispatch
// view's declared defaults), not msg.Vars raw. A resume publication carries
// no message vars; a fresh launch with no vars still folds declared defaults
// at spawn (engine.ResolveVars). A supervisor whose model spec stays
// templated here is never judged for the cap, yet spends on every execution
// in-process via claw, which reports no readings — nothing stops it in flight.
//
// Mutation witness: reverting preflightSupervisors to
// supervise.SpecsFromWorkflow(wf, msg.Vars, nil) reds both tests on their own
// park assertions.
func TestR2Probe_SupervisorTemplatedModelOnAResume(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	resets := time.Now().UTC().Add(30 * time.Hour).Truncate(time.Second)
	hard := usagecap.NewMemStore()
	weekCapped(t, hard, platformKey("fp-forfait"), resets)
	ctx := forfaitBesidePinnedZAI()

	wf := chainWorkflow(agentRoute("impl", delegate.BackendCodex, "", "openai/gpt-6"))
	wf.Vars = map[string]*ir.Var{"m": varString("m", "openai/gpt-6")}
	wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "{{vars.m}}"}}

	// Fresh launch, m pinned onto the wire: the supervisor is judged, parked.
	r := capRunner(capTestPolicy(), hard, &capStatusStore{})
	if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-sup", Vars: map[string]any{"m": "anthropic/claude-opus-5-5"}, Supervisors: ""}, iterlog.Nop()); err == nil {
		t.Fatal("fresh launch with a supervisor pinned onto the capped wire was NOT parked — precondition broken")
	}
	// The same run as a resume publication: the stored input carries the same m.
	rs := &inputStore{run: &store.Run{ID: "run-sup", Status: store.RunStatusFailedResumable, Inputs: map[string]any{"m": "anthropic/claude-opus-5-5"}}}
	r2 := capRunner(capTestPolicy(), hard, rs)
	err := r2.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-sup", Resume: &queue.ResumeSpec{}, Supervisors: ""}, iterlog.Nop())
	if err == nil {
		t.Error("RESUME whose stored input pins the supervisor onto the capped wire was NOT parked — the supervisor walk left msg.Vars raw instead of the shared launch reading; the supervisor spends on every execution in-process (claw reports no readings) and nothing stops it in flight")
	}
}

// The wider shape: a supervisor model templated over a DECLARED DEFAULT that
// lands on the wire, launched with no vars at all. Spawn resolves it
// (engine.ResolveVars folds the declared defaults); the supervisor walk must
// fold them too.
func TestR2Probe_SupervisorTemplatedOverDefaultFreshLaunch(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	hard := usagecap.NewMemStore()
	weekCapped(t, hard, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	ctx := forfaitBesidePinnedZAI()

	wf := chainWorkflow(agentRoute("impl", delegate.BackendCodex, "", "openai/gpt-6"))
	wf.Vars = map[string]*ir.Var{"m": varString("m", "anthropic/claude-opus-5-5")}
	wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "{{vars.m}}"}}

	r := capRunner(capTestPolicy(), hard, &capStatusStore{})
	err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-sup2", Supervisors: ""}, iterlog.Nop())
	if err == nil {
		t.Error("fresh launch whose supervisor model is templated over a declared default on the capped wire was NOT parked — spawn folds engine.ResolveVars and the nodes' walks fold the declared defaults; the supervisor walk must too")
	}
}

// The conservative arms the fix must not close: a resume whose inputs cannot
// be read keeps the var-less posture (no fold, the supervisor walk skips the
// spec it cannot resolve), and a launch that pins the supervisor OFF the wire
// stays un-parked.
func TestR2Probe_SupervisorWalkUnreadableInputsAndOffWireLaunch(t *testing.T) {
	blankAnthropicWireEnv(t)
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	t.Setenv("ITERION_SUPERVISORS", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	hard := usagecap.NewMemStore()
	weekCapped(t, hard, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	ctx := forfaitBesidePinnedZAI()

	wf := chainWorkflow(agentRoute("impl", delegate.BackendCodex, "", "openai/gpt-6"))
	wf.Vars = map[string]*ir.Var{"m": varString("m", "openai/gpt-6")}
	wf.Supervisors = []*ir.Supervisor{{Name: "pacer", Model: "{{vars.m}}"}}

	// Resume publication, inputs unreadable: the walk keeps the var-less
	// posture — the spec stays templated, the supervisor is not judged, no
	// park from the supervisor route (the whole-graph answer would judge the
	// templated spec only through ResolveModel failing, so nothing parks).
	r := capRunner(capTestPolicy(), hard, &inputStore{err: errors.New("mongo down")})
	if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-sup3", Resume: &queue.ResumeSpec{}, Supervisors: ""}, iterlog.Nop()); err != nil {
		t.Errorf("resume with unreadable inputs parked on a supervisor route the walk could not resolve: %v", err)
	}
	// Fresh launch pinning the supervisor OFF the wire: no park.
	r2 := capRunner(capTestPolicy(), hard, &capStatusStore{})
	if err := r2.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-sup4", Vars: map[string]any{"m": "zai/glm-5"}, Supervisors: ""}, iterlog.Nop()); err != nil {
		t.Errorf("launch with the supervisor pinned OFF the wire parked: %v", err)
	}
}
