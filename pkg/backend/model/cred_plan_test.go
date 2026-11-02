package model

import (
	"maps"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// planBundle builds the sealed bundle a door-shaped run carries: the run's
// own anthropic and openai keys, and the DONOR's zai key the fallback door
// lent — marked pool-sourced exactly like acquireDoorGrant seals it. The
// fingerprint strings are the plan's whole point: the producer must carry
// THESE strings, byte for byte, because the lease stamps the same one and
// the broker books the slice that matches it.
func planBundle() secrets.RunBundle {
	return secrets.RunBundle{
		APIKeys: map[secrets.Provider]string{
			secrets.ProviderAnthropic: "sk-own-anthropic",
			secrets.ProviderOpenAI:    "sk-own-openai",
			secrets.ProviderZAI:       "sk-donor-zai",
		},
		APIKeyFingerprints: map[secrets.Provider]string{
			secrets.ProviderAnthropic: "fp-own-anthropic",
			secrets.ProviderOpenAI:    "fp-own-openai",
			secrets.ProviderZAI:       "fp-donor-zai",
		},
		PoolSourced: map[string]bool{string(secrets.ProviderZAI): true},
	}
}

// doorWorkflow is the shape the fine booking exists for: a node whose own
// route names the run's own anthropic key, and whose authored rung names
// the DONOR's zai credential — two credentials in one node, the split a
// whole-attempt booking cannot see.
func doorWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"impl": &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "impl"},
			LLMFields: ir.LLMFields{Provider: "anthropic", Model: "anthropic/claude-opus-5"},
			Fallbacks: []ir.Fallback{{Name: "donor-rung", Provider: "zai"}},
		},
		"review": agentWith("review", "openai", "openai/gpt-5"),
	}}
}

func TestCredentialPlan_KeysTheLeaseSpacePerNodeAndProvider(t *testing.T) {
	t.Parallel()

	res := EffectiveProviders(RouteView{Workflow: doorWorkflow()}, knownForTest)
	if !res.NarrowSafe {
		t.Fatalf("the door walk narrowed to nothing — test premise broken: %+v", res)
	}
	plan := CredentialPlan(res, planBundle())
	want := map[string]string{
		"impl|anthropic": "fp-own-anthropic",
		"impl|zai":       "fp-donor-zai",
		"review|openai":  "fp-own-openai",
	}
	if !maps.Equal(plan, want) {
		t.Fatalf("plan = %v, want %v — the producer must key per (node, provider) with the bundle's own fingerprints", plan, want)
	}
}

func TestCredentialPlan_NilWhenTheWalkWidens(t *testing.T) {
	t.Parallel()

	// A bare-model node may spend anything: no per-node statement the
	// publisher could stamp is total, and stamping a partial plan would
	// book the unnamed spend on nobody.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"pinned": agentWith("pinned", "zai", "zai/glm-5"),
		"bare":   agentWith("bare", "", "claude-sonnet-4-5"),
	}}
	res := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if res.NarrowSafe {
		t.Fatal("a bare-model route must widen the walk — test premise broken")
	}
	if plan := CredentialPlan(res, planBundle()); plan != nil {
		t.Fatalf("plan = %v, want nil — a widened walk stamps no plan", plan)
	}
}

func TestCredentialPlan_OAuthForfaitFundsItsProvider(t *testing.T) {
	t.Parallel()

	// The run holds no anthropic KEY: the slot a route naming anthropic
	// spends is the Claude forfait, and the plan must carry the
	// subscription's fingerprint — the one the lease stamps when the
	// forfait itself is what a donor lent.
	bundle := secrets.RunBundle{
		OAuthCredentials: map[string][]byte{string(secrets.OAuthKindClaudeCode): []byte("payload")},
		OAuthFingerprints: map[string]string{
			string(secrets.OAuthKindClaudeCode): "fp-donor-oauth",
		},
		PoolSourced: map[string]bool{string(secrets.OAuthKindClaudeCode): true},
	}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"impl": agentWith("impl", "anthropic", "anthropic/claude-opus-5"),
	}}
	res := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	plan := CredentialPlan(res, bundle)
	want := map[string]string{"impl|anthropic": "fp-donor-oauth"}
	if !maps.Equal(plan, want) {
		t.Fatalf("plan = %v, want %v — an OAuth-funded provider plans its subscription's fingerprint", plan, want)
	}
}

func TestCredentialPlan_RunFallbackProvidersLandOnEveryNode(t *testing.T) {
	t.Parallel()

	// The run-level chain lands on every agent node through
	// ir.ApplyRunFallback: a chain rung naming the donor's provider may be
	// served at ANY of them, so every node plans that provider.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"impl":   agentWith("impl", "anthropic", "anthropic/claude-opus-5"),
		"review": agentWith("review", "openai", "openai/gpt-5"),
	}}
	view := RouteView{Workflow: wf, RunFallbacks: []FallbackEntry{{Backend: "claw", Provider: "zai"}}}
	res := EffectiveProviders(view, knownForTest)
	plan := CredentialPlan(res, planBundle())
	for _, node := range []string{"impl", "review"} {
		if got := plan[node+"|zai"]; got != "fp-donor-zai" {
			t.Fatalf("plan[%s|zai] = %q, want %q — the chain's provider plans on every node", node, got, "fp-donor-zai")
		}
	}
	if got := plan["impl|anthropic"]; got != "fp-own-anthropic" {
		t.Fatalf("plan[impl|anthropic] = %q, want %q — the chain adds a provider, it does not take one away", got, "fp-own-anthropic")
	}
}

