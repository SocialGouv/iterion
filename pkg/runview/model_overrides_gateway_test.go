package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The gateway crossing check: dispatch refuses an element whose (backend,
// provider hint, model) crosses the gateway (model.GatewayCrossingRefusal),
// whatever is funded. A triple the run's override cells compose that way is
// refused at launch when its values are decided, warned when a slot dial
// decides one of them, and warned at the write screen — never refused
// there.

const gatewayTestModel = "openai_compatible/scaleway/deepseek-v4.1-flash"

// gatewayHintBot is a claude_code node on a literal vendor model with a
// z.ai rescue route.
const gatewayHintBot = `
agent work:
  backend: "claude_code"
  model: "claude-opus-5"
  system: p
  tools: [read_file]
  fallbacks:
    glm:
      model: "glm-5.3"
      provider: "zai"

prompt p:
  Do the work.

workflow gwhint:
  entry: work
  work -> done
`

// launchGatewayBot drives Service.Launch on src for bot under the cells.
func launchGatewayBot(t *testing.T, bot, src string, cells []store.RunLLMRouteOverride, rows []ModelOverrideEntry) (*stubLaunchPublisher, error) {
	t.Helper()
	pub := &stubLaunchPublisher{}
	svc, err := NewService(t.TempDir(), WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	var pol *store.RunLLMRoutePolicy
	if cells != nil {
		pol = &store.RunLLMRoutePolicy{Overrides: cells}
	}
	_, launchErr := svc.Launch(context.Background(), LaunchSpec{
		FilePath: bot + ".bot", Source: src, BotID: bot,
		ModelOverrides: rows, LLMRoutePolicy: pol,
	})
	return pub, launchErr
}

// screenGatewayBot runs the integration write screen on src under cells.
func screenGatewayBot(t *testing.T, src string, cells []store.RunLLMRouteOverride) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.bot")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	warnings, _, err := ScreenPolicyOverrides(path, src, "", "", botIdentityFor("gwhint"), &store.RunLLMRoutePolicy{Overrides: cells})
	if err != nil {
		t.Fatalf("the write screen must not refuse a stored cell: %v", err)
	}
	return warnings
}

func requireRefusal(t *testing.T, pub *stubLaunchPublisher, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("admitted at launch (reached publisher=%v), want a refusal naming %q", pub.lastSpec != nil, want)
	}
	if pub.lastSpec != nil {
		t.Fatal("the refused launch reached the publisher")
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error = %q, want it to name %q", err, w)
		}
	}
}

func requireAdmitted(t *testing.T, pub *stubLaunchPublisher, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("launch refused: %v", err)
	}
	if pub.lastSpec == nil {
		t.Fatal("the admitted launch never reached the publisher")
	}
}

// A claw cell pinning the gateway hint, the model left to the bot's
// literal non-gateway `model:` (node), or to the route's own (route):
// every dispatch refuses the element.
func TestLaunchRefusesAGatewayHintBesideTheBotsNonGatewayModel(t *testing.T) {
	t.Run("node", func(t *testing.T) {
		pub, err := launchGatewayBot(t, "gwhint", gatewayHintBot, []store.RunLLMRouteOverride{
			{Selector: "gwhint/work", Backend: "claw", Provider: "openai_compatible"},
		}, nil)
		requireRefusal(t, pub, err, `agent "work"`, "openai_compatible", `"claude-opus-5"`, `the routing policy's override cell "gwhint/work"`)
	})
	t.Run("route", func(t *testing.T) {
		pub, err := launchGatewayBot(t, "gwhint", gatewayHintBot, []store.RunLLMRouteOverride{
			{Selector: "gwhint/work", Backend: "claw", Provider: "openai_compatible", Model: gatewayTestModel},
			{Selector: "gwhint/work#glm", Backend: "claw", Provider: "openai_compatible"},
		}, nil)
		requireRefusal(t, pub, err, `"work#glm"`, "openai_compatible", `"glm-5.3"`, `the routing policy's override cell "gwhint/work#glm"`)
	})
}

