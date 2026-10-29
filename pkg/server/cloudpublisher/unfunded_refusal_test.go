package cloudpublisher

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The workflows the ticket's own proof runs (#2485). `free` is the
// unattributable route — a claw/claude_code node with no model pin, the most
// ordinary shape in the catalogue — and it used to disarm the refusal for
// `draft` and `verify` both.
const (
	mixedAttributableBot = "agent draft:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claw\"\n\njudge verify:\n  model: \"openai/gpt-5.4-mini\"\n  backend: \"claw\"\n\nagent free:\n  backend: \"claude_code\"\n\nworkflow main:\n  draft -> verify\n  verify -> free\n  free -> done\n"
	// A human node's LLM half pins a provider the same way an agent does;
	// the walk reads it through the shared reader #2691 landed
	// (humanLLMModelOf). Beside it, an unattributable agent — so the refusal
	// can only come from the human node's own route.
	humanGateBot = "schema esc_in:\n  q: string\n\nschema esc_out:\n  a: string\n\nhuman gate:\n  input: esc_in\n  output: esc_out\n  interaction: llm_or_human\n  model: \"anthropic/claude-opus-5\"\n\nagent free:\n  backend: \"claude_code\"\n\nworkflow main:\n  entry: gate\n  gate -> free\n  free -> done\n"
)

// #2485 — under `llm_fallback: none`, ONE unattributable route disarmed the
// refusal of every unfunded provider the walk DID attribute.
//
// The refusal read its set from spendableProviders, which returns its zero
// value as soon as a route cannot be attributed (`!NarrowSafe`), and then
// pinnedUnfundedProviders returned nil on a nil set: the launch started, and
// the pinned `openai` route spent the runner pod's environment — the one
// funding this posture exists to rule out.
func TestResolve_LLMFallbackNone_AnUnattributableRouteNoLongerDisarmsTheRefusal(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")

	err := resolvePolicy(t, p, mixedAttributableBot)
	if err == nil {
		t.Fatal("the launch STARTED: one unattributable route (`free`) emptied the attributed set, so the pinned `openai` route — which the team funds nothing of — was never refused and spends the runner pod's env")
	}
	if !errors.Is(err, runview.ErrNoLLMCredential) {
		t.Fatalf("refusal = %v, want runview.ErrNoLLMCredential (every existing reader matches it with errors.Is)", err)
	}
	// The fact travels WITH the refusal: a caller that had to parse the
	// sentence to learn which provider failed would be a second, weaker
	// parser of this decision.
	var unfunded *UnfundedProvidersError
	if !errors.As(err, &unfunded) {
		t.Fatalf("refusal %T is not an *UnfundedProvidersError — the providers and their sites must travel with the error, not inside its text", err)
	}
	if got := unfunded.Providers; len(got) != 1 || got[0] != "openai" {
		t.Fatalf("unfunded providers = %v, want exactly [openai]: `anthropic` IS funded by the team's own key and `free` names no provider at all", got)
	}
	if !unfunded.Sovereign {
		t.Error("the refusal does not say it came from the sovereign posture — the operator's remedy differs from the knob's")
	}
	// Provider AND node: "provision openai" without saying which node spends
	// it sends the operator reading a whole workflow.
	if sites := unfunded.Sites["openai"]; len(sites) != 1 || sites[0] != "verify" {
		t.Errorf("sites naming openai = %v, want [verify] — the refusal must name the node", sites)
	}
	for _, want := range []string{"openai", "verify", "llm_fallback=none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q does not contain %q", err, want)
		}
	}
	// And it names NEITHER the funded provider nor the unattributable node.
	for _, unwanted := range []string{"anthropic", "free"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("refusal message %q names %q, which it does not refuse", err, unwanted)
		}
	}
}

