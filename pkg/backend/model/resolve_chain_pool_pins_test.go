package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
)

// recordOverlay is the platform bot-vars record a pod would resolve.
func recordOverlay(vars map[string]string) func(string) (string, bool) {
	res := platformcfg.NewResolverFunc[platformcfg.BotVars](func(context.Context) (*platformcfg.BotVars, error) {
		return &platformcfg.BotVars{Vars: vars}, nil
	}, nil)
	return platformcfg.BotVarsOverlay(res, nil)
}

// The dispatch chain of a pool pod reads the pool's own dials: with the
// record pinning the shared fleet's vendor id and the pod env naming the
// gateway on all three dials, the node's own route is gateway model +
// gateway hint, and the authored vendor rescue keeps the rescue dial. The
// same record WITHOUT the pins is the crossing the dispatch refuses: the
// vendor id under the gateway hint.
func TestResolveChain_PoolPinsBeatThePlatformRecord(t *testing.T) {
	record := recordOverlay(map[string]string{
		"ITERION_VIBE_MODEL_CLAUDE":  "zai/glm-5.3",
		"ITERION_VIBE_EFFORT_CLAUDE": "max",
	})
	t.Setenv("ITERION_VIBE_MODEL_CLAUDE", "openai_compatible/scaleway/glm-5.3")
	t.Setenv("ITERION_VIBE_PROVIDER_CLAUDE", "openai_compatible")
	t.Setenv("ITERION_VIBE_MODEL_CLAUDE_FALLBACK", "openai_compatible/scaleway/glm-5.3")
	t.Cleanup(func() { ir.SetEnvOverlay(nil) })

	node := chainAgentNode("reviewer_claude", "claw", "${ITERION_VIBE_PROVIDER_CLAUDE:-anthropic}", []ir.Fallback{{
		Name: "glm", Provider: "zai", Model: "${ITERION_VIBE_MODEL_CLAUDE_FALLBACK:-glm-5.3}",
	}})
	node.Model = "${ITERION_VIBE_MODEL_CLAUDE:-claude-opus-5-5}"
	e := &ClawExecutor{}

	pins, malformed := platformcfg.ParseEnvPins("ITERION_VIBE_MODEL_CLAUDE,ITERION_VIBE_MODEL_CLAUDE_FALLBACK,ITERION_VIBE_PROVIDER_CLAUDE")
	if len(malformed) != 0 {
		t.Fatalf("pin list refused: %q", malformed)
	}
	ir.SetEnvOverlay(platformcfg.PinnedOverlay(pins, record))
	chain := e.resolveChain(node)
	if len(chain) != 2 {
		t.Fatalf("chain = %+v, want the node's route and the glm rescue", chain)
	}
	if got := e.baselineModel(node); got != "openai_compatible/scaleway/glm-5.3" {
		t.Errorf("pinned pod: baseline model = %q, want the pool's gateway id (the record reached a pinned name)", got)
	}
	if chain[0].Provider != "openai_compatible" || chain[0].Model != "" {
		t.Errorf("pinned pod: route 1 = %+v, want the gateway hint inheriting the node model", chain[0])
	}
	if chain[1].Provider != "zai" || chain[1].Model != "openai_compatible/scaleway/glm-5.3" {
		t.Errorf("pinned pod: rescue = %+v, want the authored zai hint on the rescue dial's value", chain[1])
	}
	if err := refuseGatewayCrossing("claw", chain[0].Provider, e.baselineModel(node)); err != nil {
		t.Errorf("pinned pod: the node's own route is refused: %v", err)
	}

	// The same pod without pins: the record's vendor id lands under the
	// pod's gateway hint — the crossing the dispatch refuses at build.
	ir.SetEnvOverlay(record)
	if got := e.baselineModel(node); got != "zai/glm-5.3" {
		t.Fatalf("unpinned pod: baseline model = %q, want the record's %q — the test no longer stages the crossing", got, "zai/glm-5.3")
	}
	if err := refuseGatewayCrossing("claw", e.resolveChain(node)[0].Provider, e.baselineModel(node)); err == nil {
		t.Errorf("unpinned pod: the vendor id under the gateway hint must be refused")
	}
}