func TestCredentialPlan_NilWhenNothingSealed(t *testing.T) {
	t.Parallel()

	// Every route names its provider and the bundle seals nothing (the
	// gateway-funded shape): no node plans a credential, and an empty map
	// serialises as absent — never as a table of empty fingerprints.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"impl": agentWith("impl", "anthropic", "anthropic/claude-opus-5"),
	}}
	res := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if plan := CredentialPlan(res, secrets.RunBundle{}); plan != nil {
		t.Fatalf("plan = %v, want nil — nothing sealed plans nothing", plan)
	}
}

func TestCredentialPlan_SitesCarryTheChainOrder(t *testing.T) {
	t.Parallel()

	// Sites is the read-order twin of Pins: the plan and any other reader
	// of "what does this site name" must see the chain's own order, not a
	// sorted copy of it.
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"impl": agentWith("impl", "zai,anthropic", ""),
	}}
	res := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	sites := res.Sites["impl"]
	if !slices.Equal(sites, []string{"zai", "anthropic"}) {
		t.Fatalf("Sites[impl] = %v, want [zai anthropic] — the chain order is the read order", sites)
	}
	if got := res.Pins["zai"]; len(got) != 1 || got[0] != "impl" {
		t.Fatalf("Pins[zai] = %v, want [impl] — Sites never disturbs Pins", got)
	}
}

// nodeOn is agentWith plus the route's backend: the fact the twice-funded
// shapes below turn on — the delegate precedence that decides which of the
// two sealed credentials a route spends.
func nodeOn(id, backend, provider, mdl string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Backend: backend, Provider: provider, Model: mdl},
	}
}

// twiceFundedBundle is the shape a shared tier seals when a route pins the
// forfait's OWN provider (cloudpublisher's TestSharedTier_
// aPinOnTheForfaitsOwnProviderIsFundedPinnedOnly): the subscription holds
// the family while the key waits in the pinned channel for the routes that
// name it. Two credentials, one provider — the plan's answer is the route
// backend's, not the bundle's.
func twiceFundedBundle(kind secrets.OAuthKind) secrets.RunBundle {
	return secrets.RunBundle{
		OAuthCredentials: map[string][]byte{string(kind): []byte("payload")},
		OAuthFingerprints: map[string]string{
			string(kind): "fp-" + string(kind) + "-forfait",
		},
		PinnedAPIKeys: map[secrets.Provider]string{
			secrets.Provider(OAuthProvider(string(kind))): "sk-pinned",
		},
	}
}

func TestCredentialPlan_TwiceFundedProviderPlansTheForfaitForAForfaitFirstWalk(t *testing.T) {
	t.Parallel()

	// Every route naming the provider runs a forfait-first backend, so the
	// runtime spends the forfait (its declared precedence reads key,
	// forfait, pinned key) and never reaches the pinned key. The plan must
	// name the subscription's fingerprint — the channel order the
	// fingerprint reader follows would name the pinned key and book the
	// forfait's spend on an instrument nobody spent.
	for _, tc := range []struct {
		name     string
		provider string
		backend  string
		kind     secrets.OAuthKind
	}{
		{"anthropic beside a Claude forfait on claude_code", "anthropic", "claude_code", secrets.OAuthKindClaudeCode},
		{"openai beside a codex forfait on codex", "openai", "codex", secrets.OAuthKindCodex},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := twiceFundedBundle(tc.kind)
			wf := &ir.Workflow{Nodes: map[string]ir.Node{
				"impl": nodeOn("impl", tc.backend, tc.provider, tc.provider+"/claude-opus-5"),
			}}
			res := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
			if !res.NarrowSafe {
				t.Fatalf("the walk narrowed to nothing — test premise broken: %+v", res)
			}
			if !slices.Contains(res.ForfaitFirst, tc.provider) {
				t.Fatalf("ForfaitFirst = %v, want %q — every route naming it runs %s", res.ForfaitFirst, tc.provider, tc.backend)
			}
			plan := CredentialPlan(res, bundle)
			want := map[string]string{"impl|" + tc.provider: "fp-" + string(tc.kind) + "-forfait"}
			if !maps.Equal(plan, want) {
				t.Fatalf("plan = %v, want %v — a forfait-first backend spends the forfait before the pinned key, and the plan names what the runtime spends", plan, want)
			}
		})
	}
}

func TestCredentialPlan_TwiceFundedProviderBehindAMixedWalkPlansNothing(t *testing.T) {
	t.Parallel()

	// The same twice-funded bundle behind a claw route beside the
	// claude_code one: claw bills a Claude forfait as extra usage, so its
	// anthropic site spends the pinned key while the claude_code site
	// spends the forfait. One (node, provider) key cannot name both, and a
	// plan naming either books the other's spend wrong — the derivation
	// stamps nil, and the broker keeps the coarse whole-attempt booking,
	// the conservative-on-ignore direction.
	bundle := twiceFundedBundle(secrets.OAuthKindClaudeCode)
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"impl":   nodeOn("impl", "claude_code", "anthropic", "anthropic/claude-opus-5"),
		"review": nodeOn("review", "claw", "anthropic", "anthropic/claude-opus-5"),
	}}
	res := EffectiveProviders(RouteView{Workflow: wf}, knownForTest)
	if !res.NarrowSafe {
		t.Fatalf("the walk narrowed to nothing — test premise broken: %+v", res)
	}
	if slices.Contains(res.ForfaitFirst, "anthropic") {
		t.Fatalf("ForfaitFirst = %v, want it without anthropic — the claw route does not declare the forfait's precedence", res.ForfaitFirst)
	}
	if plan := CredentialPlan(res, bundle); plan != nil {
		t.Fatalf("plan = %v, want nil — a twice-funded provider behind a mixed walk has no total per-(node, provider) answer", plan)
	}
}
