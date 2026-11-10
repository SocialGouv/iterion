package model

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// ADR-131 §5's vocabulary split: the WALK reads every provider a credential
// tier can hold a key for, the gateway included; the POOL reads the same list
// less the gateway, which no legacy pool, door or pool want ever lends. One
// derivation, pinned both ways: the gateway in the walk's list, absent from
// the pool's, and nothing else differing — a provider added to one list and
// forgotten in the other would make the pool ask for a key the walk never
// seals, or the walk ignore a key the pool would lend.
// Mutants: the gateway dropped from KnownRoutingProviders → the first
// assertion reds; the pool list re-derived from the routing list → the
// second; a vendor dropped from either → the element-wise comparison.
func TestKnownPoolProvidersAreTheRoutingVocabularyLessTheGateway(t *testing.T) {
	t.Parallel()
	if !slices.Contains(KnownRoutingProviders, secrets.ProviderOpenAICompatible) {
		t.Fatalf("KnownRoutingProviders = %v: the walk vocabulary must name the gateway, or a team's gateway key is never sealed", KnownRoutingProviders)
	}
	if slices.Contains(KnownPoolProviders, secrets.ProviderOpenAICompatible) {
		t.Fatalf("KnownPoolProviders = %v: the pool vocabulary names the gateway, so the pool wants would ask donors for a gateway key", KnownPoolProviders)
	}
	var walkLessGateway []secrets.Provider
	for _, p := range KnownRoutingProviders {
		if p != secrets.ProviderOpenAICompatible {
			walkLessGateway = append(walkLessGateway, p)
		}
	}
	if !slices.Equal(walkLessGateway, KnownPoolProviders) {
		t.Fatalf("walk vocabulary less the gateway = %v, pool vocabulary = %v: the two lists must differ by the gateway alone, in the same order", walkLessGateway, KnownPoolProviders)
	}
	if walk, pool := KnownProviderSet(), KnownPoolProviderSet(); !walk[string(secrets.ProviderOpenAICompatible)] || pool[string(secrets.ProviderOpenAICompatible)] || len(walk) != len(pool)+1 {
		t.Fatalf("the sets disagree with their lists: walk=%v pool=%v", walk, pool)
	}
}

func gatewayNode(backend, provider, mdl string) *ir.AgentNode {
	return &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: backend, Provider: provider, Model: mdl}}
}