// The model a gateway hint pairs with may come from another cell: the
// composed triple is what dispatch serves.
func TestLaunchKeepsAGatewayHintWhoseModelAnotherCellPins(t *testing.T) {
	pub, err := launchGatewayBot(t, "gwhint", gatewayHintBot, []store.RunLLMRouteOverride{
		{Selector: "gwhint/*", Model: gatewayTestModel},
		{Selector: "gwhint/work", Backend: "claw", Provider: "openai_compatible"},
	}, nil)
	requireAdmitted(t, pub, err)
}

// A launch row's `hint:model` step carries its own model, and that step is
// judged on it — not on the model a cell pins beside it.
func TestLaunchJudgesARowStepOnItsOwnModel(t *testing.T) {
	pub, err := launchGatewayBot(t, "gwhint", gatewayHintBot,
		[]store.RunLLMRouteOverride{{Selector: "work", Model: gatewayTestModel}},
		[]ModelOverrideEntry{{Selector: "work", Backend: "claw", Provider: "openai_compatible:claude-opus-5"}})
	requireRefusal(t, pub, err, `agent "work"`, `"claude-opus-5"`, `the launch's own override row "work"`)
}

// A slot dial decides the model at the runner: the launch cannot refuse
// what the pod's environment may serve, so it admits and warns.
func TestLaunchAdmitsAGatewayHintBesideADialModelAndWarns(t *testing.T) {
	src := strings.Replace(gatewayHintBot, `model: "claude-opus-5"`, `model: "${PROBE_E_MODEL:-claude-opus-5}"`, 1)
	cells := []store.RunLLMRouteOverride{{Selector: "gwhint/work", Backend: "claw", Provider: "openai_compatible"}}
	pub, err := launchGatewayBot(t, "gwhint", src, cells, nil)
	requireAdmitted(t, pub, err)
	wf, path, cerr := compileRouteCrossingBot(t, src)
	if cerr != nil {
		t.Fatal(cerr)
	}
	warnings, ferr := foldAndScreen(wf, &LaunchSpec{FilePath: path, Source: src, BotID: "gwhint", LLMRoutePolicy: &store.RunLLMRoutePolicy{Overrides: cells}}, "")
	if ferr != nil {
		t.Fatalf("foldAndScreen: %v", ferr)
	}
	if joined := strings.Join(warnings, "\n"); !strings.Contains(joined, "PROBE_E_MODEL") || !strings.Contains(joined, "not a gateway route") {
		t.Fatalf("launch warnings = %q, want the dial named beside the crossing its default makes", warnings)
	}
}

// A crossing the bot alone writes is the compiler's (C186) and dispatch's:
// a cell that writes none of the fields the refusing arm reads adds no
// launch refusal — an effort cell, or a backend cell beside a two-wire
// pair whose refusal reads only the hint and the model.
func TestLaunchIgnoresABotOnlyGatewayCrossing(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		cell store.RunLLMRouteOverride
	}{
		{"claude_code beside a gateway model", strings.Replace(splitPairBot, `model: "claude-opus-5"`, `model: "`+gatewayTestModel+`"`, 1), store.RunLLMRouteOverride{Selector: "work", Effort: "high"}},
		{"gateway hint beside a vendor model", strings.Replace(splitPairBot, `backend: "claude_code"`, "backend: \"claw\"\n  provider: \"openai_compatible\"", 1), store.RunLLMRouteOverride{Selector: "work", Effort: "high"}},
		{"vendor hint beside a gateway model, the backend re-pinned", strings.NewReplacer(`backend: "claude_code"`, "backend: \"claw\"\n  provider: \"anthropic\"", `model: "claude-opus-5"`, `model: "`+gatewayTestModel+`"`).Replace(splitPairBot), store.RunLLMRouteOverride{Selector: "work", Backend: "claw"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub, err := launchGatewayBot(t, "gwhint", tc.src, []store.RunLLMRouteOverride{tc.cell}, nil)
			requireAdmitted(t, pub, err)
		})
	}
}

