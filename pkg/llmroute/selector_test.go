package llmroute_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

const selectorGateway = "openai_compatible/scaleway/deepseek-v4.1-flash"

// The override selector grammar at the write surface (ADR-132 §2 D21,
// ADR-131 §8): what a level may write, and the refusal naming why.
func TestValidate_TheSelectorGrammar(t *testing.T) {
	gw := func(sel string) llmroute.Override {
		return llmroute.Override{Selector: sel, Backend: "claw", Provider: "openai_compatible", Model: selectorGateway}
	}
	for _, o := range []llmroute.Override{
		gw("review-pr/reviewer_claude"),
		gw("review-pr/*"),
		gw("review-pr/judge"),
		gw("review-pr/reviewer_*"),
		gw("reviewer_claude"),
		gw("review-pr/reviewer_claude#glm"),
		gw("review-pr/converge#0"),
		gw("*/reviewer_claude#glm"),
		gw("revi-*/converse_agent#glm"),
		{Selector: "review-pr/supervisor:pacer", Model: selectorGateway},
		{Selector: "*/supervisor:pacer", Model: selectorGateway},
		{Selector: "review-pr/reviewer_claude#glm", Model: selectorGateway},
	} {
		if err := llmroute.Validate(llmroute.Policy{Overrides: []llmroute.Override{o}}); err != nil {
			t.Errorf("Validate(%+v) = %v, want accepted", o, err)
		}
	}
	for _, tc := range []struct {
		o    llmroute.Override
		want string
	}{
		{gw("reviewer_claude#glm"), "names no bot"},
		{llmroute.Override{Selector: "supervisor:pacer", Model: selectorGateway}, "names no bot"},
		{llmroute.Override{Selector: "review-pr/reviewer_claude#glm", Model: selectorGateway, Effort: "high"}, "effort"},
		{llmroute.Override{Selector: "review-pr/supervisor:pacer", Model: selectorGateway, Backend: "claw"}, "supervisor names a model only"},
		{llmroute.Override{Selector: "review-pr/supervisor:pacer", Model: selectorGateway, Provider: "openai_compatible"}, "supervisor names a model only"},
		{llmroute.Override{Selector: "review-pr/supervisor:pacer", Effort: "high"}, "supervisor names a model only"},
		{gw("/reviewer_claude"), "empty bot"},
		{gw("review-pr/"), "no target"},
		{gw("a/b/c"), "more than one"},
		{gw("review#pr/x"), "bot dimension"},
		{gw("review-pr/reviewer_*#glm"), "exactly"},
		{gw("review-pr/#glm"), "both named"},
		{gw("review-pr/reviewer_claude#"), "both named"},
		{llmroute.Override{Selector: "review-pr/supervisor:", Model: selectorGateway}, "names no supervisor"},
		{llmroute.Override{Selector: "review-pr/supervisor:pa*", Model: selectorGateway}, "exactly"},
		{gw("[/x"), "not a valid glob"},
		{llmroute.Override{Selector: "x", Backend: "claw", Provider: "openai_compatible", Model: "anthropic/claude-opus-5-5"}, "gateway hint serves gateway models only"},
		{llmroute.Override{Selector: "x", Provider: "openai_compatible", Model: "glm-5.3"}, "gateway hint serves gateway models only"},
		{llmroute.Override{Selector: "x", Backend: "claw", Provider: "zai", Model: selectorGateway}, "acts on none of this value"},
	} {
		err := llmroute.Validate(llmroute.Policy{Overrides: []llmroute.Override{tc.o}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%+v) = %v, want a refusal naming %q", tc.o, err, tc.want)
		}
	}
}

