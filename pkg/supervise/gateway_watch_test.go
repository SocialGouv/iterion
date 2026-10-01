package supervise

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A supervisor that may observe a gateway-served route refuses to fall back
// to a vendor family pick: evaluation parks with the named refusal until
// the operator pins a model (the pin is judged AFTER expansion — an
// ${UNSET} template is no pin) or sets the deployment-wide default. Red
// when the evaluator's refusal, the GatewayWatched wiring or the
// expansion-is-no-pin rule is dropped.
func TestGatewayWatch_NeedsAPinOrTheEnvDefault(t *testing.T) {
	e := NewLLMEvaluator()
	e.gatewayWatched = true

	_, _, err := e.Evaluate(context.Background(), EvalInput{Spec: Spec{Model: "", ProviderHint: "anthropic"}})
	if !errors.Is(err, ErrGatewayWatchNeedsPin) {
		t.Fatalf("an unpinned gateway-watched evaluator = %v, want the refusal", err)
	}

	// A pin that expands lifts the refusal (the evaluator then resolves the
	// pinned spec and fails later, on the client — not with the refusal).
	e2 := NewLLMEvaluator()
	e2.gatewayWatched = true
	t.Setenv("ITERION_TEST_SUPERVISOR_PIN", "anthropic/claude-opus-5")
	_, _, err = e2.Evaluate(context.Background(), EvalInput{Spec: Spec{Model: "${ITERION_TEST_SUPERVISOR_PIN}", ProviderHint: "anthropic"}})
	if errors.Is(err, ErrGatewayWatchNeedsPin) {
		t.Fatalf("an expanding pin must lift the refusal, got it verbatim")
	}

	// The deployment-wide default lifts it too.
	e3 := NewLLMEvaluator()
	e3.gatewayWatched = true
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "anthropic/claude-opus-5")
	_, _, err = e3.Evaluate(context.Background(), EvalInput{Spec: Spec{}})
	if errors.Is(err, ErrGatewayWatchNeedsPin) {
		t.Fatal("the env default must lift the refusal")
	}

	// An unexpanded template is NO pin: the refusal holds.
	e4 := NewLLMEvaluator()
	e4.gatewayWatched = true
	t.Setenv("ITERION_DEFAULT_SUPERVISOR_MODEL", "")
	_, _, err = e4.Evaluate(context.Background(), EvalInput{Spec: Spec{Model: "${ITERION_TEST_SUPERVISOR_PIN_UNSET_X}"}})
	if !errors.Is(err, ErrGatewayWatchNeedsPin) {
		t.Fatal("an unexpanded template is no pin — want the refusal")
	}

	// A supervisor watching no gateway route is untouched by the rule.
	e5 := NewLLMEvaluator()
	_, _, err = e5.Evaluate(context.Background(), EvalInput{Spec: Spec{}})
	if errors.Is(err, ErrGatewayWatchNeedsPin) {
		t.Fatal("a non-gateway supervisor must not see the refusal")
	}
}

// gatewayWatched reads the watched routes: a gateway model, a fallback's
// gateway model, a provider step naming the gateway — and, conservatively,
// any template the launch cannot rule out. Vendor routes answer false. Red
// when the walk drops a source or over-triggers.
func TestGatewayWatched_ReadsTheWatchedRoutes(t *testing.T) {
	wf := func(model string, fbs []ir.Fallback) *ir.Workflow {
		return &ir.Workflow{
			Nodes: map[string]ir.Node{
				"gw":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Model: model}, Fallbacks: fbs},
				"misc": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "misc"}, LLMFields: ir.LLMFields{Model: "anthropic/claude-opus-5"}},
			},
		}
	}
	cases := []struct {
		name    string
		watches []string
		wf      *ir.Workflow
		want    bool
	}{
		{"a watched gateway model", []string{"gw"}, wf("openai_compatible/team/m", nil), true},
		{"a gateway fallback", []string{"gw"}, wf("anthropic/claude-opus-5", []ir.Fallback{{Model: "openai_compatible/team/m"}}), true},
		{"a gateway provider step", []string{"gw"}, wf("anthropic/claude-opus-5", []ir.Fallback{{Provider: "openai_compatible"}}), true},
		{"an unexpanded template counts as gateway", []string{"gw"}, wf("{{vars.impl}}", nil), true},
		{"an env default naming the gateway", []string{"gw"}, wf("${GWM:-openai_compatible/team/m}", nil), true},
		{"a vendor model is not gateway", []string{"misc"}, wf("anthropic/claude-opus-5", nil), false},
		{"an unwatched gateway node does not count", []string{"misc"}, wf("openai_compatible/team/m", nil), false},
		{"no watches reads every node", nil, wf("openai_compatible/team/m", nil), true},
		// The dispatch walks a judge's fallbacks, a provider CHAIN per step,
		// and an mode:llm router — the watch must see what dispatches sees.
		{"a judge's gateway fallback", []string{"jw"}, func() *ir.Workflow {
			return &ir.Workflow{Nodes: map[string]ir.Node{
				"jw": &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "jw"}, LLMFields: ir.LLMFields{Model: "anthropic/claude-opus-5"}, Fallbacks: []ir.Fallback{{Model: "openai_compatible/team/m"}}},
			}}
		}(), true},
		{"a provider chain step naming the gateway", []string{"gw"}, func() *ir.Workflow {
			return &ir.Workflow{Nodes: map[string]ir.Node{
				"gw": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"},
					LLMFields: ir.LLMFields{Model: "openai_compatible/team/m", Provider: "anthropic, openai_compatible"}},
			}}
		}(), true},
		{"an mode:llm router on a gateway model", []string{"rt"}, func() *ir.Workflow {
			return &ir.Workflow{Nodes: map[string]ir.Node{
				"rt": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "rt"}, RouterMode: ir.RouterLLM, LLMFields: ir.LLMFields{Model: "openai_compatible/team/m"}},
			}}
		}(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatewayWatched(tc.wf, tc.watches); got != tc.want {
				t.Errorf("gatewayWatched = %v, want %v", got, tc.want)
			}
		})
	}
}

// The refusal names the remedy an operator can act on.
func TestGatewayWatch_TheRefusalNamesTheRemedy(t *testing.T) {
	if !strings.Contains(ErrGatewayWatchNeedsPin.Error(), "ITERION_DEFAULT_SUPERVISOR_MODEL") {
		t.Errorf("refusal %q does not name the env remedy", ErrGatewayWatchNeedsPin.Error())
	}
}
