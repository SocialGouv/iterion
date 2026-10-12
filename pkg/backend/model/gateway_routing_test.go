package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// clearGatewayEnv pins the gateway variables to empty for a test: the host
// under test must not be able to turn a "no env" case green by happening to
// serve a gateway.
func clearGatewayEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "")
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "")
}

// TestRefuseGatewayCrossing_RefusesACrossingInBothDirections is the witness
// for gateway_routing.go's parity rule, over its five behaviours. Mutation
// that must redden it: make refuseGatewayCrossing return nil unconditionally
// — every wantErr row then fails on its own assertion, and every nil row is
// the guard against the opposite mutation (refuse everything).
func TestRefuseGatewayCrossing_RefusesACrossingInBothDirections(t *testing.T) {
	cases := []struct {
		name      string
		backend   string
		hint      string
		model     string
		wantErr   bool
		wantNamed []string // fragments the refusal must carry
	}{
		// (e) neither gateway: nothing to refuse, on any backend.
		{"plain vendor route on codex", "codex", "anthropic", "anthropic/claude-x", false, nil},
		{"no hint no gateway", "claw", "", "anthropic/claude-x", false, nil},
		{"bare vendor model on claude_code", "claude_code", "", "claude-x", false, nil},

		// (a) a gateway model beside a vendor hint names two wires.
		{"gateway model with an anthropic hint", "claw", "anthropic", "openai_compatible/m", true,
			[]string{"openai_compatible/m", "anthropic"}},
		{"gateway model with slashes and a vendor hint", "claw", "openai", "openai_compatible/team-a/m", true,
			[]string{"openai_compatible/team-a/m", "openai"}},
		// The gateway-hint match is the exact lower-cased, trimmed token:
		// "OpenAI_Compatible" and a padded form are the gateway hint.
		// "auto" in ANY spelling is the engine's absent hint and passes:
		// the compiler's fold, the delegate normaliser and this refusal
		// all fold it (the runtime chain normalises only lowercase, which
		// is why the fold lives here and not upstream). A real vendor
		// hint beside a gateway model is still the refusal.
		{"gateway hint is case-insensitive", "claw", "OpenAI_Compatible", "openai_compatible/m", false, nil},
		{"gateway hint is trimmed", "claw", "  openai_compatible  ", "openai_compatible/m", false, nil},
		{"the folded auto is the absent hint", "claw", "auto", "openai_compatible/m", false, nil},
		{"Auto passes like the fold it receives", "claw", "Auto", "openai_compatible/m", false, nil},
		{"AUTO passes like the fold it receives", "claw", "AUTO", "openai_compatible/m", false, nil},
		{"a vendor hint beside a gateway model", "claw", "anthropic", "openai_compatible/m", true,
			[]string{"openai_compatible/m", "anthropic"}},

		// (b) a gateway hint on a non-gateway model is a route to nowhere.
		{"gateway hint on a vendor model", "claw", "openai_compatible", "anthropic/claude-x", true,
			[]string{"anthropic/claude-x", "openai_compatible"}},

		// (c) a gateway route on a backend that cannot speak to one.
		{"gateway model on claude_code", "claude_code", "", "openai_compatible/m", true,
			[]string{"cannot serve an OpenAI-compatible gateway route", "claude_code"}},
		{"gateway model on codex", "codex", "", "openai_compatible/m", true,
			[]string{"cannot serve an OpenAI-compatible gateway route", "codex"}},
		// The hint conflict is decided before the backend: a gateway hint
		// beside a vendor model reads as (b) on any backend.
		{"gateway hint on codex with a vendor model", "codex", "openai_compatible", "anthropic/claude-x", true,
			[]string{"is not a gateway route"}},
		{"gateway model and gateway hint on pi", "pi", "openai_compatible", "openai_compatible/m", true,
			[]string{"cannot serve an OpenAI-compatible gateway route"}},

		// (d) gateway routes on the backends that resolve to claw: unset,
		// claw, auto — case and padding included.
		{"gateway model with no backend", "", "", "openai_compatible/m", false, nil},
		{"gateway model on claw", "claw", "", "openai_compatible/m", false, nil},
		{"gateway model on an auto backend", "Auto ", "", "openai_compatible/m", false, nil},
		{"gateway model with slashes on CLAW", "CLAW", "", "openai_compatible/team-a/m", false, nil},
		{"gateway model and gateway hint on claw", "claw", "openai_compatible", "openai_compatible/team-a/m", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseGatewayCrossing(tc.backend, tc.hint, tc.model)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("refuseGatewayCrossing(%q, %q, %q) = %v, want nil", tc.backend, tc.hint, tc.model, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("refuseGatewayCrossing(%q, %q, %q) = nil, want a refusal", tc.backend, tc.hint, tc.model)
			}
			for _, frag := range tc.wantNamed {
				if !strings.Contains(err.Error(), frag) {
					t.Errorf("refusal %q does not name %q", err, frag)
				}
			}
		})
	}
}