// The bot dimension reaches a launch's bot by its id or by the manifest
// name of the bundle it serves, exactly or by a glob over either; no bot
// part and `*/` reach every bot, a launch naming none included. The match
// ranks the reach, least specific first: every bot < glob < manifest name
// < bot id. The comparison is exact: case and `-`/`_` count.
func TestSelector_TheBotDimension(t *testing.T) {
	fork := llmroute.BotIdentity{ID: "review-pr-pool", Name: "review-pr"}
	for _, tc := range []struct {
		sel  string
		bot  llmroute.BotIdentity
		want llmroute.BotMatch
	}{
		{"reviewer_claude", llmroute.BotIdentity{ID: "review-pr"}, llmroute.BotMatchAll},
		{"reviewer_claude", llmroute.BotIdentity{}, llmroute.BotMatchAll},
		{"*/reviewer_claude", llmroute.BotIdentity{}, llmroute.BotMatchAll},
		{"*/reviewer_claude", fork, llmroute.BotMatchAll},
		{"review-pr/reviewer_claude", llmroute.BotIdentity{ID: "review-pr"}, llmroute.BotMatchID},
		{"review-pr/reviewer_claude", llmroute.BotIdentity{ID: "revi-converse"}, llmroute.BotMatchNone},
		{"review-pr/reviewer_claude", llmroute.BotIdentity{}, llmroute.BotMatchNone},
		{"review-pr/x", fork, llmroute.BotMatchName},
		{"review-pr-pool/x", fork, llmroute.BotMatchID},
		{"review-p?/x", fork, llmroute.BotMatchGlob},
		{"*-pool/x", fork, llmroute.BotMatchGlob},
		{"revi*/x#glm", llmroute.BotIdentity{ID: "revi-converse"}, llmroute.BotMatchGlob},
		{"*/supervisor:pacer", llmroute.BotIdentity{ID: "docs-refresh"}, llmroute.BotMatchAll},
		{"**/x", llmroute.BotIdentity{}, llmroute.BotMatchNone},
		{"Review-PR/x", llmroute.BotIdentity{ID: "review-pr"}, llmroute.BotMatchNone},
		{"review_pr/x", llmroute.BotIdentity{ID: "review-pr"}, llmroute.BotMatchNone},
	} {
		s, err := llmroute.ParseSelector(tc.sel)
		if err != nil {
			t.Fatalf("ParseSelector(%q): %v", tc.sel, err)
		}
		if got := s.MatchBot(tc.bot); got != tc.want {
			t.Errorf("%q.MatchBot(%+v) = %v, want %v", tc.sel, tc.bot, got, tc.want)
		}
	}
	order := []llmroute.BotMatch{llmroute.BotMatchNone, llmroute.BotMatchAll, llmroute.BotMatchGlob, llmroute.BotMatchName, llmroute.BotMatchID}
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] {
			t.Error("the bot matches are not ordered every bot < glob < manifest name < bot id")
			break
		}
	}
	s, _ := llmroute.ParseSelector("review-pr/reviewer_claude#glm")
	if s.Target != "reviewer_claude#glm" || s.Class != llmroute.SelectorRoute || s.Node != "reviewer_claude" || s.Route != "glm" {
		t.Errorf("ParseSelector(route) = %+v", s)
	}
	s, _ = llmroute.ParseSelector("review-pr/supervisor:pacer")
	if s.Target != "supervisor:pacer" || s.Class != llmroute.SelectorSupervisor || s.Supervisor != "pacer" {
		t.Errorf("ParseSelector(supervisor) = %+v", s)
	}
}

// IsGatewayModel is the write gate's reading of a gateway spec; dispatch
// reads modelroute's. The two agree on every shape.
func TestIsGatewayModel_AgreesWithDispatch(t *testing.T) {
	for _, spec := range []string{
		selectorGateway, "openai_compatible/x", "openai_compatible/", "openai_compatible", "/x",
		"anthropic/claude-opus-5-5", "glm-5.3", "OpenAI_Compatible/x", "", "openai_compatible//x",
	} {
		if got, want := llmroute.IsGatewayModel(spec), modelroute.Parse(spec).Gateway(); got != want {
			t.Errorf("IsGatewayModel(%q) = %v, dispatch reads %v", spec, got, want)
		}
	}
}

