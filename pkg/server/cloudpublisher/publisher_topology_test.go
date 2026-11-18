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
	return launchTopologyRunWithVars(t, p, wf, runID, nil)
}

// launchTopologyRunWithVars is launchTopologyRun with the operator's own
// --var map, so a case can state what the LAUNCH asked for and read what the
// publisher queued.
func launchTopologyRunWithVars(t *testing.T, p *Publisher, wf *ir.Workflow, runID string, vars map[string]string) (*store.Run, *queue.RunMessage) {
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
		Vars:     vars,
	}
	if _, err := p.SubmitLaunch(ctx, runID, spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
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
	wf := topologyWorkflow("review_mode", "plan_review", "llm_families")
	r, msg := launchTopologyRun(t, p, wf, "run-topo")

	for name, want := range map[string]string{
		"plan_review":  "on",
		"review_mode":  "single",
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

// A run whose bundle resolved NOTHING rides the runner's env fallback, which
// the publisher cannot see — the two injections that READ the bundle
// (plan_review, llm_families) must leave the bot's own defaults untouched
// rather than be resolved from a false "no credentials" premise. The review
// mode is deliberately not one of them: it reads no credential, and its own
// witness is TestSubmitLaunchResolvesTheModeWithoutCredentials.
func TestSubmitLaunchLeavesTopologyUntouchedWithoutCredentials(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{store: st, logger: testLogger()}
	wf := topologyWorkflow("review_mode", "plan_review")
	r, msg := launchTopologyRun(t, p, wf, "run-env")

	for _, name := range []string{"plan_review", "llm_families"} {
		if _, ok := r.Inputs[name]; ok {
			t.Errorf("run doc Inputs[%s] injected despite an empty bundle: %+v", name, r.Inputs)
		}
		if _, ok := msg.Vars[name]; ok {
			t.Errorf("RunMessage Vars[%s] injected despite an empty bundle: %+v", name, msg.Vars)
		}
	}
}

// The review mode is resolved on the EMPTY-BUNDLE lane too, and that is the
// half that keeps a pre-rename integration working: the mode reads no
// credential any more (ADR-169), so the skip below it — which exists because
// plan_review and llm_families are facts about credentials this publisher
// cannot see — does not reach it. Without this, a frozen `dual` would reach
// the bot's guards as an unknown value (a paired review silently downgraded
// to single) and a frozen `mono` would be refused by its enum gate.
//
// The lane arm of the claim, not a second copy of the injection above: the
// arm is that `len(creds.families.Families()) > 0` decides whether the two
// CREDENTIAL-READING injections run at all.
func TestSubmitLaunchResolvesTheModeWithoutCredentials(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{store: st, logger: testLogger()}
	wf := topologyWorkflow("review_mode", "plan_review", "llm_families")
	for launched, want := range map[string]string{"dual": "paired", "mono": "single", "": "single", "single": "single"} {
		vars := map[string]string{}
		if launched != "" {
			vars["review_mode"] = launched
		}
		runID := "run-env-mode-" + launched
		r, msg := launchTopologyRunWithVars(t, p, wf, runID, vars)
		if got := msg.Vars["review_mode"]; got != want {
			t.Errorf("RunMessage Vars[review_mode] = %v for a launch asking %q, want the concrete %q — the lane resolved no family, and the mode does not need one", got, launched, want)
		}
		if got := r.Inputs["review_mode"]; got != want {
			t.Errorf("run doc Inputs[review_mode] = %v for a launch asking %q, want %q", got, launched, want)
		}
		// The two credential-reading vars keep surviving untouched.
		for _, name := range []string{"plan_review", "llm_families"} {
			if _, ok := msg.Vars[name]; ok {
				t.Errorf("RunMessage Vars[%s] injected despite an empty bundle: %+v", name, msg.Vars)
			}
		}
	}
}

func TestSubmitLaunchKeepsAnExplicitModeWhenTheBundleResolvedNothing(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	p := &Publisher{store: st, logger: testLogger()}
	wf := topologyWorkflow("review_mode")
	launched := map[string]string{"review_mode": "paired"}
	r, msg := launchTopologyRunWithVars(t, p, wf, "run-env-pin", launched)

	for name, want := range launched {
		if got := msg.Vars[name]; got != want {
			t.Errorf("RunMessage Vars[%s] = %v, want %q — an empty bundle injects nothing, so the operator's own value rides the launch verbatim", name, got, want)
		}
		if got := r.Inputs[name]; got != want {
			t.Errorf("run doc Inputs[%s] = %v, want %q — the frozen launch inputs a resume replays carry the same value", name, got, want)
		}
	}
}