// TestElementBuilder_RefusedGatewayElementIsABuildError pins that the
// element builder turns a refused gateway element into a BUILD error — the
// seam dispatchChain's walk routes on. Mutations that must redden it:
//   - delete the builder's checkGatewayEnv call → the env-less element
//     builds cleanly and the first subtest's "want an error" fails;
//   - delete the builder's refuseGatewayCrossing call → the crossing
//     element builds cleanly and the second subtest's "another wire"
//     fragment goes missing.
func TestElementBuilder_RefusedGatewayElementIsABuildError(t *testing.T) {
	t.Run("a gateway element with no env fails to build, naming the variable", func(t *testing.T) {
		clearGatewayEnv(t)
		e := newFallbackExecutor(delegate.NewRegistry(), EventHooks{})
		build := e.newElementBuilder("gw", delegate.BackendClaw, &backendScriptedBackend{name: delegate.BackendClaw},
			func(_ context.Context, _ string) (*delegate.Task, error) {
				return &delegate.Task{NodeID: "gw", Model: "openai_compatible/m"}, nil
			})
		_, _, _, err := build(context.Background(), 0, chainElement{Model: "openai_compatible/m"}, "")
		if err == nil {
			t.Fatal("a gateway element with no gateway env built cleanly — the builder's checkGatewayEnv is gone")
		}
		if !strings.Contains(err.Error(), "OPENAI_COMPATIBLE_BASE_URL") {
			t.Errorf("build error %q does not name the variable that is missing", err)
		}
	})

	t.Run("a gateway element crossed with a vendor hint fails to build", func(t *testing.T) {
		e := newFallbackExecutor(delegate.NewRegistry(), EventHooks{})
		build := e.newElementBuilder("gw", delegate.BackendClaw, &backendScriptedBackend{name: delegate.BackendClaw},
			func(_ context.Context, _ string) (*delegate.Task, error) {
				return &delegate.Task{NodeID: "gw", Model: "openai_compatible/m"}, nil
			})
		_, _, _, err := build(context.Background(), 0, chainElement{Provider: "anthropic", Model: "openai_compatible/m"}, "")
		if err == nil {
			t.Fatal("a gateway element carrying a vendor hint built cleanly — the builder's refuseGatewayCrossing is gone")
		}
		if !strings.Contains(err.Error(), "another wire") || !strings.Contains(err.Error(), "anthropic") {
			t.Errorf("build error %q does not say the hint names another wire", err)
		}
	})
}

// TestChainWalksPastARefusedGatewayElement pins the walk itself: a refused
// gateway element is ONE element's failure, not the node's — the chain
// reaches the vendor fallback and the node is served by it. Mutation that
// must redden it: delete the builder's checkGatewayEnv call — element 0
// then builds and serves, and the ServedBy assertion reddens.
func TestChainWalksPastARefusedGatewayElement(t *testing.T) {
	clearGatewayEnv(t)

	tail := &backendScriptedBackend{name: delegate.BackendClaw, tokens: 30}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaw, tail)
	e := newFallbackExecutor(reg, EventHooks{})
	chain := []chainElement{
		{Label: "gw", Model: "openai_compatible/m"},
		{Label: "vendor", Model: "anthropic/claude-x"},
	}
	build := e.newElementBuilder("impl", delegate.BackendClaw, tail,
		func(_ context.Context, _ string) (*delegate.Task, error) {
			return &delegate.Task{NodeID: "impl", Model: "openai_compatible/m"}, nil
		})

	out, err := e.dispatchChain(context.Background(), "impl", chain, "openai_compatible/m", build)
	if err != nil {
		t.Fatalf("the chain should have been served by the vendor element, got: %v", err)
	}
	if out.ServedBy != "vendor" {
		t.Errorf("served by %q, want the vendor fallback — a refused gateway element must not serve, nor fail the node", out.ServedBy)
	}
	if !out.FellThrough {
		t.Error("the outcome reports no fall-through — the walk to the vendor element left no trace")
	}
	if out.BackendName != delegate.BackendClaw {
		t.Errorf("serving backend = %q, want claw", out.BackendName)
	}
}