// A launch's own rows carry no bot dimension and obey the class field
// rules a routing cell obeys; the shapes dispatch honours stay accepted.
func TestValidateLaunchRow(t *testing.T) {
	for _, tc := range []struct{ sel, backend, provider, effort, want string }{
		{"review-pr/converge", "", "", "", `names bot "review-pr"`},
		{"*/converge", "", "", "", `names bot "*"`},
		{"converge#glm", "", "", "high", "effort"},
		{"supervisor:pacer", "claw", "", "", "supervisor names a model only"},
		{"supervisor:pacer", "", "anthropic", "", "supervisor names a model only"},
		{"converge#*", "", "", "", "exactly"},
	} {
		if err := llmroute.ValidateLaunchRow(tc.sel, tc.backend, tc.provider, tc.effort); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateLaunchRow(%q, %q, %q, %q) = %v, want a refusal naming %q", tc.sel, tc.backend, tc.provider, tc.effort, err, tc.want)
		}
	}
	for _, tc := range []struct{ sel, backend, provider, effort string }{
		{"*", "claw", "zai", "high"},
		{"agent", "", "", "high"},
		{"reviewer_*", "codex", "", ""},
		{"converge#glm", "claw", "openai_compatible", ""},
		{"supervisor:pacer", "", "", ""},
	} {
		if err := llmroute.ValidateLaunchRow(tc.sel, tc.backend, tc.provider, tc.effort); err != nil {
			t.Errorf("ValidateLaunchRow(%q, %q, %q, %q) = %v, want accepted", tc.sel, tc.backend, tc.provider, tc.effort, err)
		}
	}
}

// A cell naming the gateway hint names gateway models only, and a node
// chain step's own model (`openai_compatible:<model>`) is the model THAT
// step dispatches: it is judged too, not only the cell's `model`. A route
// pin carries no step (TestValidate_ARouteProviderIsOneHint).
func TestValidate_TheGatewayHintJudgesEachStepsOwnModel(t *testing.T) {
	for _, o := range []llmroute.Override{
		{Selector: "x/n", Backend: "claw", Provider: "openai_compatible:claude-opus-5-5"},
		{Selector: "x/n", Backend: "claw", Provider: "openai_compatible:claude-opus-5-5", Model: selectorGateway},
	} {
		err := llmroute.Validate(llmroute.Policy{Overrides: []llmroute.Override{o}})
		if err == nil || !strings.Contains(err.Error(), "gateway hint serves gateway models only") {
			t.Errorf("Validate(%+v) = %v, want a refusal naming the step's non-gateway model", o, err)
			continue
		}
		_, stepModel, _ := strings.Cut(o.Provider, ":")
		if !strings.Contains(err.Error(), stepModel) {
			t.Errorf("Validate(%+v) = %v, want the refusal to name the step's model %q", o, err, stepModel)
		}
	}
	// A step carrying its own gateway model is served on claw.
	ok := llmroute.Override{Selector: "x/n", Backend: "claw", Provider: "openai_compatible:" + selectorGateway, Model: selectorGateway}
	if err := llmroute.Validate(llmroute.Policy{Overrides: []llmroute.Override{ok}}); err != nil {
		t.Errorf("Validate(%+v) = %v, want accepted", ok, err)
	}
}

// A gateway step that carries its own gateway model, beside a cell `model`
// that is not a gateway route, stays refused — the cell's model is the one
// the node's routes naming no model inherit — and the refusal does not
// claim that dispatch refuses the step, which serves it.
func TestValidate_AGatewayStepBesideANonGatewayCellModelStaysRefused(t *testing.T) {
	o := llmroute.Override{Selector: "x/n", Backend: "claw", Provider: "openai_compatible:" + selectorGateway, Model: "anthropic/claude-opus-5-5"}
	err := llmroute.Validate(llmroute.Policy{Overrides: []llmroute.Override{o}})
	if err == nil || !strings.Contains(err.Error(), "anthropic/claude-opus-5-5") {
		t.Fatalf("Validate(%+v) = %v, want a refusal naming the cell's non-gateway model", o, err)
	}
	if strings.Contains(err.Error(), "dispatch refuses") {
		t.Errorf("Validate(%+v) = %v — the step carries its own gateway model, which dispatch serves", o, err)
	}
}

