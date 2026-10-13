package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A cloud launch is refused at the API for a value outside its var's
// constraint: no run row, no queue message — rather than a run the runner pod
// fails after the operator has gone.

func submitWithVars(t *testing.T, p *Publisher, wf *ir.Workflow, runID string, spec runview.LaunchSpec) (*queue.RunMessage, error) {
	t.Helper()
	var published []*queue.RunMessage
	p.publishRun = func(_ context.Context, msg *queue.RunMessage) error {
		published = append(published, msg)
		return nil
	}
	spec.FilePath, spec.Source = "w.bot", "workflow w:\n  start -> done\n"
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	_, err := p.SubmitLaunch(ctx, runID, spec, wf, &runview.CompiledSource{Hash: "hash"})
	if err != nil {
		if len(published) != 0 {
			t.Fatalf("a refused launch published %d message(s)", len(published))
		}
		if _, lerr := p.store.LoadRun(ctx, runID); lerr == nil {
			t.Fatalf("a refused launch left run %s behind", runID)
		}
		return nil, err
	}
	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}
	return published[0], nil
}

func modeWorkflow() *ir.Workflow {
	return &ir.Workflow{Name: "w", Vars: map[string]*ir.Var{
		"mode": {Name: "mode", Type: ir.VarString, EnumValues: []string{"fast", "slow"}},
		"tag":  {Name: "tag", Type: ir.VarString, Matching: "^[a-z]+$"},
	}}
}

func TestSubmitLaunchRefusesAVarOutsideItsConstraint(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{store: st, logger: testLogger()}
	for i, tc := range []struct {
		vars map[string]string
		want []string
	}{
		{map[string]string{"mode": "turbo"}, []string{`var "mode"`, `"turbo"`, `"fast", "slow"`}},
		{map[string]string{"tag": "codeX --some-flag"}, []string{`var "tag"`, `"codeX --some-flag"`, `"^[a-z]+$"`}},
	} {
		_, err := submitWithVars(t, p, modeWorkflow(), "run-refused-"+string(rune('a'+i)), runview.LaunchSpec{Vars: tc.vars})
		if !errors.Is(err, runview.ErrLaunchVarsRefused) {
			t.Fatalf("%v: err = %v, want ErrLaunchVarsRefused", tc.vars, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%v: refusal %q does not name %s", tc.vars, err, w)
			}
		}
	}
	msg, err := submitWithVars(t, p, modeWorkflow(), "run-admitted", runview.LaunchSpec{Vars: map[string]string{"mode": "slow", "tag": "abc"}})
	if err != nil {
		t.Fatalf("values inside the constraints refused: %v", err)
	}
	if msg.Vars["mode"] != "slow" || msg.Vars["tag"] != "abc" {
		t.Fatalf("RunMessage Vars = %v, want the admitted values", msg.Vars)
	}
}

// The runner's environment decides `${MODE:-turbo}`, and the API cannot see
// it: the value rides to the runner's gate, as written.
func TestSubmitLaunchLeavesAnEnvironmentReadToTheRunner(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{store: st, logger: testLogger()}
	msg, err := submitWithVars(t, p, modeWorkflow(), "run-env", runview.LaunchSpec{Vars: map[string]string{"mode": "${MODE:-turbo}"}})
	if err != nil {
		t.Fatalf("a value only the runner's environment can read was refused at the API: %v", err)
	}
	if msg.Vars["mode"] != "${MODE:-turbo}" {
		t.Fatalf("RunMessage Vars[mode] = %v, want the text as sent", msg.Vars["mode"])
	}
}

// The gate judges the values the RunMessage carries, the credential-derived
// injection applied: an operator's review_mode the injection replaces never
// reaches the runner, so refusing it would refuse a launch the runner admits.
func TestSubmitLaunchJudgesTheValueTheTopologyInjected(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, "team", secrets.ProviderOpenAI, "sk-openai")
	seedKey(t, keys, sealer, "team", secrets.ProviderAnthropic, "sk-ant")
	p := &Publisher{
		store:      st,
		apiKeys:    keys,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		logger:     testLogger(),
	}
	wf := &ir.Workflow{Name: "w", Vars: map[string]*ir.Var{
		"review_mode": {Name: "review_mode", Type: ir.VarString, EnumValues: []string{"auto", "mono", "dual"}},
	}}
	msg, err := submitWithVars(t, p, wf, "run-topo-gate", runview.LaunchSpec{Vars: map[string]string{"review_mode": "both"}})
	if err != nil {
		t.Fatalf("a value the injection replaced was judged: %v", err)
	}
	if got := msg.Vars["review_mode"]; got != "mono" && got != "dual" {
		t.Fatalf("RunMessage Vars[review_mode] = %v, want the injected topology", got)
	}
}
