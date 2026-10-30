package sessionboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The sessionboard twin of supervise's ErrGatewayWatchNeedsPin (#2099): a
// board curating a gateway-served session evaluates that session's content,
// so an unpinned curation resolving the detector's vendor default would spend
// a credential the deployment never funded — and burn it uncapped, invisible
// to the env-funded boundary. The evaluator refuses instead, and the
// coordinator parks.
//
// The free layer stays free: every subtest runs with the provider env made
// hermetic — the only credential in sight is a dummy OPENAI_API_KEY aimed at
// a LOCAL httptest endpoint (wire_model_test.go's pattern), so a subtest that
// gets past the pin gate wires to it and never to a vendor, regardless of
// the ambient environment. The per-subtest call counts are the assertion
// that the wire went where the test pinned it.
func TestLLMEvaluator_GatewaySessionNeedsAPin(t *testing.T) {
	t.Setenv("ITERION_DEFAULT_SESSIONBOARD_MODEL", "")
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ZAI_API_KEY", "MOONSHOT_API_KEY", "XAI_API_KEY",
		"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT",
		"GOOGLE_CLOUD_PROJECT", "AWS_REGION", "AWS_DEFAULT_REGION",
	} {
		t.Setenv(k, "")
	}
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_BASE_URL", srv.URL)
	t.Setenv("OPENAI_API_KEY", "test-key")
	callsSoFar := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}

	t.Run("unpinned refuses before any detection", func(t *testing.T) {
		before := callsSoFar()
		e := NewLLMEvaluator("")
		e.gatewayWatched = true
		_, _, err := e.Evaluate(context.Background(), EvalInput{})
		if !errors.Is(err, ErrGatewayBoardNeedsPin) {
			t.Fatalf("err = %v, want ErrGatewayBoardNeedsPin", err)
		}
		// The refusal is the gate's alone: it must answer BEFORE any
		// provider endpoint is contacted.
		if n := callsSoFar() - before; n != 0 {
			t.Errorf("the refusal fired after %d wire call(s) — the gate must answer before any provider is contacted", n)
		}
	})

	t.Run("a pin answers", func(t *testing.T) {
		before := callsSoFar()
		e := NewLLMEvaluator("openai/sessionboard-pin-probe")
		e.gatewayWatched = true
		_, _, err := e.Evaluate(context.Background(), EvalInput{})
		// The pin passes the gate; the call itself may still fail — never
		// with the refusal, which is the gate's alone.
		if errors.Is(err, ErrGatewayBoardNeedsPin) {
			t.Fatalf("a pinned curation was refused: %v", err)
		}
		// The evaluation must wire to the PINNED endpoint: zero local calls
		// means the resolution left this test's environment and went
		// vendor-shaped — spend from the free layer, the defect this pin
		// exists to prevent.
		if n := callsSoFar() - before; n == 0 {
			t.Error("the pinned evaluation reached no local endpoint — the pin resolved outside the pinned OPENAI_BASE_URL")
		}
	})

	t.Run("the env override answers", func(t *testing.T) {
		t.Setenv("ITERION_DEFAULT_SESSIONBOARD_MODEL", "openai/sessionboard-pin-probe")
		before := callsSoFar()
		e := NewLLMEvaluator("")
		e.gatewayWatched = true
		_, _, err := e.Evaluate(context.Background(), EvalInput{})
		if errors.Is(err, ErrGatewayBoardNeedsPin) {
			t.Fatalf("an env-pinned curation was refused: %v", err)
		}
		if n := callsSoFar() - before; n == 0 {
			t.Error("the env-pinned evaluation reached no local endpoint — the model resolved outside the pinned OPENAI_BASE_URL")
		}
	})

	t.Run("a non-gateway session is untouched", func(t *testing.T) {
		e := NewLLMEvaluator("")
		// gatewayWatched false: no refusal, whatever detection resolves —
		// and what it resolves stays in the pinned environment above.
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