// TestClawBackendExecute_GatewayRouteResolvesItsEndpointAtTheHead pins the
// head refusal of ClawBackend.Execute: a gateway model resolves its
// endpoint before anything is dispatched, naming OPENAI_COMPATIBLE_BASE_URL
// and never a value; a private endpoint is refused until
// ITERION_LLM_ENDPOINT_ALLOW_PRIVATE lifts it, after which dispatch
// proceeds. Mutations that must redden it:
//   - delete the Execute-head block → both refusal subtests reach the stub
//     client and redden on their "stub reached" guard;
//   - make the head refuse regardless of the escape hatch (or drop the
//     strict-flag inversion) → the lifted subtest never reaches the stub.
func TestClawBackendExecute_GatewayRouteResolvesItsEndpointAtTheHead(t *testing.T) {
	const stubSentinel = "stub gateway client reached"
	newBackend := func(t *testing.T) *ClawBackend {
		t.Helper()
		reg := NewRegistry()
		reg.Register(modelroute.OpenAICompatible, func(modelID string) (api.APIClient, error) {
			return &execMockClient{err: errors.New(stubSentinel)}, nil
		})
		return NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
	}
	task := delegate.Task{NodeID: "gw", Model: "openai_compatible/m", UserPrompt: "go"}

	t.Run("no env is refused at the head, naming the variable", func(t *testing.T) {
		clearGatewayEnv(t)
		_, err := newBackend(t).Execute(context.Background(), task)
		if err == nil {
			t.Fatal("a gateway model with no gateway env executed — the head refusal is gone")
		}
		if !strings.Contains(err.Error(), "OPENAI_COMPATIBLE_BASE_URL") {
			t.Errorf("the refusal %q does not name the variable that is missing", err)
		}
		if strings.Contains(err.Error(), stubSentinel) {
			t.Errorf("the head refusal did not fire before dispatch: %v", err)
		}
	})

	t.Run("a private endpoint is refused before dispatch", func(t *testing.T) {
		clearGatewayEnv(t)
		t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "http://127.0.0.1:9/v1")
		_, err := newBackend(t).Execute(context.Background(), task)
		if err == nil {
			t.Fatal("a private gateway endpoint executed with the escape hatch unset")
		}
		if !strings.Contains(err.Error(), "not a public") {
			t.Errorf("the refusal %q does not say the endpoint is not public", err)
		}
		if strings.Contains(err.Error(), stubSentinel) {
			t.Errorf("the private-endpoint refusal did not fire before dispatch: %v", err)
		}
	})

	t.Run("allow-private lifts the refusal and dispatch proceeds", func(t *testing.T) {
		clearGatewayEnv(t)
		t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "http://127.0.0.1:9/v1")
		t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
		_, err := newBackend(t).Execute(context.Background(), task)
		if err == nil || !strings.Contains(err.Error(), stubSentinel) {
			t.Fatalf("Execute = %v, want the stub client's error — the head must let a lifted endpoint through to dispatch", err)
		}
	})
}

