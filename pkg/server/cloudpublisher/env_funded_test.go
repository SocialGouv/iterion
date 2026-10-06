package cloudpublisher

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// envFundedWorkflow is a workflow whose every LLM route rides the runner's
// openai_compatible gateway, and that declares one generic secret — the
// credential the walk must STILL resolve and seal.
var envFundedWorkflow = &ir.Workflow{
	Nodes: map[string]ir.Node{
		"implement": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "implement"}, LLMFields: ir.LLMFields{
			Backend: "claw", Model: "openai_compatible/team/m",
		}},
	},
}

// The same run, declaring one generic secret — resolved and sealed even
// though no LLM credential is acquired.
func envFundedWorkflowWithSecrets() *ir.Workflow {
	wf := *envFundedWorkflow
	wf.Nodes = envFundedWorkflow.Nodes
	wf.Secrets = map[string]*ir.Secret{"forge_token": {}}
	return &wf
}

// #2038: a run whose every model route is an openai_compatible route is
// funded by the runner's environment — the walk acquires no LLM credential
// for it: no BYOK, org or platform key, no forfait, no pool lease, no
// restore. A declared generic secret still resolves and seals. A mixed run
// keeps today's behaviour.
func TestEnvFundedRun_AcquiresNoLLMCredential(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wf          *ir.Workflow
		wantZAI     bool
		wantAnthro  bool
		wantForf    bool
		wantSealed  bool
		wantGeneric bool
	}{
		{"env-funded: nothing acquired", envFundedWorkflow, false, false, false, false, false},
		{"env-funded: a declared generic secret still seals", envFundedWorkflowWithSecrets(), false, false, false, true, true},
		{"mixed: the anthropic key still fills", &ir.Workflow{Nodes: map[string]ir.Node{
			"implement": envFundedWorkflow.Nodes["implement"],
			"review":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "anthropic", Model: "claude-opus-5"}},
		}}, false, true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			keys := secrets.NewMemoryApiKeyStore()
			seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform")
			seedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
			oauth := secrets.NewMemoryOAuthStore()
			seedOAuth(t, oauth, sealer, "webhook:cfg-1", "sk-ant-team-forfait")
			fd := string(platformcfg.FacadeAuto)
			genericStore := secrets.NewMemoryGenericSecretStore()
			if tc.wantGeneric {
				secretID := secrets.NewGenericSecretID()
				sealedGeneric, err := secrets.SealGenericSecret(sealer, secretID, []byte("forge-token-value"))
				if err != nil {
					t.Fatalf("SealGenericSecret: %v", err)
				}
				if err := genericStore.Create(context.Background(), secrets.GenericSecret{
					ID: secretID, ScopeTeamID: "team1", ScopeUserID: "webhook:cfg-1", Name: "forge_token", SealedSecret: sealedGeneric, CreatedAt: time.Now().UTC(),
				}); err != nil {
					t.Fatalf("Create: %v", err)
				}
			}
			p := &Publisher{
				apiKeys: keys, oauthForfait: oauth, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
				logger: testLogger(), usageCaps: usagecap.NewMemStore(),
				genericSecrets:   genericStore,
				platformAudience: audienceResolver(&platformcfg.PlatformCredentials{FacadeDefault: &fd}, nil),
			}
			ctx := store.WithTenant(context.Background(), "team1")
			res, err := p.resolveAndSealCredentials(ctx, "run-2038", "", "team1", "webhook:cfg-1", "", "", tc.wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil, nil)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			if (res.secretsRef == "") == tc.wantSealed {
				t.Fatalf("sealed = %v, want sealed=%v", !tc.wantSealed, tc.wantSealed)
			}
			if res.secretsRef == "" {
				return
			}
			rec, err := p.runSecrets.(*secrets.MemoryRunSecretsStore).Get(ctx, res.secretsRef)
			if err != nil {
				t.Fatal(err)
			}
			b, err := secrets.OpenRunBundle(mustDEKSealer(t, res.dek), rec.TenantID, "", "run-2038", rec.KeyID, rec.SealedBundle)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantGeneric && b.GenericSecrets["forge_token"] == "" {
				t.Error("the declared generic secret did not seal")
			}
			if got := b.APIKeys[secrets.ProviderZAI] != ""; got != tc.wantZAI {
				t.Errorf("platform z.ai key sealed = %v, want %v", got, tc.wantZAI)
			}
			if got := b.APIKeys[secrets.ProviderAnthropic] != ""; got != tc.wantAnthro {
				t.Errorf("team anthropic key sealed = %v, want %v", got, tc.wantAnthro)
			}
			if got := len(b.OAuthCredentials["claude_code"]) != 0; got != tc.wantForf {
				t.Logf("bundle dump: api=%v pinned=%v oauth=%v platform=%v org=%v pool=%v",
					b.APIKeys, b.PinnedAPIKeys, map[string]int{"claude_code": len(b.OAuthCredentials["claude_code"])}, b.PlatformSourced, b.OrgSourced, b.PoolSourced)
				t.Errorf("a forfait was sealed = %v, want %v", got, tc.wantForf)
			}
			if got := b.PinnedAPIKeys; len(got) != 0 {
				t.Errorf("pinned keys = %v, want none: nothing here pins a route", got)
			}
			if res.grant != nil {
				t.Error("the pool leased a donor to a run that cannot spend it")
			}
		})
	}
}

