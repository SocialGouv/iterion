package runner

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// Round-1 witnesses for the pre-flight's launch reading (ADR-129 §4): a
// resume publication reads the run's stored inputs into the WIRE view too —
// the same launch the first walk read, not the declared defaults; an
// engine-named launch value never reads as the default's answer; and a
// literal message value over an env-consulting default decides the route.

// A resume publication carries no vars; the attempt it continues resumes
// from the checkpoint the stored inputs built. When those inputs move a
// templated route ONTO the capped wire while the declared default reads OFF
// it, the wire walk must still see the route — folding the declared defaults
// alone answers no-route and lets the run start into its first capped call.
// Mutant: the wire view built from msg.Vars raw (no launchReading) → the
// resume arm reds on its park assertion.
func TestUsageCapPreflight_AResumeWhoseStoredInputsRideTheWireIsParked(t *testing.T) {
	blankAnthropicWireEnv(t)
	hard := usagecap.NewMemStore()
	weekCapped(t, hard, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	ctx := forfaitBesidePinnedKeys()

	templated := func(def string) *ir.Workflow {
		wf := chainWorkflow(agentRoute("impl", "{{vars.b}}", "openai", "openai/gpt-6"))
		wf.Vars = map[string]*ir.Var{"b": varString("b", def)}
		return wf
	}
	// The launch, vars in hand: the route reads onto the wire — parked.
	r := capRunner(capTestPolicy(), hard, &capStatusStore{})
	if err := r.usageCapPreflight(ctx, templated("claw"), &queue.RunMessage{RunID: "run-wire", Vars: map[string]any{"b": "claude_code"}}, iterlog.Nop()); err == nil {
		t.Fatal("launch with b=claude_code was not parked on the capped wire — precondition broken")
	}
	// The resume: no vars on the publication, the stored input says claude_code.
	rs := &inputStore{run: &store.Run{ID: "run-wire", Status: store.RunStatusFailedResumable, Inputs: map[string]any{"b": "claude_code"}}}
	r2 := capRunner(capTestPolicy(), hard, rs)
	if err := r2.usageCapPreflight(ctx, templated("claw"), &queue.RunMessage{RunID: "run-wire", Resume: &queue.ResumeSpec{}}, iterlog.Nop()); err == nil {
		t.Error("resume whose stored input resolves the route onto the capped wire was NOT parked — the wire walk folded the declared defaults alone")
	}
	if rs.load != 1 {
		t.Errorf("LoadRun called %d time(s), want 1 — the stored inputs are read once, for the wire view and the edge walk together", rs.load)
	}
	// The twin stays: a resume whose stored input reads OFF the wire starts.
	rs3 := &inputStore{run: &store.Run{ID: "run-wire", Status: store.RunStatusFailedResumable, Inputs: map[string]any{"b": "claw"}}}
	r3 := capRunner(capTestPolicy(), hard, rs3)
	if err := r3.usageCapPreflight(ctx, templated("claude_code"), &queue.RunMessage{RunID: "run-wire", Resume: &queue.ResumeSpec{}}, iterlog.Nop()); err != nil {
		t.Errorf("resume with b=claw parked: %v — the stored input is the run's own answer, and it reads off the capped wire", err)
	}
}

// A launch value whose text names engine-supplied state (${PROJECT_DIR} —
// the engine's varExpandFn answers it at dispatch) never reads as the
// declared default's answer: undecided is the only honest reading, and
// undecided parks under the hard cap.
func TestUsageCapPreflight_EngineNamedMsgValueNeverReadsAsTheDefault(t *testing.T) {
	blankAnthropicWireEnv(t)
	hard := usagecap.NewMemStore()
	weekCapped(t, hard, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	ctx := forfaitBesidePinnedKeys()

	wf := chainWorkflow(agentRoute("impl", "{{vars.cfg.backend}}", "openai", "{{vars.cfg.model}}"))
	wf.Vars = map[string]*ir.Var{
		"cfg": {Name: "cfg", Type: ir.VarJSON, HasDefault: true, Default: `{"backend":"claw","model":"openai_compatible/gw-1"}`},
	}
	r := capRunner(capTestPolicy(), hard, &capStatusStore{})
	msgVars := map[string]any{"cfg": `{"backend":"claude_code","model":"claude-opus-5-5","note":"${PROJECT_DIR}"}`}
	if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-eng", Vars: msgVars}, iterlog.Nop()); err == nil {
		t.Error("the pre-flight read the default's claw route for a run whose own cfg resolves claude_code — it started a run whose backend it never read")
	}
}

// A literal value arriving on the run message is the run's own decision, over
// a default whose text consults the environment: the route reads off the
// wire and the run starts. Parking it is the #668 freeze in the pinning
// corner — the launch already answered what the default only consulted.
func TestUsageCapPreflight_LiteralMsgValueOverEnvDefaultDecides(t *testing.T) {
	blankAnthropicWireEnv(t)
	hard := usagecap.NewMemStore()
	weekCapped(t, hard, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	ctx := forfaitBesidePinnedKeys()

	t.Setenv("E2RV_LIT_OVER_ENV", "")
	wf := chainWorkflow(agentRoute("impl", "{{vars.b}}", "openai", "openai/gpt-6"))
	wf.Vars = map[string]*ir.Var{"b": varString("b", "${E2RV_LIT_OVER_ENV:-claw}")}
	r := capRunner(capTestPolicy(), hard, &capStatusStore{})
	if err := r.usageCapPreflight(ctx, wf, &queue.RunMessage{RunID: "run-lit", Vars: map[string]any{"b": "claw"}}, iterlog.Nop()); err != nil {
		t.Errorf("parked although msg.Vars carries the literal claw value the run dispatches: %v", err)
	}
}