// TestForwardableProviderEnv_GatewayEnvRidesOnlyGatewayNodes pins the
// sandbox env seam: the gateway endpoint (and its key, when set) crosses to
// the in-container runner for a gateway node ONLY, with the escape hatch
// forwarded explicitly when lifted; a vendor node's map never carries an
// OPENAI_COMPATIBLE_* pair. Mutations that must redden it:
//   - delete the gateway block → the gateway subtests miss their entries;
//   - make the block unconditional → the vendor subtest finds the endpoint;
//   - drop the APIKey!= "" guard → the lifted subtest finds name=<empty>;
//   - drop the ALLOW_PRIVATE forwarding → the lifted subtest misses it.
func TestForwardableProviderEnv_GatewayEnvRidesOnlyGatewayNodes(t *testing.T) {
	// Keep the codex version probe off the process: no exec, no clock.
	t.Setenv("ITERION_CODEX_VERSION", "0.0.0-test")

	t.Run("a gateway node carries the endpoint and the key when set", func(t *testing.T) {
		clearGatewayEnv(t)
		// A public IP literal: strict validation would resolve a name, and
		// this test dials nothing.
		t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://93.184.216.34/v1")
		t.Setenv("OPENAI_COMPATIBLE_API_KEY", "gw-secret")
		env, err := forwardableProviderEnv(context.Background(), "openai_compatible/m")
		if err != nil {
			t.Fatalf("forwardableProviderEnv: %v", err)
		}
		if env["OPENAI_COMPATIBLE_BASE_URL"] != "https://93.184.216.34/v1" {
			t.Errorf("endpoint in the sandbox env = %q, want the operator's gateway", env["OPENAI_COMPATIBLE_BASE_URL"])
		}
		if env["OPENAI_COMPATIBLE_API_KEY"] != "gw-secret" {
			t.Errorf("gateway key in the sandbox env = %q, want the set key", env["OPENAI_COMPATIBLE_API_KEY"])
		}
		if v, ok := env["ITERION_LLM_ENDPOINT_ALLOW_PRIVATE"]; ok {
			t.Errorf("the escape hatch crossed although it was not lifted (=%q)", v)
		}
	})

	t.Run("a lifted private endpoint tells the in-container factory", func(t *testing.T) {
		clearGatewayEnv(t)
		t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "http://127.0.0.1:9/v1")
		t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
		env, err := forwardableProviderEnv(context.Background(), "openai_compatible/m")
		if err != nil {
			t.Fatalf("forwardableProviderEnv: %v", err)
		}
		if env["ITERION_LLM_ENDPOINT_ALLOW_PRIVATE"] != "1" {
			t.Errorf("the lifted escape hatch did not cross (=%q) — the in-container factory would re-derive and refuse it", env["ITERION_LLM_ENDPOINT_ALLOW_PRIVATE"])
		}
		if _, ok := env["OPENAI_COMPATIBLE_API_KEY"]; ok {
			t.Error("an unset gateway key crossed as a name=<empty> pair — some providers read that as an attempted auth")
		}
	})

	t.Run("a vendor node carries neither gateway variable", func(t *testing.T) {
		clearGatewayEnv(t)
		t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://93.184.216.34/v1")
		t.Setenv("OPENAI_COMPATIBLE_API_KEY", "gw-secret")
		env, err := forwardableProviderEnv(context.Background(), "anthropic/claude-x")
		if err != nil {
			t.Fatalf("forwardableProviderEnv: %v", err)
		}
		for _, name := range []string{"OPENAI_COMPATIBLE_BASE_URL", "OPENAI_COMPATIBLE_API_KEY"} {
			if v, ok := env[name]; ok {
				t.Errorf("vendor node's sandbox env carries %s=%q — operator gateway infrastructure leaked to a non-gateway node", name, v)
			}
		}
	})
}

// gatewayAgent builds an agent node the way override_fold_test.go does:
// backend and model on the LLM fields, no provider chain.
func gatewayAgent(id, backend, mdl string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Backend: backend, Model: mdl},
	}
}

// TestEffectiveProviders_GatewayRouteIsEnvFundedNotUnknown pins the fold:
// an openai_compatible prefix is recorded as EnvFunded and keeps the run
// narrow-UNSAFE, without entering Providers or Unknown — the name is not a
// typo, and the spend books on nobody. Mutations that must redden it:
//   - stop collecting envFunded → EnvFunded goes empty;
//   - drop the narrowSafe=false on that branch → a caller would narrow a
//     run whose env-funded half names no credential;
//   - let the prefix fall through to hint() → it lands in Unknown.
func TestEffectiveProviders_GatewayRouteIsEnvFundedNotUnknown(t *testing.T) {
	// #2109's shipped semantics, kept on the rebase: a gateway route is
	// recorded in EnvFunded and NEVER widens — it acquires no credential,
	// so narrowing to the named providers is exact (OnlyEnvFunded is the
	// publisher's predicate). Unknown stays the typo channel.
	t.Setenv("OPENAI_COMPATIBLE_MODELS", "")
	wf := func(nodes map[string]ir.Node) *ir.Workflow {
		return &ir.Workflow{Nodes: nodes}
	}
	gwOnly := wf(map[string]ir.Node{
		"gw": gatewayAgent("gw", "claw", "openai_compatible/team/m"),
	})
	res := EffectiveProviders(gwOnly, ModelOverrides{}, nil, knownForTest)
	if len(res.EnvFunded) != 1 || res.EnvFunded[0] != "openai_compatible" {
		t.Errorf("EnvFunded = %v, want [openai_compatible]", res.EnvFunded)
	}
	if len(res.Providers) != 0 || len(res.Unknown) != 0 {
		t.Errorf("Providers = %v Unknown = %v, want both empty (a gateway id is not a typo)", res.Providers, res.Unknown)
	}
	if !res.NarrowSafe {
		t.Errorf("NarrowSafe = false — the gateway acquires no credential, narrowing to the (empty) named set is exact")
	}

	mixed := wf(map[string]ir.Node{
		"gw":     gatewayAgent("gw", "claw", "openai_compatible/team/m"),
		"vendor": gatewayAgent("vendor", "claw", "anthropic/claude-opus-5"),
	})
	res = EffectiveProviders(mixed, ModelOverrides{}, nil, knownForTest)
	if len(res.EnvFunded) != 1 || len(res.Providers) != 1 || res.Providers[0] != "anthropic" {
		t.Errorf("EnvFunded = %v Providers = %v, want the gateway named apart and the vendor in Providers", res.EnvFunded, res.Providers)
	}

	vendorOnly := wf(map[string]ir.Node{
		"vendor": gatewayAgent("vendor", "claw", "anthropic/claude-opus-5"),
	})
	res = EffectiveProviders(vendorOnly, ModelOverrides{}, nil, knownForTest)
	if len(res.EnvFunded) != 0 || !res.NarrowSafe {
		t.Errorf("a vendor-only run: EnvFunded = %v NarrowSafe = %v, want empty and safe", res.EnvFunded, res.NarrowSafe)
	}
}

