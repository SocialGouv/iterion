package sessionboard

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The sessionboard twin of supervise's ErrGatewayWatchNeedsPin (#2099): a
// board curating a gateway-served session evaluates that session's content,
// so an unpinned curation resolving the detector's vendor default would spend
// a credential the deployment never funded — and burn it uncapped, invisible
// to the env-funded boundary. The evaluator refuses instead, and the
// coordinator parks.
func TestLLMEvaluator_GatewaySessionNeedsAPin(t *testing.T) {
	t.Setenv("ITERION_DEFAULT_SESSIONBOARD_MODEL", "")

	t.Run("unpinned refuses before any detection", func(t *testing.T) {
		e := NewLLMEvaluator("")
		e.gatewayWatched = true
		_, _, err := e.Evaluate(context.Background(), EvalInput{})
		if !errors.Is(err, ErrGatewayBoardNeedsPin) {
			t.Fatalf("err = %v, want ErrGatewayBoardNeedsPin", err)
		}
	})

	t.Run("a pin answers", func(t *testing.T) {
		e := NewLLMEvaluator("anthropic/claude-haiku-4-5")
		e.gatewayWatched = true
		_, _, err := e.Evaluate(context.Background(), EvalInput{})
		// The pin must pass the gate; resolution or the call itself may
		// still fail (no credential in the test env) — never with the
		// refusal, which is the gate's alone.
		if errors.Is(err, ErrGatewayBoardNeedsPin) {
			t.Fatalf("a pinned curation was refused: %v", err)
		}
	})

	t.Run("the env override answers", func(t *testing.T) {
		t.Setenv("ITERION_DEFAULT_SESSIONBOARD_MODEL", "anthropic/claude-haiku-4-5")
		e := NewLLMEvaluator("")
		e.gatewayWatched = true
		_, _, err := e.Evaluate(context.Background(), EvalInput{})
		if errors.Is(err, ErrGatewayBoardNeedsPin) {
			t.Fatalf("an env-pinned curation was refused: %v", err)
		}
	})

	t.Run("a non-gateway session is untouched", func(t *testing.T) {
		e := NewLLMEvaluator("")
		// gatewayWatched false: no refusal, whatever detection resolves.
		if _, _, err := e.Evaluate(context.Background(), EvalInput{}); errors.Is(err, ErrGatewayBoardNeedsPin) {
			t.Fatalf("an ordinary session hit the gateway refusal: %v", err)
		}
	})
}

// pinRefusingEvaluator plays the gateway-refusing evaluator for the
// coordinator's park contract.
type pinRefusingEvaluator struct{ calls int }

func (e *pinRefusingEvaluator) Evaluate(context.Context, EvalInput) (*BoardDecision, EvalUsage, error) {
	e.calls++
	return nil, EvalUsage{}, ErrGatewayBoardNeedsPin
}

// The refusal is a missing pin, not a transient failure: the coordinator
// parks on it rather than re-refusing (and re-warning) on every wake —
// supervise's maxEvalFailures shape, for a refusal that can never heal.
func TestCoordinatorParksOnGatewayPinRefusal(t *testing.T) {
	eval := &pinRefusingEvaluator{}
	c := New(staticObserver{}, &fakeEmitter{}, "run_1", Config{Cooldown: 0, MaxEvals: 10}, eval, nil)
	if c == nil {
		t.Fatal("New returned nil")
	}
	c.ctx = context.Background()
	c.ingest(&store.Event{Seq: 1, Type: store.EventNodeStarted, NodeID: "n"})
	for i := 0; i < 4; i++ {
		c.evaluate("turn_boundary", true)
	}
	if eval.calls != 1 {
		t.Errorf("evaluator calls = %d, want 1 — a parked coordinator never re-asks", eval.calls)
	}
	if !c.parked {
		t.Error("the coordinator did not park on ErrGatewayBoardNeedsPin")
	}
}

// New wires the config's GatewayWatched into the evaluator it builds — the
// seam startSessionBoard feeds from supervise.WorkflowNamesGateway.
func TestNewCarriesGatewayWatchedToTheEvaluator(t *testing.T) {
	c := New(staticObserver{}, &fakeEmitter{}, "run_1", Config{GatewayWatched: true}, nil, nil)
	if c == nil {
		t.Fatal("New returned nil")
	}
	e, ok := c.eval.(*LLMEvaluator)
	if !ok {
		t.Fatalf("default evaluator is %T, want *LLMEvaluator", c.eval)
	}
	if !e.gatewayWatched {
		t.Error("Config.GatewayWatched did not reach the evaluator")
	}
}
