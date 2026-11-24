package runview

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// A stored cell the write gate refuses for a gateway step on a model that
// is not a gateway route folds at launch (llmroute.ValidateStored) and is
// judged per node on the elements dispatch builds: a claw chain sends its
// head alone, so the dead tail refuses nothing; claude_code walks the chain
// and builds the tail, which every dispatch refuses — refused, naming the
// cell; a bot with no such node is not refused for it.
func TestLaunchJudgesAStoredDeadGatewayStepPerNode(t *testing.T) {
	cells := []store.RunLLMRouteOverride{{Selector: "work", Provider: "zai,openai_compatible:scaleway/deepseek-v4.1-flash"}}
	pub, err := launchGatewayBot(t, "gwhint", chainHeadBot, cells, nil)
	requireAdmitted(t, pub, err)
	claudeCode := strings.Replace(chainHeadBot, `backend: "claw"`, `backend: "claude_code"`, 1)
	pub, err = launchGatewayBot(t, "gwhint", claudeCode, cells, nil)
	requireRefusal(t, pub, err, `agent "work"`, `model "scaleway/deepseek-v4.1-flash" is not a gateway route`, `the routing policy's override cell "work"`)
	pub, err = launchGatewayBot(t, "gwhint", strings.NewReplacer("agent work:", "agent draft:", "entry: work", "entry: draft", "work -> done", "draft -> done").Replace(chainHeadBot), cells, nil)
	requireAdmitted(t, pub, err)
}

// launchGatewayBotUnderProviders is launchGatewayBot under a policy that
// also sets the run's provider allowlist.
func launchGatewayBotUnderProviders(t *testing.T, src string, cells []store.RunLLMRouteOverride, providers []string) (*stubLaunchPublisher, error) {
	t.Helper()
	pub := &stubLaunchPublisher{}
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	pol := &store.RunLLMRoutePolicy{Overrides: cells, Providers: providers, Sources: map[string]string{llmroute.FieldProviders: "team"}}
	_, launchErr := svc.Launch(context.Background(), LaunchSpec{FilePath: "gwhint.bot", Source: src, BotID: "gwhint", LLMRoutePolicy: pol})
	return pub, launchErr
}

// Dispatch drops the chain elements the run's provider allowlist forbids
// (dropForbidden) before it collapses a hint-only chain to its head: under
// [openai_compatible] a claw node sends the first allowed step, so a step
// behind a dropped head is built and judged, and a refused head the list
// drops is never built.
func TestLaunchJudgesTheStepsTheProviderAllowlistLeaves(t *testing.T) {
	allow := []string{"openai_compatible"}
	deadTail := []store.RunLLMRouteOverride{{Selector: "work", Provider: "zai,openai_compatible:scaleway/deepseek-v4.1-flash"}}
	pub, err := launchGatewayBotUnderProviders(t, chainHeadBot, deadTail, allow)
	requireRefusal(t, pub, err, `agent "work"`, `model "scaleway/deepseek-v4.1-flash" is not a gateway route`, `the routing policy's override cell "work"`, "the provider allowlist (team level) drops the steps ahead of it")
	pub, err = launchGatewayBotUnderProviders(t, chainHeadBot, deadTail, nil)
	requireAdmitted(t, pub, err)

	refusedHead := []store.RunLLMRouteOverride{{Selector: "work", Provider: "anthropic,openai_compatible", Model: gatewayTestModel}}
	pub, err = launchGatewayBotUnderProviders(t, chainHeadBot, refusedHead, allow)
	requireAdmitted(t, pub, err)
	pub, err = launchGatewayBotUnderProviders(t, chainHeadBot, refusedHead, nil)
	requireRefusal(t, pub, err, `"anthropic"`, "names another wire")

	// The bot's own chain: no cell writes a field, the allowlist alone
	// builds the dead tail the bot's claw never sends.
	botChain := strings.Replace(chainHeadBot, "  system: p\n", "  provider: \"anthropic,openai_compatible\"\n  system: p\n", 1)
	pub, err = launchGatewayBotUnderProviders(t, botChain, nil, allow)
	requireRefusal(t, pub, err, `agent "work"`, `model "anthropic/claude-opus-5" is not a gateway route`, `provider "openai_compatible" from the node's own provider field`, "the provider allowlist (team level) drops the steps ahead of it")
	pub, err = launchGatewayBotUnderProviders(t, botChain, nil, []string{"anthropic", "openai_compatible"})
	requireAdmitted(t, pub, err)
}

// A cell's backend that walks the provider chain the bot's own backend
// collapses builds the bot's steps behind the head: a refusal of such a
// step is the cell's, though the bot wrote its hint and model.
func TestLaunchRefusesTheBotsStepAnOverrideBackendBuilds(t *testing.T) {
	botChain := strings.Replace(chainHeadBot, "  system: p\n", "  provider: \"anthropic,openai_compatible\"\n  system: p\n", 1)
	pub, err := launchGatewayBot(t, "gwhint", botChain, []store.RunLLMRouteOverride{{Selector: "gwhint/work", Effort: "high"}}, nil)
	requireAdmitted(t, pub, err)
	cells := []store.RunLLMRouteOverride{{Selector: "gwhint/work", Backend: "claude_code"}}
	pub, err = launchGatewayBot(t, "gwhint", botChain, cells, nil)
	requireRefusal(t, pub, err, `agent "work"`, `model "anthropic/claude-opus-5" is not a gateway route`, `backend "claude_code" from the routing policy's override cell "gwhint/work"`, `which walks the provider chain the bot's own backend collapses to its head`)
	if warnings := strings.Join(screenGatewayBot(t, botChain, cells), "\n"); !strings.Contains(warnings, "dispatch refuses") || !strings.Contains(warnings, `backend "claude_code"`) {
		t.Errorf("write screen = %q, want the step the cell's backend builds warned", warnings)
	}
}