// A `fallbacks:` route's provider is one hint at dispatch, sent whole: the
// write gate and a launch's own row refuse a `,` chain and a `hint:model`
// step on a route pin, and keep both on a node pin, whose chain dispatch
// walks step by step.
func TestValidate_ARouteProviderIsOneHint(t *testing.T) {
	for _, tc := range []struct{ backend, provider, want string }{
		{"", "openai_compatible:" + selectorGateway, `"model": "` + selectorGateway + `"`},
		{"claw", "anthropic:anthropic/claude-opus-5-5", `"provider": "anthropic"`},
		{"", "zai,anthropic", "pin one hint"},
		{"", "zai:", `write "provider": "zai"`},
	} {
		cell := llmroute.Override{Selector: "copilot/reflect#claude", Backend: tc.backend, Provider: tc.provider}
		err := llmroute.Validate(llmroute.Policy{Overrides: []llmroute.Override{cell}})
		if err == nil || !strings.Contains(err.Error(), "a route's provider is one hint") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("cell Validate(%+v) = %v, want the one-hint refusal naming %q", cell, err, tc.want)
		}
		err = llmroute.ValidateLaunchRow("reflect#claude", tc.backend, tc.provider, "")
		if err == nil || !strings.Contains(err.Error(), "a route's provider is one hint") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("row ValidateLaunchRow(reflect#claude, %q, %q) = %v, want the one-hint refusal naming %q", tc.backend, tc.provider, err, tc.want)
		}
	}
	for _, cell := range []llmroute.Override{
		{Selector: "copilot/reflect#claude", Backend: "claw", Provider: "openai_compatible", Model: selectorGateway},
		{Selector: "copilot/judge", Backend: "claude_code", Provider: "zai,anthropic"},
		{Selector: "copilot/reflect", Backend: "claw", Provider: "openai_compatible:" + selectorGateway, Model: selectorGateway},
	} {
		if err := llmroute.Validate(llmroute.Policy{Overrides: []llmroute.Override{cell}}); err != nil {
			t.Errorf("control Validate(%+v) = %v, want accepted", cell, err)
		}
	}
	// A dial's `:-` is no step: the runner expands it, and a write surface
	// refuses the non-literal on its own terms.
	if err := llmroute.ValidateLaunchRow("reflect#claude", "", "${ITERION_PROBE_P:-openai_compatible}", ""); err != nil {
		t.Errorf("control ValidateLaunchRow(dial) = %v, want accepted", err)
	}
}

// The gateway-model rule is a write surface's: Validate refuses a gateway
// step on a model that is not a gateway route, and a gateway hint beside
// such a cell model, while ValidateStored — the launch's reading of a
// stored layer — leaves that crossing to the launch's per-node judge and
// keeps every reading rule.
func TestValidateStored_LeavesTheGatewayModelRuleToTheLaunch(t *testing.T) {
	for _, o := range []llmroute.Override{
		{Selector: "work", Provider: "zai,openai_compatible:scaleway/deepseek-v4.1-flash"},
		{Selector: "work", Provider: "openai_compatible", Model: "anthropic/claude-opus-5-5"},
	} {
		p := llmroute.Policy{Overrides: []llmroute.Override{o}}
		if err := llmroute.Validate(p); err == nil || !strings.Contains(err.Error(), "the gateway hint serves gateway models only") {
			t.Errorf("Validate(%+v) = %v, want the write surface's gateway-model refusal", o, err)
		}
		if err := llmroute.ValidateStored(p); err != nil {
			t.Errorf("ValidateStored(%+v) = %v, want the stored cell read", o, err)
		}
	}
	for _, o := range []llmroute.Override{
		{Selector: "work", Provider: "zai,nosuch"},
		{Selector: "work"},
		{Selector: "copilot/reflect#claude", Provider: "zai,anthropic"},
	} {
		if err := llmroute.ValidateStored(llmroute.Policy{Overrides: []llmroute.Override{o}}); err == nil {
			t.Errorf("ValidateStored(%+v) = nil, want a record the fold cannot read refused", o)
		}
	}
}