// ADR-131 §5: under RouteView.GatewayKeyFunded a DECIDED gateway route — a
// literal `openai_compatible/…` model — records the openai_compatible
// provider, pinned at its site, wherever the caller's vocabulary knows that
// provider; the run is no longer env-funded. Without the field, and under a
// vocabulary that does not know the gateway (the pool's), the route stays
// env-funded and never turns into an unknown name the pool wants would widen
// on. An env-templated model is never attributed (ADR-129 §4), and a gateway
// hint over a bare model stays an unknown name whatever the vocabulary.
// Mutants: the keyed arm removed from prefixOrWiden → "claw literal" reds;
// removed from chainDecides → "claude_code hint" reds; the vocabulary check
// dropped → "pool vocabulary" reds (a provider the pool cannot name); the
// raw-prefix check dropped → "env-templated model" reds; the bare-model hint
// recorded as known → "gateway hint, bare model" reds.
func TestEffectiveProviders_GatewayKeyFundedAttributesTheDecidedGatewayRoute(t *testing.T) {
	t.Parallel()
	gw := string(secrets.ProviderOpenAICompatible)
	full, pool := KnownProviderSet(), KnownPoolProviderSet()
	rows := []struct {
		name          string
		node          *ir.AgentNode
		keyed         bool
		known         map[string]bool
		allowed       []string
		wantAttrib    bool
		wantEnvFunded bool
		wantNarrow    bool
		wantUnknown   bool
		wantForbidden bool
	}{
		{name: "claw literal, unkeyed: env-funded as before", node: gatewayNode("claw", "", "openai_compatible/team/m"), known: full, wantEnvFunded: true, wantNarrow: true},
		{name: "claw literal, keyed: attributed", node: gatewayNode("claw", "", "openai_compatible/team/m"), keyed: true, known: full, wantAttrib: true, wantNarrow: true},
		{name: "claude_code hint, keyed: attributed", node: gatewayNode("claude_code", "openai_compatible", "openai_compatible/team/m"), keyed: true, known: full, wantAttrib: true, wantNarrow: true},
		{name: "pool vocabulary, keyed: env-funded, never unknown", node: gatewayNode("claw", "", "openai_compatible/team/m"), keyed: true, known: pool, wantEnvFunded: true, wantNarrow: true},
		{name: "env-templated model, keyed: undecided, never attributed", node: gatewayNode("claw", "", "${C3_UNSET_GATEWAY_DIAL:-openai_compatible/team/m}"), keyed: true, known: full},
		{name: "gateway hint, bare model, keyed: unknown", node: gatewayNode("claw", "openai_compatible", "claude-opus-5"), keyed: true, known: full, wantUnknown: true},
		{name: "gateway hint, bare model, unkeyed: unknown", node: gatewayNode("claw", "openai_compatible", "claude-opus-5"), known: full, wantUnknown: true},
		{name: "keyed under an allowlist naming the gateway", node: gatewayNode("claw", "", "openai_compatible/team/m"), keyed: true, known: full, allowed: []string{gw}, wantAttrib: true, wantNarrow: true},
		{name: "keyed under an allowlist excluding the gateway", node: gatewayNode("claw", "", "openai_compatible/team/m"), keyed: true, known: full, allowed: []string{"anthropic"}, wantNarrow: true, wantForbidden: true, wantEnvFunded: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": row.node}}
			got := EffectiveProviders(RouteView{Workflow: wf, GatewayKeyFunded: row.keyed, AllowedProviders: row.allowed}, row.known)
			attributed := slices.Contains(got.Providers, gw)
			if attributed != row.wantAttrib {
				t.Fatalf("gateway attributed = %v (providers=%v envFunded=%v unknown=%v), want %v", attributed, got.Providers, got.EnvFunded, got.Unknown, row.wantAttrib)
			}
			if row.wantAttrib {
				if !slices.Equal(got.Pins[gw], []string{"a"}) || !slices.Equal(got.Sites["a"], []string{gw}) {
					t.Errorf("an attributed gateway route must be pinned at its site: pins=%v sites=%v", got.Pins, got.Sites)
				}
			}
			if got.OnlyEnvFunded() != row.wantEnvFunded {
				t.Errorf("OnlyEnvFunded = %v, want %v (providers=%v narrow=%v unknown=%v)", got.OnlyEnvFunded(), row.wantEnvFunded, got.Providers, got.NarrowSafe, got.Unknown)
			}
			if got.NarrowSafe != row.wantNarrow {
				t.Errorf("NarrowSafe = %v, want %v (unknown=%v)", got.NarrowSafe, row.wantNarrow, got.Unknown)
			}
			if gotUnknown := slices.Contains(got.Unknown, gw); gotUnknown != row.wantUnknown {
				t.Errorf("gateway unknown = %v (unknown=%v), want %v", gotUnknown, got.Unknown, row.wantUnknown)
			}
			if len(got.Unknown) > 0 && !row.wantUnknown {
				t.Errorf("unknown names %v: a gateway route must never widen the walk on a name", got.Unknown)
			}
			if _, forbidden := got.Forbidden[gw]; forbidden != row.wantForbidden {
				t.Errorf("gateway forbidden = %v (forbidden=%v), want %v", forbidden, got.Forbidden, row.wantForbidden)
			}
		})
	}
}

// The fine plan (#2288) rows an attributed gateway route on the credential
// the runner will book it under: the bundle's endpoint-aware stamp, which no
// runner can recompute from the secret alone (ADR-131 §2). Without the field
// the gateway route names no row — the pod's money books nobody.
// Mutant: the keyed arm recording the provider without its site → the plan
// names no row and this reds.
func TestCredentialPlan_RowsAnAttributedGatewayRouteOnTheServersFingerprint(t *testing.T) {
	t.Parallel()
	gw := secrets.ProviderOpenAICompatible
	wantFP := secrets.APIKeyFingerprint(gw, "sk-gw", "https://gw.example")
	bundle := secrets.RunBundle{
		APIKeys:            map[secrets.Provider]string{gw: "sk-gw"},
		APIBaseURLs:        map[secrets.Provider]string{gw: "https://gw.example"},
		APIKeyFingerprints: map[secrets.Provider]string{gw: wantFP},
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": gatewayNode("claw", "", "openai_compatible/team/m")}}
	plan := CredentialPlan(EffectiveProviders(RouteView{Workflow: wf, GatewayKeyFunded: true}, KnownProviderSet()), bundle)
	if got := plan["a"+PlanKeySep+string(gw)]; got != wantFP {
		t.Fatalf("plan = %v, want the gateway route rowed on the server's endpoint-aware fingerprint %q", plan, wantFP)
	}
	if plan := CredentialPlan(EffectiveProviders(RouteView{Workflow: wf}, KnownProviderSet()), bundle); len(plan) != 0 {
		t.Fatalf("plan = %v on an unkeyed view: an env-funded gateway route books nobody", plan)
	}
}