// The compromise the unattributable routes are owed is unchanged: they
// contribute no name, so they are never refused on their own. A team holding
// its own credential and a workflow NO route of which the walk can read still
// starts (the arm #2309 shipped), and a team holding NOTHING is still refused.
func TestResolve_LLMFallbackNone_TheUnattributableCompromiseIsUnchanged(t *testing.T) {
	t.Run("a credential of the team's own keeps the compromise", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
		if err := resolvePolicy(t, p, unattributableBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — a route the walk cannot read is refused by no name", err)
		}
	})
	t.Run("nothing of the team's own still refuses", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		if err := resolvePolicy(t, p, unattributableBot); !errors.Is(err, runview.ErrNoLLMCredential) {
			t.Fatalf("resolveAndSealCredentials = %v, want runview.ErrNoLLMCredential — the pod's env would be the only funding left", err)
		}
	})
}

// The class, not the site: a human node's LLM half pins a provider, and the
// walk reads it through the ONE shared reader #2691 landed (d62f0cd6c,
// humanLLMModelOf). Beside it an unattributable agent, so the only name in
// the attributed set is the human node's — which is what makes this a
// witness of the reader and not of the agent arm.
func TestResolve_LLMFallbackNone_AHumanGatesModelIsRefusedByName(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderOpenAI, "sk-openai-team", "fp-openai-team")

	err := resolvePolicy(t, p, humanGateBot)
	var unfunded *UnfundedProvidersError
	if !errors.As(err, &unfunded) {
		t.Fatalf("resolveAndSealCredentials = %v, want an *UnfundedProvidersError naming anthropic: the human gate's `model:` spends a provider the team funds nothing of", err)
	}
	if got := unfunded.Providers; len(got) != 1 || got[0] != "anthropic" {
		t.Fatalf("unfunded providers = %v, want [anthropic] — the human node's own route", got)
	}
	if sites := unfunded.Sites["anthropic"]; len(sites) != 1 || sites[0] != "gate" {
		t.Errorf("sites naming anthropic = %v, want [gate]", sites)
	}
	// The same team, with the provider its gate names funded, starts.
	q, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, q.apiKeys, q.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
	if err := resolvePolicy(t, q, humanGateBot); err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — the team funds exactly what its gate names", err)
	}
}

// The deployment knob (ITERION_CLOUD_REQUIRE_LLM_CREDENTIAL) was disarmed by
// the same emptied set, and keeps its own — permissive — rule: one funded
// provider lets the run start. What changed is the set it is computed on.
func TestResolve_RequireLLMCredential_ReadsTheAttributedSet(t *testing.T) {
	t.Run("a mixed workflow with nothing funded is refused", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, nil)
		p.requireLLMCredential = true
		err := resolvePolicy(t, p, mixedAttributableBot)
		var unfunded *UnfundedProvidersError
		if !errors.As(err, &unfunded) {
			t.Fatalf("resolveAndSealCredentials = %v, want an *UnfundedProvidersError: one unattributable route emptied the set and disarmed the knob too", err)
		}
		if unfunded.Sovereign {
			t.Error("the knob's refusal claims the sovereign posture")
		}
		if got := unfunded.Providers; len(got) != 2 || got[0] != "anthropic" || got[1] != "openai" {
			t.Fatalf("unfunded providers = %v, want [anthropic openai]", got)
		}
	})
	t.Run("one funded provider still lets the run start", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, nil)
		p.requireLLMCredential = true
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
		if err := resolvePolicy(t, p, mixedAttributableBot); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — the knob's compromise is deliberate: the other route's failure is the run's to report", err)
		}
	})
}

// #2783 — a tool node's `recovery: model:` is a route the walk reads: under
// `llm_fallback: none`, a recovery rung pinning a provider the team funds
// nothing of refuses the launch, naming the provider AND the node. `verify`
// pins the anthropic the team holds, so the refusal can only come from the
// rungs' own routes — which is what makes this a witness of the reader
// (recoveryRungsOf), not of the agent arm. Mutant: the recovery arm dropped
// from EffectiveProviders → the refusal disappears.
const recoveryRungBot = "tool heal:\n  command: \"make build\"\n  goal: \"the build succeeds\"\n  postcondition: \"test -f build.ok\"\n  policy: recover\n  recovery:\n    max_repair_attempts: 1\n    max_agent_attempts: 1\n    model: \"openai/gpt-5.4-mini\"\n\nagent verify:\n  model: \"anthropic/claude-opus-5\"\n  backend: \"claw\"\n\nworkflow main:\n  heal -> verify\n  verify -> done\n"