// A cell moving a literal claude_code node onto the gateway, the backend
// left to the bot: dispatch refuses every attempt. The same cell naming
// claude_code itself is refused the same way, the backend now the cell's.
func TestLaunchRefusesAGatewayCellOnAClaudeCodeNode(t *testing.T) {
	t.Run("the backend left to the bot", func(t *testing.T) {
		pub, err := launchGatewayBot(t, "gwhint", splitPairBot, []store.RunLLMRouteOverride{
			{Selector: "work", Provider: "openai_compatible", Model: gatewayTestModel},
		}, nil)
		requireRefusal(t, pub, err, `"claude_code"`, "cannot serve an OpenAI-compatible gateway route", `the routing policy's override cell "work"`, "the node's own backend field")
	})
	t.Run("the cell naming claude_code", func(t *testing.T) {
		pub, err := launchGatewayBot(t, "gwhint", splitPairBot, []store.RunLLMRouteOverride{
			{Selector: "work", Backend: "claude_code", Provider: "openai_compatible", Model: gatewayTestModel},
		}, nil)
		requireRefusal(t, pub, err, `backend "claude_code" from the routing policy's override cell "work"`, "cannot serve an OpenAI-compatible gateway route")
	})
}

// The write screen says so, and stays a warning: a stored record is not
// refused at write for a pair dispatch refuses.
func TestScreenPolicyOverridesWarnsOnTheGatewayCrossing(t *testing.T) {
	warnings := screenGatewayBot(t, splitPairBot, []store.RunLLMRouteOverride{
		{Selector: "work", Provider: "openai_compatible", Model: gatewayTestModel},
	})
	if !strings.Contains(strings.Join(warnings, "\n"), "cannot serve an OpenAI-compatible gateway route") {
		t.Fatalf("warnings = %q, want the gateway crossing named", warnings)
	}
}

// A route naming no backend runs on its node's; a gateway route cell on it
// inherits the node's literal claude_code.
func TestLaunchRefusesAGatewayRouteCellOnAClaudeCodeNode(t *testing.T) {
	src := strings.Replace(splitPairBot, "  tools: [read_file]\n", "  tools: [read_file]\n  fallbacks:\n    r:\n      model: \"claude-sonnet-5\"\n", 1)
	pub, err := launchGatewayBot(t, "gwhint", src, []store.RunLLMRouteOverride{
		{Selector: "*/work#r", Provider: "openai_compatible", Model: gatewayTestModel},
	}, nil)
	requireRefusal(t, pub, err, `"work#r"`, `"claude_code"`, "cannot serve an OpenAI-compatible gateway route")
}

// A route naming its own claw backend serves the gateway, whatever its
// node runs on.
func TestLaunchAdmitsAGatewayRouteCellOnAClawRoute(t *testing.T) {
	src := strings.Replace(splitPairBot, "  tools: [read_file]\n", "  tools: [read_file]\n  fallbacks:\n    r:\n      backend: \"claw\"\n      model: \"anthropic/claude-sonnet-5\"\n", 1)
	pub, err := launchGatewayBot(t, "gwhint", src, []store.RunLLMRouteOverride{
		{Selector: "*/work#r", Provider: "openai_compatible", Model: gatewayTestModel},
	}, nil)
	requireAdmitted(t, pub, err)
}

// A vendor hint beside a gateway model names two wires: dispatch refuses
// it on every backend, so funding is not the remedy.
func TestLaunchRefusesAVendorHintBesideAGatewayModelCell(t *testing.T) {
	pub, err := launchGatewayBot(t, "gwhint", splitPairBot, []store.RunLLMRouteOverride{
		{Selector: "work", Backend: "claw", Provider: "anthropic", Model: gatewayTestModel},
	}, nil)
	requireRefusal(t, pub, err, `"anthropic"`, "names another wire")
	if err != nil && strings.Contains(err.Error(), "provision") {
		t.Errorf("error = %q, must not send the operator to fund a pair dispatch refuses", err)
	}
}

