package model

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// W1/W2: restoring the nil-only token stamp preserves the forged zero or
// inflated count. Removing the namespace sweep preserves _invented and
// absent optional metadata; reading cost from Output preserves its forgery.
func TestDelegateMetadataUsesOnlyHarnessCounters(t *testing.T) {
	t.Parallel()
	for _, forged := range []int{0, 99999999} {
		output := map[string]any{
			"answer": "blue", "_tokens": forged, "_cost_usd": 99999999,
			"_usage_unreported_calls": 999, "_invented": "model",
			"_session_id": "fake", "_model": "fake", "_context_window": 999,
			"_context_used": 999, "_max_output_tokens": 999,
			"_thinking_tokens": 999, "_thinking_ms": 999,
			"_fallback_used": true, "_session_fingerprint": "fake",
		}
		result := delegate.Result{Output: output, Tokens: 42, CostUSD: 0.25, UnreportedCalls: 1}
		stampDelegateOutputMeta(output, result, "stub")
		want := map[string]any{"answer": "blue", "_tokens": 42, "_cost_usd": 0.25,
			"_usage_unreported_calls": 1, "_backend": "stub"}
		if !reflect.DeepEqual(output, want) {
			t.Fatalf("forged tokens %d: output = %#v, want %#v", forged, output, want)
		}
		// Zero counters clear prior metadata too; re-stamping is idempotent.
		stampDelegateOutputMeta(output, delegate.Result{}, "stub")
		want = map[string]any{"answer": "blue", "_tokens": 0, "_backend": "stub"}
		if !reflect.DeepEqual(output, want) {
			t.Fatalf("zero result retains model metadata: %#v", output)
		}
	}
}

// W4: the invariant above owns the policy; each exit must invoke it. Deleting
// any named call fails its subtest, including recovery, router and relay.
func TestDelegateOutputExitsUseTheMetadataBoundary(t *testing.T) {
	t.Parallel()
	for file, names := range map[string][]string{
		"executor_build_task.go": {"meteredFailureOutput", "executeBackend", "validateAndRetry", "extractStructuredViaClaw"},
		"executor_router.go":     {"executeLLMRouterUnified"},
		"claw_backend.go":        {"executeViaSandboxRunner"},
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			t.Run(name, func(t *testing.T) {
				found := false
				for _, declaration := range parsed.Decls {
					fn, ok := declaration.(*ast.FuncDecl)
					if !ok || fn.Name.Name != name {
						continue
					}
					ast.Inspect(fn.Body, func(node ast.Node) bool {
						if call, ok := node.(*ast.CallExpr); ok {
							if callee, ok := call.Fun.(*ast.Ident); ok && callee.Name == "stampDelegateOutputMeta" {
								found = true
							}
						}
						return true
					})
				}
				if !found {
					t.Fatalf("%s no longer applies stampDelegateOutputMeta", name)
				}
			})
		}
	}
}

// W4 relay: removing the host-side stamp exposes the injected metadata;
// dropping a typed wire field loses the harness bill on the crossing.
func TestDelegateRelayUsesHarnessCounters(t *testing.T) {
	t.Parallel()
	for _, failure := range []bool{false, true} {
		dir := t.TempDir()
		res := delegate.ToIOResult(delegate.Result{
			Output: map[string]any{"answer": "blue", "_tokens": 99999999, "_invented": true, "_cost_usd": 999},
			Tokens: 42, CostUSD: 0.25, UnreportedCalls: 1, EffectiveModel: "measured",
		})
		if failure {
			res.Error = "runner fails after spending"
		}
		path := resultEnvelopeFile(t, dir, res)
		backend := newTestClawBackend()
		got, err := backend.executeViaSandboxRunner(context.Background(), delegate.Task{
			NodeID: "n", Model: "openai/gpt-5", WorkDir: dir,
			Sandbox: &scriptedSandbox{script: awaitTaskEnvelope + "cat \"$1\"\n", args: []string{path}},
		})
		if (err != nil) != failure {
			t.Fatalf("failure=%v: %v", failure, err)
		}
		want := map[string]any{"answer": "blue", "_tokens": 42, "_cost_usd": 0.25,
			"_usage_unreported_calls": 1, "_model": "measured", "_backend": "claw"}
		if !reflect.DeepEqual(got.Output, want) {
			t.Fatalf("relayed output = %#v, want %#v", got.Output, want)
		}
	}
}

// W3: dropping the separate spend return loses 100 tokens; merging it into
// answers overwrites an explicitly asked _tokens answer and leaks metadata.
func TestInteractionAnswersKeepTheirSpendSeparate(t *testing.T) {
	t.Parallel()
	client := newMockClient(toolUseEvents("answer", "structured_output",
		`{"which_db":"postgres","_tokens":"an answer","_invented":true}`, 70, 30))
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return client, nil })
	exec := NewClawExecutor(reg, &ir.Workflow{})
	answers, spend, needsHuman, err := exec.ExecuteHumanLLMForInteraction(context.Background(), "n",
		&ErrNeedsInteraction{Questions: map[string]any{"which_db": "which database?", "_tokens": "literal question"}},
		ir.InteractionFields{Interaction: ir.InteractionLLM, InteractionModel: "anthropic/claude-opus-5"})
	if err != nil || needsHuman {
		t.Fatalf("interaction: needsHuman=%v, err=%v", needsHuman, err)
	}
	if spend["_tokens"] != 100 {
		t.Fatalf("spend = %#v, want 100 tokens", spend)
	}
	if !reflect.DeepEqual(answers, map[string]any{"which_db": "postgres", "_tokens": "an answer"}) {
		t.Fatalf("answers contain metadata or lose an asked key: %#v", answers)
	}
}
