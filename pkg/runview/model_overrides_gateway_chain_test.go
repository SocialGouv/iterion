package runview

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// chainHeadBot is a claw node on a literal vendor model; the cases write
// its backend and its own provider chain.
const chainHeadBot = `
agent work:
  backend: "claw"
  model: "anthropic/claude-opus-5"
  system: p
  tools: [read_file]

prompt p:
  Do the work.

workflow gwhint:
  entry: work
  work -> done
`

// The launch judges the provider steps dispatch builds: a backend that does
// not walk a hint-only chain (claw, and an unnamed backend the runner
// decides) sends the head alone (collapseHintOnlyChain), so a step behind
// it is never built and refuses nothing; claude_code walks the chain, so
// every step is built and judged.
func TestLaunchJudgesTheProviderStepsDispatchBuilds(t *testing.T) {
	withBackend := func(b string) string {
		return strings.Replace(chainHeadBot, `backend: "claw"`, `backend: "`+b+`"`, 1)
	}
	for _, tc := range []struct {
		name   string
		src    string
		cell   store.RunLLMRouteOverride
		refuse []string // nil: admitted, and the write screen stays silent
	}{
		{"claw: a vendor step behind the gateway head is never built", chainHeadBot,
			store.RunLLMRouteOverride{Selector: "work", Provider: "openai_compatible,anthropic", Model: gatewayTestModel}, nil},
		{"claw: the bot's own chain, the gateway model from a cell", strings.Replace(chainHeadBot, "  system: p\n", "  provider: \"openai_compatible,anthropic\"\n  system: p\n", 1),
			store.RunLLMRouteOverride{Selector: "work", Model: gatewayTestModel}, nil},
		{"unnamed backend: the runner's claw sends the head alone", strings.Replace(chainHeadBot, "  backend: \"claw\"\n", "", 1),
			store.RunLLMRouteOverride{Selector: "work", Provider: "openai_compatible,anthropic", Model: gatewayTestModel}, nil},
		{"dialed backend: the head is the step every expansion builds", withBackend("${G_BACKEND:-claw}"),
			store.RunLLMRouteOverride{Selector: "work", Provider: "openai_compatible,anthropic", Model: gatewayTestModel}, nil},
		{"claw: the head is still judged", chainHeadBot,
			store.RunLLMRouteOverride{Selector: "work", Provider: "anthropic,openai_compatible", Model: gatewayTestModel}, []string{`"anthropic"`, "names another wire"}},
		{"claude_code walks the chain: its tail is judged", withBackend("claude_code"),
			store.RunLLMRouteOverride{Selector: "work", Provider: "anthropic,openai_compatible"}, []string{`"openai_compatible"`, "not a gateway route"}},
		{"a cell's claude_code over the bot's claw walks the chain", chainHeadBot,
			store.RunLLMRouteOverride{Selector: "work", Backend: "claude_code", Provider: "anthropic,openai_compatible"}, []string{`"openai_compatible"`, "not a gateway route"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cells := []store.RunLLMRouteOverride{tc.cell}
			pub, err := launchGatewayBot(t, "gwhint", tc.src, cells, nil)
			warnings := strings.Join(screenGatewayBot(t, tc.src, cells), "\n")
			if tc.refuse == nil {
				requireAdmitted(t, pub, err)
				if strings.Contains(warnings, "dispatch refuses") {
					t.Errorf("write screen warns on a step dispatch never builds: %q", warnings)
				}
				return
			}
			requireRefusal(t, pub, err, tc.refuse...)
		})
	}
}

// A gateway model a cell pins beside a vendor hint the bot wrote names two
// wires; the refusal names what the cell can write — the gateway hint, and
// the claw backend where the element's backend cannot serve a gateway
// route — and the cell written that way launches.
func TestLaunchRefusalNamesTheCellsRemedyForABotHintBesideACellGatewayModel(t *testing.T) {
	route := "  fallbacks:\n    glm:\n      model: \"glm-5.3\"\n      provider: \"zai\"\n"
	clawBot := strings.Replace(chainHeadBot, "  tools: [read_file]\n", "  tools: [read_file]\n"+route, 1)
	for _, tc := range []struct {
		name, src, remedy string
		fixed             store.RunLLMRouteOverride
	}{
		{"a claw route", clawBot, `pin "provider": "openai_compatible" in the cell`,
			store.RunLLMRouteOverride{Selector: "gwhint/work#glm", Provider: "openai_compatible", Model: gatewayTestModel}},
		{"a route on a claude_code node", gatewayHintBot, `pin "provider": "openai_compatible" and "backend": "claw" in the cell`,
			store.RunLLMRouteOverride{Selector: "gwhint/work#glm", Backend: "claw", Provider: "openai_compatible", Model: gatewayTestModel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub, err := launchGatewayBot(t, "gwhint", tc.src, []store.RunLLMRouteOverride{{Selector: "gwhint/work#glm", Model: gatewayTestModel}}, nil)
			requireRefusal(t, pub, err, `"work#glm"`, "names another wire", `provider "zai" from the route's own provider field`, tc.remedy)
			pub, err = launchGatewayBot(t, "gwhint", tc.src, []store.RunLLMRouteOverride{tc.fixed}, nil)
			requireAdmitted(t, pub, err)
		})
	}
	// The gateway hint the bot wrote beside a cell's vendor model: the hint
	// is already the gateway's, dispatch's own remedy (a gateway model)
	// stands, and no hint is named.
	t.Run("the bot's gateway hint beside a cell's vendor model", func(t *testing.T) {
		src := strings.NewReplacer("  system: p\n", "  provider: \"openai_compatible\"\n  system: p\n", `model: "anthropic/claude-opus-5"`, `model: "`+gatewayTestModel+`"`).Replace(chainHeadBot)
		pub, err := launchGatewayBot(t, "gwhint", src, []store.RunLLMRouteOverride{{Selector: "gwhint/work", Model: "anthropic/claude-sonnet-5"}}, nil)
		requireRefusal(t, pub, err, "is not a gateway route", "use openai_compatible/<gateway model id>")
		if err != nil && strings.Contains(err.Error(), `pin "provider"`) {
			t.Errorf("error = %q names a hint remedy beside the gateway hint itself", err)
		}
	})
}
