package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The publisher reads ONE view for a templated program (ADR-129 §4, §7):
// the credential walk's env-funded answer, the pinned stamp and the tier
// walk all read the launch view — the run's vars by provenance. A declared
// default whose value the run itself decides makes the run env-funded, so
// no credential is acquired for it — not even the team key the tier walk
// would have sealed for the undecided route the var-less reading used to
// see.
// Mutant: the env-funded derivation reads a var-less view again (the view
// argument reverted) → the templated model stays undecided → the team's
// anthropic key seals → this test reds on the sealed-key assertion.
func TestPublisherReadsOneView(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
	fd := string(platformcfg.FacadeAuto)
	p := &Publisher{
		apiKeys: keys, oauthForfait: secrets.NewMemoryOAuthStore(),
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: testLogger(), usageCaps: usagecap.NewMemStore(),
		platformAudience: audienceResolver(&platformcfg.PlatformCredentials{FacadeDefault: &fd}, nil),
	}
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"m": {Name: "m", Type: ir.VarString, HasDefault: true, Default: "openai_compatible/gw-1"},
		},
		Nodes: map[string]ir.Node{
			"impl": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{
				Backend: "claw", Model: "{{vars.m}}",
			}},
		},
	}
	view := model.RouteViewForLaunch[string](wf, model.ModelOverrides{}, nil, nil, nil)
	if pinned := derivePinnedProviders(view); len(pinned) != 0 {
		t.Fatalf("pinned = %v, want none — the gateway route names no bundle slot", pinned)
	}
	ctx := store.WithTenant(context.Background(), "team1")
	res, err := p.resolveAndSealCredentials(ctx, "run-view", "", "team1", "webhook:cfg-1", "", "", wf, nil, nil, view, nil, store.RunTrustDefault, nil, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if res.secretsRef != "" {
		t.Fatalf("sealed a bundle for an env-funded run (ref %s), want none", res.secretsRef)
	}
}
