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

// The bot dimension reaches the bots it names: none = every bot, a name =
// that bot, a glob = the names it matches; a launch naming no bot is reached
// by no scoped cell. NamesBot is the exact-name half the zero-match warning
// reads.
func TestSelector_TheBotDimension(t *testing.T) {
	for _, tc := range []struct {
		sel, bot       string
		reaches, named bool
	}{
		{"reviewer_claude", "review-pr", true, false},
		{"reviewer_claude", "", true, false},
		{"review-pr/reviewer_claude", "review-pr", true, true},
		{"review-pr/reviewer_claude", "revi-converse", false, false},
		{"review-pr/reviewer_claude", "", false, false},
		{"revi*/x#glm", "revi-converse", true, false},
		{"revi*/x#glm", "review-pr", true, false},
		{"*/supervisor:pacer", "docs-refresh", true, false},
	} {
		s, err := llmroute.ParseSelector(tc.sel)
		if err != nil {
			t.Fatalf("ParseSelector(%q): %v", tc.sel, err)
		}
		if got := s.ReachesBot(tc.bot); got != tc.reaches {
			t.Errorf("%q.ReachesBot(%q) = %v, want %v", tc.sel, tc.bot, got, tc.reaches)
		}
		if got := s.NamesBot(tc.bot); got != tc.named {
			t.Errorf("%q.NamesBot(%q) = %v, want %v", tc.sel, tc.bot, got, tc.named)
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
