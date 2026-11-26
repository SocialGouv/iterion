package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
)

// streamRefusingGateway answers a request forcing a tool choice with a
// stream that opens and then carries the gateway's error frame — the shape a
// grammar refusal takes when vLLM raises it after the response started —
// and serves the others from scripts in order.
type streamRefusingGateway struct {
	forcedChoiceGateway
	frame string
}

func (g *streamRefusingGateway) StreamResponse(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	if req.ToolChoice == nil {
		return g.forcedChoiceGateway.StreamResponse(ctx, req)
	}
	g.mu.Lock()
	g.calls = append(g.calls, req)
	g.mu.Unlock()
	ch := make(chan api.StreamEvent, 1)
	ch <- api.StreamEvent{Type: api.EventError, ErrorMessage: g.frame}
	close(ch)
	return ch, nil
}

// #3048 M1: the no-grammar re-ask of a refused forced tool choice reads the
// refusal through api.RefusalOf, so a refusal relayed as a STREAM error frame
// re-asks by instruction like its HTTP form does, instead of failing the node.
func TestStreamGrammarRefusalOfAForcedChoiceRetriesByInstruction(t *testing.T) {
	t.Parallel()
	gw := &streamRefusingGateway{
		frame: "openai stream error: Grammar error: Unable to determine if regex is empty (type=BadRequestError, code=500)",
		forcedChoiceGateway: forcedChoiceGateway{scripts: [][]api.StreamEvent{
			toolUseEvents("c1", "read_image", `{}`, 11, 7),
			textEvents("PORTAIL-7 -> BASE-QX", 11, 7),
		}},
	}
	prompt := []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Open schema.png"}}}}
	executed := 0
	var refusals []error
	out, err := GenerateTextDirect(context.Background(), gw, gatewayGeneration(prompt, &executed, &refusals))
	if err != nil {
		t.Fatalf("a stream grammar refusal of the forced choice failed the generation: %v", err)
	}
	if out.Text != "PORTAIL-7 -> BASE-QX" || executed != 1 {
		t.Fatalf("text=%q executed=%d, want the answer after one tool run", out.Text, executed)
	}
	if len(refusals) != 1 || api.RefusalOf(refusals[0]) != api.RefusalGrammar {
		t.Errorf("refusals reported = %v, want the one grammar refusal", refusals)
	}
}

// #3048 item 1 (operator doctrine): a gateway's budget refusal is a spend
// ceiling — the claw boundary types it as the account refusal every other
// backend raises for one (a usage window naming no reset), whatever status
// or channel it came on, so the run parks and an hourly probe relaunches it.
func TestClawBudgetRefusalIsAnAccountRefusal(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]error{
		"422": &api.APIError{Provider: "openai", StatusCode: 422,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
		"429 knob": &api.APIError{Provider: "openai", StatusCode: 429, Retryable: true,
			Message: "ExceededBudget: Key over 1d budget. Spend=5.01, Budget=5.0"},
		"500": &api.APIError{Provider: "openai", StatusCode: 500, Retryable: true,
			Message: "Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
		"stream": &APIError{StatusCode: 400,
			Message: "stream error: Budget has been exceeded! Current cost: 150.18, Max budget: 150.0"},
	} {
		typed := clawUsageWindow(fmt.Errorf("claw backend: runner: %w", refusal), time.Now())
		var rl *delegate.ErrRateLimited
		if !errors.As(typed, &rl) || rl.Kind != delegate.RateLimitKindUsageWindow {
			t.Errorf("%s: %v is not a usage-window refusal", name, typed)
			continue
		}
		if !delegate.IsAccountRefusal(typed) {
			t.Errorf("%s: not an account refusal — it would wait for a reset no budget names", name)
		}
		if !errors.Is(typed, refusal) {
			t.Errorf("%s: the provider's error is lost under the typed refusal", name)
		}
	}
}
