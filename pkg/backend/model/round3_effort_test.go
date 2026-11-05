package model

import (
	"bytes"
	"context"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

const r3Table = `{"deepseek":["low","high","xhigh","max"]}`

func r3GatewayEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv(compatgw.ResolvedEnvName, "")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "http://127.0.0.1:8081/v1")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "test-key")
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)
}

// B. A direct generation that sends a reasoning effort: the human/interaction stand-in.
func TestR3_G8HumanStandInUsesTheRouteMatrix(t *testing.T) {
	r3GatewayEnv(t)
	t.Setenv("OPENAI_COMPATIBLE_REASONING_EFFORTS", r3Table)
	mock := newMockClient(toolUseEvents("ok", "structured_output", `{"answer":"yes"}`, 10, 10))
	reg := NewRegistry()
	reg.Register("openai_compatible", func(string) (api.APIClient, error) { return mock, nil })
	wf := &ir.Workflow{Schemas: map[string]*ir.Schema{"ans": {Name: "ans", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}}, Prompts: map[string]*ir.Prompt{}}
	e := newTestClawExecutor(reg, wf, WithLogger(iterlog.Nop()))
	node := &ir.HumanNode{BaseNode: ir.BaseNode{ID: "approve"}}
	node.Interaction = ir.InteractionLLM
	node.Model = "openai_compatible/deepseek"
	node.OutputSchema = "ans"
	_, err := e.executeHumanLLM(context.Background(), node, map[string]any{"_reasoning_effort": "medium"}, nil)
	calls := mock.getCalls()
	sent := ""
	if len(calls) > 0 {
		sent = calls[0].ReasoningEffort
	}
	if err != nil || len(calls) != 1 || sent != "high" {
		t.Errorf("human effort=%s calls=%d err=%v", sent, len(calls), err)
	}
	// Same route through the delegate seam, for contrast:
	t.Logf("executeDelegate seam would send %q", e.routeReasoningEffort(context.Background(), "approve", "openai_compatible/deepseek", "medium"))
}

// The cut-turn rung lowers to an effort the route accepts.
func TestR3_G8EffortAfterCutUsesTheRouteMatrix(t *testing.T) {
	r3GatewayEnv(t)
	t.Setenv("OPENAI_COMPATIBLE_REASONING_EFFORTS", `{"strict":["high","max"]}`)
	opts := GenerationOptions{Model: "openai_compatible/strict", ProviderOptions: map[string]any{"reasoning_effort": "max"}}
	lowered, to, ok := effortAfterCut(context.Background(), opts)
	if !ok || to != "high" || lowered.ProviderOptions["reasoning_effort"] != "high" {
		t.Errorf("cut: ok=%v to=%s opts=%v", ok, to, lowered)
	}
}

// The route mapping reads the invocation's carried environment.
func TestR3_G8CarriedEnvSeam(t *testing.T) {
	r3GatewayEnv(t)
	ctx := withCarriedEnv(context.Background(), carriedEnv{"OPENAI_COMPATIBLE_REASONING_EFFORTS": r3Table})
	e := newTestClawExecutor(NewRegistry(), verdictWorkflow(), WithLogger(iterlog.Nop()))
	b := &r3EffortBackend{}
	_, err := e.executeDelegate(ctx, b, delegate.Task{NodeID: "n", Model: "openai_compatible/deepseek", ReasoningEffort: "medium"})
	if err != nil || b.effort != "high" {
		t.Fatalf("carried mapping: effort=%s err=%v", b.effort, err)
	}
}

type r3EffortBackend struct{ effort string }

func (b *r3EffortBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.effort = task.ReasoningEffort
	return delegate.Result{}, nil
}
func TestR3_G8Tolerance(t *testing.T) {
	r3GatewayEnv(t)
	for _, tc := range []struct {
		raw, want string
		warning   bool
	}{
		{`{"deepseek":[" Low ","HIGH"," Xhigh ","Max"]}`, "high", false},
		{`{"broken":["hihg"],"deepseek":["low","high"]}`, "high", true},
		{`{"deepseek":["low","hihg","max"]}`, "medium", true},
		{`{"deepseek":[50]}`, "50", false},
		{`{"deepseek":["50"]}`, "50", false},
		{`{"deepseek":[101]}`, "medium", true},
		{`{"deepseek":[]}`, "medium", false},
		{`{`, "medium", true},
	} {
		t.Setenv("OPENAI_COMPATIBLE_REASONING_EFFORTS", tc.raw)
		var log bytes.Buffer
		e := newTestClawExecutor(NewRegistry(), verdictWorkflow(), WithLogger(iterlog.New(iterlog.LevelWarn, &log)))
		got := e.routeReasoningEffort(context.Background(), "n", "openai_compatible/deepseek", "medium")
		if got != tc.want {
			t.Errorf("%s: got=%s want=%s", tc.raw, got, tc.want)
		}
		if warned := strings.Contains(log.String(), "REASONING_EFFORTS_INVALID"); warned != tc.warning {
			t.Errorf("%s: warning=%v log=%s", tc.raw, warned, log.String())
		}
	}
}