// TestRouteOnAnthropicWire_GatewayModelAnswersFalse pins what
// wire_reach.go already does: a gateway route is not an anthropic-wire
// route, so the weekly-cap pre-flight does not park a run that cannot spend
// the capped subscription. Mutation that must redden it: teach the
// anthropic-wire guard the gateway prefix (or make the model prefix fall
// through as unresolved on claw) → the answer flips to true.
func TestRouteOnAnthropicWire_GatewayModelAnswersFalse(t *testing.T) {
	for _, model := range []string{"openai_compatible/m", "openai_compatible/team-a/m"} {
		if routeOnAnthropicWire(delegate.BackendClaw, "", "", model) {
			t.Errorf("routeOnAnthropicWire(claw, %q) = true — a gateway route answered reachable on the anthropic wire", model)
		}
	}
}

// A gateway-served node's parse-fallback recovery is a VENDOR call the
// deployment never asked for (the detector would pick a vendor default):
// it is skipped, named, and the node keeps its parse-fallback outcome. Red
// when the skip is deleted — the rescue would then succeed through the
// detected vendor provider and ok would come back true.
func TestExtractStructuredViaClaw_AGatewayNodeSkipsTheVendorRecovery(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key") // the detector WOULD find a vendor
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "")
	schema := &ir.Schema{Name: "out_schema", Fields: []*ir.SchemaField{{Name: "answer", Type: ir.FieldTypeString}}}
	schemaJSON, err := SchemaToJSON(schema)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	clawReg := NewRegistry()
	clawReg.Register("anthropic", func(string) (api.APIClient, error) {
		return newMockClient(toolUseEvents("tu_1", "structured_output", `{"answer":"blue"}`, 200, 50)), nil
	})
	delegateReg := delegate.NewRegistry()
	delegateReg.Register("test_backend", &capturingBackend{results: []delegate.Result{{
		Output: map[string]any{"text": "the answer is blue"}, Tokens: 500, ParseFallback: true, BackendName: "test_backend",
	}}})
	exec := NewClawExecutor(clawReg,
		&ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{"out_schema": schema}},
		WithBackendRegistry(delegateReg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
	)
	backend, err := delegateReg.Resolve("test_backend")
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	first := delegate.Result{Output: map[string]any{"text": "the answer is blue"}, Tokens: 1_000, ParseFallback: true, BackendName: "test_backend"}
	if _, err := exec.validateAndRetry(context.Background(), backendFields{id: "answerer", outputSchema: "out_schema"},
		"test_backend", backend, &delegate.Task{Model: "openai_compatible/team/m", OutputSchema: schemaJSON}, first, schema); err == nil {
		t.Fatal("the vendor recovery rescued a gateway-served node — the skip is gone")
	}
}

