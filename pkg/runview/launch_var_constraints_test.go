package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func constrainedWorkflow() *ir.Workflow {
	return &ir.Workflow{Name: "w", Vars: map[string]*ir.Var{
		"mode": {Name: "mode", Type: ir.VarString, EnumValues: []string{"fast", "slow"}},
		"tag":  {Name: "tag", Type: ir.VarString, Matching: "^[a-z]+$"},
		"both": {Name: "both", Type: ir.VarString, EnumValues: []string{"ok", "OK"}, Matching: "^[a-z]+$"},
		"free": {Name: "free", Type: ir.VarString},
	}}
}

// The gate refuses, in the engine's words, a literal value outside either
// constraint — both judged, both reported — and nothing else: a value inside
// them, a var without a constraint, a key no var declares.
func TestGateLaunchVars_RefusesALiteralOutsideItsConstraint(t *testing.T) {
	wf := constrainedWorkflow()
	for _, tc := range []struct {
		inputs map[string]any
		want   []string
	}{
		{map[string]any{"mode": "turbo"}, []string{`var "mode"`, `"turbo"`, `"fast", "slow"`}},
		{map[string]any{"tag": "two words"}, []string{`var "tag"`, `"two words"`, `"^[a-z]+$"`}},
		{map[string]any{"both": "OK"}, []string{`var "both"`, `"OK"`, `does not match the declared pattern`}},
		{map[string]any{"mode": ""}, []string{`var "mode"`, `value ""`}},
	} {
		err := GateLaunchVars(wf, tc.inputs)
		if !errors.Is(err, ErrLaunchVarsRefused) {
			t.Fatalf("%v: err = %v, want ErrLaunchVarsRefused", tc.inputs, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%v: refusal %q does not name %s", tc.inputs, err, w)
			}
		}
	}
	for _, ok := range []map[string]any{
		{"mode": "fast", "tag": "abc", "both": "ok"},
		{"free": "${anything}"},
		{"undeclared": "turbo"},
		nil,
	} {
		if err := GateLaunchVars(wf, ok); err != nil {
			t.Errorf("%v: refused a value the engine admits: %v", ok, err)
		}
	}
}

// A value that reads the environment is the engine's to judge, where that
// environment exists: refusing `${MODE:-turbo}` here would refuse a launch
// whose runner sets MODE=fast. The gate must never refuse what the engine
// admits.
func TestGateLaunchVars_LeavesAnEnvironmentReadToTheEngine(t *testing.T) {
	wf := constrainedWorkflow()
	for _, v := range []string{"${MODE:-turbo}", "${MODE}", "$MODE", "${PROJECT_DIR}/x"} {
		if err := GateLaunchVars(wf, map[string]any{"mode": v, "tag": v}); err != nil {
			t.Errorf("%q: judged a value only the engine's environment can read: %v", v, err)
		}
	}
}

// The local in-process launch refuses on the request, before a run document
// exists — not from the engine a moment after the launch was accepted.
func TestLaunch_RefusesAVarOutsideItsConstraintBeforeTheRunExists(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "gated.bot")
	src := strings.Replace(inputGatedBot, `engine: string = "cpu"`, `engine: string [enum: "cpu", "gpu"] = "cpu"`, 1)
	if src == inputGatedBot {
		t.Fatal("the fixture no longer declares the var this test constrains")
	}
	if err := os.WriteFile(botPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, Vars: map[string]string{"engine": "tpu"}})
	if !errors.Is(err, ErrLaunchVarsRefused) || !strings.Contains(err.Error(), `"tpu"`) {
		t.Fatalf("launch with engine=tpu: err = %v, want ErrLaunchVarsRefused naming the value", err)
	}
	runs, err := svc.ListCtx(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("ListCtx: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("a refused launch left %d run doc(s) behind", len(runs))
	}
	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath, Vars: map[string]string{"engine": "gpu"}})
	if err != nil {
		t.Fatalf("launch with engine=gpu refused: %v", err)
	}
	select {
	case <-res.Done:
	case <-runWaitContext(t).Done():
		t.Fatal("run goroutine did not exit (expected immediate human pause)")
	}
}