// A backend a slot dial decides is the runner's: the launch admits a
// gateway cell on it and warns with the dial's default.
func TestLaunchAdmitsAGatewayCellOnAnEnvDialedBackend(t *testing.T) {
	src := strings.Replace(splitPairBot, `backend: "claude_code"`, `backend: "${G_BACKEND:-claude_code}"`, 1)
	cells := []store.RunLLMRouteOverride{{Selector: "work", Provider: "openai_compatible", Model: gatewayTestModel}}
	pub, err := launchGatewayBot(t, "gwhint", src, cells, nil)
	requireAdmitted(t, pub, err)
	if warnings := screenGatewayBot(t, src, cells); !strings.Contains(strings.Join(warnings, "\n"), "G_BACKEND") {
		t.Fatalf("write warnings = %q, want the backend dial named", warnings)
	}
}

// A route sends its provider whole: a launch row pinning a `hint:model`
// step or a `,` chain on a route is refused before the fold, where a node
// row's steps stay its chain.
func TestLaunchRefusesAStepOrAChainOnARouteRow(t *testing.T) {
	for _, provider := range []string{"openai_compatible:" + gatewayTestModel, "zai,anthropic"} {
		t.Run(provider, func(t *testing.T) {
			pub, err := launchGatewayBot(t, "gwhint", gatewayHintBot, nil,
				[]ModelOverrideEntry{{Selector: "work#glm", Provider: provider}})
			requireRefusal(t, pub, err, `"work#glm"`, "a route's provider is one hint")
		})
	}
	pub, err := launchGatewayBot(t, "gwhint", gatewayHintBot, nil,
		[]ModelOverrideEntry{{Selector: "work", Provider: "zai,anthropic"}})
	requireAdmitted(t, pub, err)
}

// A stray ',' on a route row is no hint ("zai," reaches dispatch whole),
// and a template on one is no literal: the launch refuses both, naming the
// row. A node row keeps the stray ',': dispatch splits a node chain.
func TestLaunchRefusesAStrayCommaOrATemplateOnARouteRow(t *testing.T) {
	pub, err := launchGatewayBot(t, "gwhint", gatewayHintBot, nil, []ModelOverrideEntry{{Selector: "work#glm", Provider: "zai,"}})
	requireRefusal(t, pub, err, `"work#glm"`, "a route's provider is one hint", `write "provider": "zai"`)
	pub, err = launchGatewayBot(t, "gwhint", gatewayHintBot, nil, []ModelOverrideEntry{{Selector: "work#glm", Provider: "${R3V_UNSET:-zai,anthropic}"}})
	requireRefusal(t, pub, err, `"work#glm"`, "is a LITERAL")
	pub, err = launchGatewayBot(t, "gwhint", gatewayHintBot, nil, []ModelOverrideEntry{{Selector: "work", Provider: "zai,"}})
	requireAdmitted(t, pub, err)
}

// A gateway hint a cell pins beside a model a dial decides, on a literal
// claude_code node: dispatch refuses a gateway expansion (the backend
// cannot serve one) and any other (the gateway hint serves gateway models
// only), so the launch refuses whatever the dial says. On claw a gateway
// expansion serves: the launch admits and warns on the dial's default.
func TestLaunchRefusesAGatewayHintRefusedWhateverTheModelDialSays(t *testing.T) {
	src := strings.Replace(gatewayHintBot, `model: "claude-opus-5"`, `model: "${PROBE_GW_MODEL:-claude-opus-5}"`, 1)
	cells := []store.RunLLMRouteOverride{{Selector: "gwhint/work", Provider: "openai_compatible"}}
	pub, err := launchGatewayBot(t, "gwhint", src, cells, nil)
	requireRefusal(t, pub, err, `agent "work"`, `whatever model "${PROBE_GW_MODEL:-claude-opus-5}"`, "cannot serve an OpenAI-compatible gateway route", "is not a gateway route", `the routing policy's override cell "gwhint/work"`)

	claw := strings.Replace(src, `backend: "claude_code"`, `backend: "claw"`, 1)
	pub, err = launchGatewayBot(t, "gwhint", claw, cells, nil)
	requireAdmitted(t, pub, err)

	// A cell pinning claude_code with no gateway hint: a gateway expansion
	// is refused, any other model serves — the dial decides, the launch
	// admits.
	pub, err = launchGatewayBot(t, "gwhint", src, []store.RunLLMRouteOverride{{Selector: "gwhint/work", Backend: "claude_code"}}, nil)
	requireAdmitted(t, pub, err)
}
