package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The queued-run half of the credential-derived topology injection: a
// cloud launch has no host detection report, so review_mode / plan_review
// / llm_families resolve from what actually SEALED into the run's bundle.
// Before this, queued runs got no injection at all and every declared bot
// ran on its raw "auto" defaults.

func topologyWorkflow(vars ...string) *ir.Workflow {
	w := &ir.Workflow{Name: "w", Vars: map[string]*ir.Var{}}
	for _, v := range vars {
		w.Vars[v] = &ir.Var{}
	}
	return w
}

func launchTopologyRun(t *testing.T, p *Publisher, wf *ir.Workflow, runID string) (*store.Run, *queue.RunMessage) {
	t.Helper()
	var published []*queue.RunMessage
	p.publishRun = func(_ context.Context, msg *queue.RunMessage) error {
		published = append(published, msg)
		return nil
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	spec := runview.LaunchSpec{
		FilePath: "w.bot",
		Source:   "workflow w:\n  start -> done\n",
	}
	if _, err := p.SubmitLaunch(ctx, runID, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}
	r, err := p.store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return r, published[0]
}

func TestSubmitLaunchInjectsTopologyFromSealedBundle(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
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
	wf := topologyWorkflow("review_mode", "plan_review", "llm_families")
	r, msg := launchTopologyRun(t, p, wf, "run-topo")

	for name, want := range map[string]string{
		"plan_review":  "on",
		"review_mode":  "mono",
		"mono_family":  "claude",
		"llm_families": "claude,gpt",
	} {
		if got := r.Inputs[name]; got != want {
			t.Errorf("run doc Inputs[%s] = %v, want %q", name, got, want)
		}
		if got := msg.Vars[name]; got != want {
			t.Errorf("RunMessage Vars[%s] = %v, want %q", name, got, want)
		}
	}
}

// A run whose bundle resolved NOTHING rides the runner's env fallback,
// which the publisher cannot see — the bot's own "auto" defaults must
// survive untouched rather than be resolved from a false "no credentials"
// premise.
func TestSubmitLaunchLeavesTopologyUntouchedWithoutCredentials(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{store: st, logger: testLogger()}
	wf := topologyWorkflow("review_mode", "plan_review")
	r, msg := launchTopologyRun(t, p, wf, "run-env")

	for _, name := range []string{"review_mode", "plan_review", "mono_family"} {
		if _, ok := r.Inputs[name]; ok {
			t.Errorf("run doc Inputs[%s] injected despite an empty bundle: %+v", name, r.Inputs)
		}
		if _, ok := msg.Vars[name]; ok {
			t.Errorf("RunMessage Vars[%s] injected despite an empty bundle: %+v", name, msg.Vars)
		}
	}
}
