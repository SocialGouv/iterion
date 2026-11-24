package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// inputGatedBot declares one var and pauses on a human entry node (no LLM,
// no credentials), so Launch tests can drive the input path and read the
// persisted run doc without a backend.
const inputGatedBot = `
prompt say:
  hi

human gate:
  instructions: say
  interaction: human

workflow input_demo:
  vars:
    engine: string = "cpu"
  worktree: none
  repo_devbox: off
  entry: gate
  gate -> done
`

func writeInputBot(t *testing.T, dir string) string {
	t.Helper()
	botPath := filepath.Join(dir, "input_demo.bot")
	if err := os.WriteFile(botPath, []byte(inputGatedBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	return botPath
}

// TestLaunch_RefusesAnInputThatNamesNoVar pins #1757: a launch input that
// matches no declared var is refused, naming the key and the declared set
// — never dropped in silence, where the run executes on defaults while
// the operator believes it was parameterised (measured in #1757: a bot
// ran 2 h 46 min on its first environment while its operator had asked
// for the second).
func TestLaunch_RefusesAnInputThatNamesNoVar(t *testing.T) {
	dir := t.TempDir()
	botPath := writeInputBot(t, dir)
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	_, err = svc.Launch(context.Background(), LaunchSpec{
		FilePath: botPath,
		Vars:     map[string]string{"engin": "pg"},
	})
	if err == nil {
		t.Fatal("launch accepted an input that names no declared var, want refusal")
	}
	if !strings.Contains(err.Error(), "engin") || !strings.Contains(err.Error(), "engine") {
		t.Fatalf("error = %v, want it to name the unknown key AND the declared var", err)
	}

	// The refusal happens before any run doc is created: nothing to clean
	// up, nothing to reconcile.
	runs, err := svc.ListCtx(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("ListCtx: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("a refused launch left %d run doc(s) behind", len(runs))
	}
}

// TestLaunch_AllowUnknownInputsRidesTheKey is the opt-out twin (#1757):
// with spec.AllowUnknownInputs the unknown key rides the launch — the
// forwarding channel an undeclared payload key rides to a subbot through
// {{input.*}} (C149) — never silently: the refusal's absence is the
// operator's explicit word, carried in the spec.
func TestLaunch_AllowUnknownInputsRidesTheKey(t *testing.T) {
	dir := t.TempDir()
	botPath := writeInputBot(t, dir)
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	res, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath:           botPath,
		Vars:               map[string]string{"engin": "pg"},
		AllowUnknownInputs: true,
	})
	if err != nil {
		t.Fatalf("opted-out launch refused: %v", err)
	}
	select {
	case <-res.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("run goroutine did not exit (expected immediate human pause)")
	}
}

// TestLaunch_RidesAnEngineInjectedInput pins #2416: a strict launch (no
// opt-out) accepts an input whose name is engine-injected — an env-shaped
// bot-var dial a sovereign pool's provision stamps into launch_vars
// (ITERION_VIBE_*), answered by the engine's expansion rather than by a DSL
// var — while an infra name and a plain typo keep the #1757 refusal.
func TestLaunch_RidesAnEngineInjectedInput(t *testing.T) {
	dir := t.TempDir()
	botPath := writeInputBot(t, dir)
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath: botPath,
		Vars:     map[string]string{"ITERION_VIBE_BACKEND_EMIT": "claw", "ITERION_VIBE_PROVIDER_EMIT": "openai_compatible"},
	})
	if err != nil {
		t.Fatalf("launch with engine-injected inputs refused: %v", err)
	}
	select {
	case <-res.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("run goroutine did not exit (expected immediate human pause)")
	}
	r, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got := r.Inputs["ITERION_VIBE_BACKEND_EMIT"]; got != "claw" {
		t.Fatalf("run inputs = %v, want the injected dial carried on the doc", r.Inputs)
	}

	// An infra name is not engine-injected: nothing answers it at run time,
	// so the refusal still owes the caller the truth.
	_, err = svc.Launch(context.Background(), LaunchSpec{
		FilePath: botPath,
		Vars:     map[string]string{"ITERION_MONGO_URL": "mongodb://elsewhere"},
	})
	if err == nil {
		t.Fatal("launch accepted an infra-shaped input, want the #1757 refusal")
	}

	// Mixed: the typo still refuses, naming itself — the injected names ride.
	_, err = svc.Launch(context.Background(), LaunchSpec{
		FilePath: botPath,
		Vars:     map[string]string{"engin": "pg", "ITERION_VIBE_BACKEND_EMIT": "claw"},
	})
	if err == nil || !strings.Contains(err.Error(), "engin") {
		t.Fatalf("error = %v, want the refusal naming the typo", err)
	}
	if strings.Contains(err.Error(), "ITERION_VIBE_BACKEND_EMIT") {
		t.Fatalf("error = %v, must not name the engine-injected dial as unknown", err)
	}
}

// TestLaunch_TakesADeclaredInput is the positive twin: a declared var
// launches and lands on the run doc, so the refusal above is a refusal of
// the unknown only, not of inputs altogether.
func TestLaunch_TakesADeclaredInput(t *testing.T) {
	dir := t.TempDir()
	botPath := writeInputBot(t, dir)
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath: botPath,
		Vars:     map[string]string{"engine": "pg"},
	})
	if err != nil {
		t.Fatalf("Launch with a declared input refused: %v", err)
	}
	select {
	case <-res.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("run goroutine did not exit (expected immediate human pause)")
	}
	r, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got := r.Inputs["engine"]; got != "pg" {
		t.Fatalf("run inputs = %v, want engine=pg on the doc", r.Inputs)
	}
}
