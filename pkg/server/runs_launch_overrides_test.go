package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
)

func TestValidateModelOverrides(t *testing.T) {
	ok := []runview.ModelOverrideEntry{
		{Selector: "*", Model: "anthropic/claude-opus-5"},
		{Selector: "agent", Backend: "claw", Effort: "high"},
		// ultracode is a mode, not a wire value, but it IS an accepted level.
		{Selector: "reviewer_*", Effort: "ultracode"},
		// A model iterion has never heard of stays legal: model names are host
		// state this process cannot enumerate.
		{Selector: "fix_*", Model: "somevendor/some-model-9"},
	}
	if err := validateModelOverrides(ok); err != nil {
		t.Fatalf("valid overrides rejected: %v", err)
	}

	// The effort reaches the provider verbatim, so an unknown level must die at
	// admission — not as an API error on the run's first node.
	err := validateModelOverrides([]runview.ModelOverrideEntry{
		{Selector: "*", Effort: "turbo"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown reasoning effort")
	}
	if !strings.Contains(err.Error(), "turbo") {
		t.Errorf("error should name the offending value, got %q", err)
	}
	// And it should say what IS accepted, so the caller can fix it in one go.
	if !strings.Contains(err.Error(), "xhigh") {
		t.Errorf("error should list the valid levels, got %q", err)
	}

	err = validateModelOverrides([]runview.ModelOverrideEntry{
		{Selector: "*", Backend: "clua"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown backend")
	}
	if !strings.Contains(err.Error(), "claude_code") {
		t.Errorf("error should list the valid backends, got %q", err)
	}

	if err := validateModelOverrides([]runview.ModelOverrideEntry{
		{Selector: "  ", Model: "anthropic/claude-opus-5"},
	}); err == nil {
		t.Error("expected an error for an empty selector")
	}
}

// A launch's own rows reach its own run: a bot dimension is a 400, and so
// is a field the selector's class does not carry (effort on a route,
// backend/provider on a supervisor) — the rows a routing cell would be
// refused for, refused at admission instead of folding into a dead pin.
func TestValidateModelOverrides_RefusesRowsTheGrammarCannotHonour(t *testing.T) {
	for _, e := range []runview.ModelOverrideEntry{
		{Selector: "review-pr/converge", Model: "zai/glm-5.3"},
		{Selector: "converge#glm", Effort: "high"},
		{Selector: "supervisor:pacer", Backend: "claw"},
		{Selector: "supervisor:pacer", Provider: "anthropic"},
		{Selector: "converge#*", Model: "zai/glm-5.3"},
		{Selector: "converge#glm", Provider: "zai,anthropic"},
		{Selector: "converge#glm", Provider: "zai:glm-5.3"},
	} {
		err := validateModelOverrides([]runview.ModelOverrideEntry{e})
		if err == nil {
			t.Errorf("validateModelOverrides accepted %+v", e)
			continue
		}
		if strings.Contains(e.Selector, "/") && !strings.Contains(err.Error(), "review-pr") {
			t.Errorf("refusal %q does not name the bot", err)
		}
	}
	for _, e := range []runview.ModelOverrideEntry{
		{Selector: "converge#glm", Model: "zai/glm-5.3", Backend: "claw", Provider: "zai"},
		{Selector: "supervisor:pacer", Model: "anthropic/claude-opus-5"},
		{Selector: "*", Model: "x"},
		{Selector: "agent", Effort: "high"},
		{Selector: "reviewer_*", Model: "x"},
	} {
		if err := validateModelOverrides([]runview.ModelOverrideEntry{e}); err != nil {
			t.Errorf("validateModelOverrides refused the honoured row %+v: %v", e, err)
		}
	}
}

// A route row's provider is one literal hint: a stray ',' rides the hint
// whole to dispatch, and a template expands to a value the launch never
// judged — both a 400 naming the row. A node row keeps the stray ',':
// dispatch splits a node chain and drops the empty step.
func TestValidateModelOverrides_RefusesAStrayCommaOrATemplateOnARouteRow(t *testing.T) {
	for p, want := range map[string]string{
		"zai,":                        "a route's provider is one hint",
		"${R3V_UNSET:-zai,anthropic}": "is a LITERAL",
	} {
		err := validateModelOverrides([]runview.ModelOverrideEntry{{Selector: "converge#glm", Provider: p}})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), `"converge#glm"`) {
			t.Errorf("validateModelOverrides(converge#glm, provider %q) = %v, want a refusal naming the row and %q", p, err, want)
		}
	}
	if err := validateModelOverrides([]runview.ModelOverrideEntry{{Selector: "converge", Provider: "zai,"}}); err != nil {
		t.Errorf("validateModelOverrides refused the node row converge zai,: %v", err)
	}
}