func TestResolve_LLMFallbackNone_ARecoveryRungsModelIsRefusedByName(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")

	err := resolvePolicy(t, p, recoveryRungBot)
	var unfunded *UnfundedProvidersError
	if !errors.As(err, &unfunded) {
		t.Fatalf("resolveAndSealCredentials = %v, want an *UnfundedProvidersError naming openai: the recovery rungs' `model:` spends a provider the team funds nothing of", err)
	}
	if got := unfunded.Providers; len(got) != 1 || got[0] != "openai" {
		t.Fatalf("unfunded providers = %v, want exactly [openai] — `verify`'s anthropic IS funded by the team's own key", got)
	}
	// Provider AND node: each enabled rung names its own site, so the
	// operator reading "provision openai" learns which node spends it.
	wantSites := []string{"heal#agent_recovery", "heal#self_repair"}
	if sites := unfunded.Sites["openai"]; len(sites) != 2 || sites[0] != wantSites[0] || sites[1] != wantSites[1] {
		t.Errorf("sites naming openai = %v, want %v — the refusal must name the node and the rung", sites, wantSites)
	}
	// The same team, funding what its rungs name, starts.
	q, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedKeyFP(t, q.apiKeys, q.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
	seedKeyFP(t, q.apiKeys, q.sealer, "team1", secrets.ProviderOpenAI, "sk-openai-team", "fp-openai-team")
	if err := resolvePolicy(t, q, recoveryRungBot); err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil — the team funds exactly what its rungs name", err)
	}
}

// #2783's other half — the walk prices only what dispatch can spend. The
// same bot with a REGISTRY recipe (`command: "noop_tool"`: no space, no
// slash, so a registered tool name) gives rung 3 no command to correct, so
// dispatch skips the rung and the launch must start: refusing it named a
// provider the run could never reach, for a workflow that launched on the
// release before. Mutant: drop the recipe-kind conjunct from
// ir.ToolRecoverySelfRepairRung → the first half fails with the phantom
// openai refusal.
const registryRecipeRung3Bot = `tool heal:
  command: "noop_tool"
  goal: "the build succeeds"
  postcondition: "test -f build.ok"
  policy: recover
  recovery:
    max_repair_attempts: 1
    max_agent_attempts: 0
    model: "openai/gpt-5.4-mini"

agent verify:
  model: "anthropic/claude-opus-5"
  backend: "claw"

workflow main:
  heal -> verify
  verify -> done
`

// The same registry recipe with rung 4 budgeted instead: an agent achieves
// the goal with real tools whatever the recipe was, so THAT rung is a real
// route and the unfunded refusal must still name it.
const registryRecipeRung4Bot = `tool heal:
  command: "noop_tool"
  goal: "the build succeeds"
  postcondition: "test -f build.ok"
  policy: recover
  recovery:
    max_repair_attempts: 0
    max_agent_attempts: 1
    model: "openai/gpt-5.4-mini"

agent verify:
  model: "anthropic/claude-opus-5"
  backend: "claw"

workflow main:
  heal -> verify
  verify -> done
`

func TestResolve_LLMFallbackNone_ARegistryRecipeIsRefusedForRungFourAlone(t *testing.T) {
	launch := func(t *testing.T, src string) error {
		t.Helper()
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		seedKeyFP(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team", "fp-ant-team")
		return resolvePolicy(t, p, src)
	}

	if err := launch(t, registryRecipeRung3Bot); err != nil {
		t.Fatalf("resolveAndSealCredentials = %v, want nil: a registry recipe has no self-repair rung for dispatch to spend, so its `recovery.model` is no route to refuse", err)
	}

	err := launch(t, registryRecipeRung4Bot)
	var unfunded *UnfundedProvidersError
	if !errors.As(err, &unfunded) {
		t.Fatalf("resolveAndSealCredentials = %v, want an *UnfundedProvidersError: rung 4 runs for a registry recipe, so its model is a route", err)
	}
	if sites := unfunded.Sites["openai"]; len(sites) != 1 || sites[0] != "heal#agent_recovery" {
		t.Errorf("sites naming openai = %v, want exactly [heal#agent_recovery] — the self-repair site is a rung dispatch cannot run", sites)
	}
}