// The strict deployment's refusal (ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL)
// keys on PINNED providers a tier must hold. An env-funded run pins none of
// the vocabulary: it is the runner-env fallback the flag describes, not a
// provisionable run — unchanged either way.
func TestEnvFundedRun_RequireLLMCredentialDoesNotRefuse(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	p := &Publisher{
		apiKeys: secrets.NewMemoryApiKeyStore(), oauthForfait: secrets.NewMemoryOAuthStore(),
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger(),
		usageCaps: usagecap.NewMemStore(), requireLLMCredential: true,
		platformAudience: audienceResolver(&platformcfg.PlatformCredentials{}, nil),
	}
	ctx := store.WithTenant(context.Background(), "team1")
	if _, err := p.resolveAndSealCredentials(ctx, "run-2038b", "", "team1", "webhook:cfg-1", "", "", envFundedWorkflow, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil, nil); err != nil {
		t.Fatalf("an env-funded run was refused under requireLLMCredential: %v", err)
	}
}

// A closed team forfait beside the gateway route: nothing acquires, and the
// window skip costs no Warn — the runner env is the plan, not a failure.
func TestEnvFundedRun_ClosedForfaitIsNotAFailure(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform")
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "webhook:cfg-1", "sk-ant-team-forfait")
	st := usagecap.NewMemStore()
	if err := st.Record(context.Background(), usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team1"), seededFP("webhook:cfg-1")), usagecap.Reading{
		Window: usagecap.WindowSevenDay, Status: usagecap.StatusRejected, Utilization: 1,
		ResetsAt: time.Now().Add(48 * time.Hour), ObservedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	fd := string(platformcfg.FacadeAuto)
	p := &Publisher{
		apiKeys: keys, oauthForfait: oauth, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: testLogger(), usageCaps: st,
		platformAudience: audienceResolver(&platformcfg.PlatformCredentials{FacadeDefault: &fd}, nil),
	}
	ctx := store.WithTenant(context.Background(), "team1")
	res, err := p.resolveAndSealCredentials(ctx, "run-2038c", "", "team1", "webhook:cfg-1", "", "", envFundedWorkflow, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	// Nothing to acquire and nothing generic to seal: the resolution is
	// empty — which is the env-funded contract, not a failure.
	if res.secretsRef != "" {
		rec, _ := p.runSecrets.(*secrets.MemoryRunSecretsStore).Get(ctx, res.secretsRef)
		b, err := secrets.OpenRunBundle(mustDEKSealer(t, res.dek), rec.TenantID, "", "run-2038c", rec.KeyID, rec.SealedBundle)
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("an env-funded run acquired credentials beside its closed forfait: api=%v oauth=%v", b.APIKeys, b.OAuthCredentials)
	}
}

// The preview answers for an env-funded run the way the live fill does:
// no candidate selected, and the reason said where the operator reads it.
func TestCredentialPreviewMatchesTheEnvFundedRun(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, secrets.PlatformTenantID, secrets.ProviderZAI, "sk-zai-platform")
	p := &Publisher{
		apiKeys: keys, oauthForfait: secrets.NewMemoryOAuthStore(),
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, logger: testLogger(),
		usageCaps:        usagecap.NewMemStore(),
		platformAudience: audienceResolver(&platformcfg.PlatformCredentials{}, nil),
	}
	const team = "team1"
	spec := previewSpec(team, "webhook:private")
	wf := envFundedWorkflow

	preview, err := previewReadOnly(p).PreviewCredentials(context.Background(), spec, wf)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	for _, c := range preview.Candidates {
		if c.Selected {
			t.Fatalf("the preview selected %s/%s for a run that acquires nothing", c.Tier, c.Provider)
		}
	}
	said := false
	for _, w := range preview.Warnings {
		if strings.Contains(w, "openai_compatible") {
			said = true
		}
	}
	if !said {
		t.Errorf("the preview does not say the run is env-funded: %v", preview.Warnings)
	}

	// The live resolution agrees: nothing sealed.
	res, err := p.resolveAndSealCredentials(store.WithTenant(context.Background(), team), "run-2038d", "", team, spec.OwnerID, spec.Context.BotID, "", wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.secretsRef != "" {
		t.Fatal("the live fill sealed credentials for an env-funded run")
	}
}

// An env-templated route (${GW}/m) is UNRESOLVED in the publisher process:
// the runner may expand it to any vendor. The pool's wants derivation must
// fail open — narrowing on the run's resolved peers would drop the very
// donation the templated route needs when the runner expands it elsewhere.
func TestWantsFor_AnEnvTemplatedRouteFailsOpen(t *testing.T) {
	t.Setenv("GW", "openai_compatible")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"gw": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{
			Backend: "claw", Provider: "openai_compatible", Model: "${GW}/m",
		}},
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{
			Backend: "claw", Provider: "anthropic", Model: "claude-opus-5",
		}},
	}}
	wants, res := wantsFor(wf, model.ModelOverrides{}, nil)
	if res.NarrowSafe {
		t.Fatal("narrow-safe on an env-templated route: the pool would narrow on it")
	}
	if len(wants) != len(poolWantOrder) {
		t.Fatalf("wants narrowed to %d of %d — the env-templated route dropped donations", len(wants), len(poolWantOrder))
	}
}

// The env-SET spelling (the deployment resolves ${GW} to the gateway here)
// is not the gateway route either — what ${GW} names on the RUNNER is still
// the runner's word: on a mixed run the pool's wants must fail open, or the
// templated route drops the donation its peers need.
func TestWantsFor_AnEnvSetTemplatedRouteFailsOpen(t *testing.T) {
	t.Setenv("GW", "openai_compatible")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"gw": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{
			Backend: "claw", Model: "${GW}/m",
		}},
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{
			Backend: "claw", Provider: "anthropic", Model: "claude-opus-5",
		}},
	}}
	wants, res := wantsFor(wf, model.ModelOverrides{}, nil)
	if res.NarrowSafe {
		t.Fatal("narrow-safe on an env-set templated route: the pool would narrow on it")
	}
	if len(wants) != len(poolWantOrder) {
		t.Fatalf("wants narrowed to %d of %d — the env-set templated route dropped donations", len(wants), len(poolWantOrder))
	}
}