// A vendor hint a route writes beside the gateway model it inherits from a
// node cell: the node cell already pins the gateway hint for the node, so
// the remedy names a cell on the route, and the cell written that way
// launches. A route that names a backend names its own model (C173), so a
// node cell takes part in no refusal of a route's own backend.
func TestLaunchRefusalNamesARouteCellForTheRoutesOwnHint(t *testing.T) {
	nodeCell := store.RunLLMRouteOverride{Selector: "gwhint/work", Backend: "claw", Provider: "openai_compatible", Model: gatewayTestModel}
	src := strings.Replace(chainHeadBot, "  tools: [read_file]\n", "  tools: [read_file]\n  fallbacks:\n    glm:\n      provider: \"zai\"\n", 1)
	pub, err := launchGatewayBot(t, "gwhint", src, []store.RunLLMRouteOverride{nodeCell}, nil)
	requireRefusal(t, pub, err, `"work#glm"`, `provider "zai" from the route's own provider field`, `the routing policy's override cell "gwhint/work"`,
		`pin "provider": "openai_compatible" in a cell on the route ("gwhint/work#glm")`)
	pub, err = launchGatewayBot(t, "gwhint", src, []store.RunLLMRouteOverride{nodeCell, {Selector: "gwhint/work#glm", Provider: "openai_compatible"}}, nil)
	requireAdmitted(t, pub, err)
}

// A step the bot's own reading builds stays the bot's: a backend the bot
// names that walks its chain builds its tail under no cell, so a cell that
// writes no field the refusing arm reads adds no launch refusal. A step
// the allowlist alone builds, beside no cell, is refused with dispatch's
// own remedy: no cell exists to pin one in.
func TestLaunchKeepsTheStepsTheBotsOwnReadingBuilds(t *testing.T) {
	walked := strings.NewReplacer(`backend: "claw"`, `backend: "claude_code"`, `model: "anthropic/claude-opus-5"`, `model: "claude-opus-5"`, "  system: p\n", "  provider: \"anthropic,openai_compatible\"\n  system: p\n").Replace(chainHeadBot)
	pub, err := launchGatewayBot(t, "gwhint", walked, []store.RunLLMRouteOverride{{Selector: "gwhint/work", Effort: "high"}}, nil)
	requireAdmitted(t, pub, err)

	gwChain := strings.NewReplacer(`model: "anthropic/claude-opus-5"`, `model: "`+gatewayTestModel+`"`, "  system: p\n", "  provider: \"openai_compatible,anthropic\"\n  system: p\n").Replace(chainHeadBot)
	pub, err = launchGatewayBotUnderProviders(t, gwChain, nil, []string{"anthropic"})
	requireRefusal(t, pub, err, `the route's provider hint "anthropic" names another wire`, "the provider allowlist (team level) drops the steps ahead of it")
	if err != nil && strings.Contains(err.Error(), `pin "provider"`) {
		t.Errorf("error = %q names a cell remedy where no cell takes part", err)
	}
}

// A provider a dial decides: the steps the runner expands it to decide
// what the allowlist drops, so the launch judges the element every
// expansion builds — here a cell's gateway model on the bot's claude_code,
// which dispatch refuses whether the list keeps the step or drops the
// node's last route. A model a dial decides beside no hint names the
// provider the drop reads only at the runner: the element is kept, the
// process environment unread.
func TestTheAllowlistDropReadsNoDialedValue(t *testing.T) {
	dialed := strings.NewReplacer(`backend: "claw"`, `backend: "claude_code"`, `model: "anthropic/claude-opus-5"`, `model: "claude-opus-5"`, "  system: p\n", "  provider: \"${G_P:-anthropic}\"\n  system: p\n").Replace(chainHeadBot)
	pub, err := launchGatewayBotUnderProviders(t, dialed, []store.RunLLMRouteOverride{{Selector: "work", Model: gatewayTestModel}}, []string{"anthropic"})
	requireRefusal(t, pub, err, `agent "work"`, "cannot serve an OpenAI-compatible gateway route", `the routing policy's override cell "work"`)

	t.Setenv("G_M", "")
	list := gatewayAllowlist{set: map[string]bool{"openai_compatible": true}}
	if list.drops(gatewayElement{model: gatewayField{value: "${G_M:-anthropic/claude-opus-5}"}}) {
		t.Error("a dialed model beside no hint dropped on the dial's expansion")
	}
	if !list.drops(gatewayElement{model: gatewayField{value: "anthropic/claude-opus-5"}}) {
		t.Error("a literal anthropic model beside no hint kept under [openai_compatible]")
	}
}