// A gateway route's compaction trigger sits at its CATALOG window × the
// effective ratio (authored when > 0, else 0.85); an unknown window keeps
// claw's default floor, and a vendor's window is never borrowed by name.
// Red when compactionConfig reads the spec's own spelling (the vendor
// tables' unknown threshold) or drops the ratio.
func TestCompactionConfig_AGatewayRouteSizesFromItsCatalogWindow(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_MODELS", `{"m1":{"context_window":200000}}`)
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("ITERION_OPENAI_COMPATIBLE_RESOLVED", "")
	compatgw.ResetCatalogCaches()

	if got := compactionConfig("openai_compatible/m1", 0, 0).MaxEstimatedTokens; got != int(200000*0.85) {
		t.Errorf("default-ratio trigger = %d, want 200000×0.85", got)
	}
	if got := compactionConfig("openai_compatible/m1", 0.5, 0).MaxEstimatedTokens; got != 100000 {
		t.Errorf("authored-ratio trigger = %d, want 100000", got)
	}
	compatgw.ResetCatalogCaches()
	t.Setenv("OPENAI_COMPATIBLE_MODELS", "")
	if got := compactionConfig("openai_compatible/no-such", 0, 0).MaxEstimatedTokens; got != clawrt.DefaultCompactionConfig().MaxEstimatedTokens {
		t.Errorf("unknown-window trigger = %d, want claw's default floor", got)
	}
}

// The invocation's catalog record rides the result: provenance on the
// output (`_gateway_spec`, unknown recorded AS unknown) and the gateway's
// window as the effective context window. Red when the stamp or the
// window wiring is dropped.
func TestClawBackend_TheCatalogRecordRidesTheResult(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_MODELS", `{"gw/m":{"context_window":123456,"input_usd_per_mtok":1,"output_usd_per_mtok":2}}`)
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://198.51.100.7")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-test")
	compatgw.ResetCatalogCaches()

	reg := NewRegistry()
	stub := &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents(`{"answer":"blue"}`, "end_turn")}}
	reg.Register("openai_compatible", func(string) (api.APIClient, error) { return stub, nil })
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{MaxAttempts: 1})
	res, err := backend.Execute(context.Background(), delegate.Task{
		NodeID: "gw", Model: "openai_compatible/gw/m", UserPrompt: "q",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	raw, _ := res.Output["_gateway_spec"].(json.RawMessage)
	if raw == nil {
		t.Fatalf("output %v carries no _gateway_spec", res.Output)
	}
	if !strings.Contains(string(raw), `"source":"operator"`) || !strings.Contains(string(raw), `"context_window":123456`) {
		t.Errorf("_gateway_spec = %s, want the operator provenance and window", raw)
	}
	if res.ContextWindow != 123456 {
		t.Errorf("Result.ContextWindow = %d, want the gateway's", res.ContextWindow)
	}
}

// The host's resolution crosses into the sandbox with the gateway env, so
// the in-container catalog answers with the same record. Red when the
// forward is dropped.
func TestForwardableProviderEnv_CarriesTheHostResolution(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_MODELS", `{"gw/m":{"context_window":123456}}`)
	t.Setenv("OPENAI_COMPATIBLE_CATALOG_PROVIDER", "")
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://198.51.100.7")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-test")
	compatgw.ResetCatalogCaches()

	env, err := forwardableProviderEnv(context.Background(), "openai_compatible/gw/m")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	raw, ok := env["ITERION_OPENAI_COMPATIBLE_RESOLVED"]
	if !ok || !strings.Contains(raw, `"model":"gw/m"`) || !strings.Contains(raw, `"source":"operator"`) {
		t.Errorf("env carries %v, want the forwarded record", raw)
	}
	// A vendor node never sees it.
	env, err = forwardableProviderEnv(context.Background(), "anthropic/claude-opus-5")
	if err != nil {
		t.Fatalf("forwardableProviderEnv: %v", err)
	}
	if _, ok := env["ITERION_OPENAI_COMPATIBLE_RESOLVED"]; ok {
		t.Error("a vendor node received the gateway resolution")
	}
}

// A malformed OPENAI_COMPATIBLE_MODELS REFUSES the route at the seam —
// naming the variable — instead of silently degrading the gateway to an
// unknown, unpriced record while the run continues. Red when the seam
// probe stops judging the operator table.
func TestCheckGatewayEnv_AMalformedOperatorTableRefuses(t *testing.T) {
	t.Setenv("OPENAI_COMPATIBLE_BASE_URL", "https://gw.example.com")
	t.Setenv("OPENAI_COMPATIBLE_API_KEY", "sk-test")
	t.Setenv("OPENAI_COMPATIBLE_MODELS", `{"gw/m":{"input_usd_per_mtok":0.1}}`) // unpaired price
	compatgw.ResetCatalogCaches()

	err := checkGatewayEnv(false)
	if err == nil || !strings.Contains(err.Error(), "OPENAI_COMPATIBLE_MODELS") {
		t.Fatalf("checkGatewayEnv = %v, want the malformed-table refusal naming the variable", err)
	}
}
