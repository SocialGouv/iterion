package supervise

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A supervisor with no model pin should evaluate with the SAME provider
// family as the run it supervises, not with whatever provider the host
// detector lists first: on the prod pods a dead platform OPENAI key made
// every Persy eval 429 while the supervised campaign was happily running
// on the Anthropic credential (run 01a042c2).
func TestSpecsFromWorkflowDeriveProviderHint(t *testing.T) {
	// The env-ref case below reads its dial through the EXPANDING process's
	// environment: blanked here so the case answers from the fixture's own
	// default, not from the shell. An operator with this repo's Revi dials
	// exported (`ITERION_VIBE_MODEL_CLAUDE=openai/…`) otherwise reads a hint
	// regression where only the shell changed. assertNoHostAnsweredDial
	// below is what keeps that one line honest as the table grows: the
	// freeze is checked against the reader, not trusted as a list.
	t.Setenv("ITERION_VIBE_MODEL_CLAUDE", "")
	wf := func(node ir.Node) *ir.Workflow {
		return &ir.Workflow{
			Nodes: map[string]ir.Node{"campaign": node},
			Supervisors: []*ir.Supervisor{{
				Name:    "persy",
				Watches: []string{"campaign"},
			}},
			Prompts: map[string]*ir.Prompt{},
		}
	}

	cases := []struct {
		name string
		node ir.Node
		want string
	}{
		{
			name: "claude_code backend maps to anthropic",
			node: &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "claude_code", Model: "claude-opus-5"}},
			want: "anthropic",
		},
		{
			name: "claw model provider prefix wins",
			node: &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "claw", Model: "openai/gpt-5.4-mini"}},
			want: "openai",
		},
		{
			name: "explicit provider field wins over everything",
			node: &ir.AgentNode{LLMFields: ir.LLMFields{Provider: "zai,anthropic", Backend: "claude_code", Model: "claude-opus-5"}},
			want: "zai",
		},
		{
			name: "env-ref model still resolves the family from the backend",
			node: &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "claude_code", Model: "${ITERION_VIBE_MODEL_CLAUDE:-claude-opus-5}"}},
			want: "anthropic",
		},
		{
			name: "codex backend maps to openai",
			node: &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "codex", Model: "gpt-5.5"}},
			want: "openai",
		},
		{
			name: "judge nodes count too",
			node: &ir.JudgeNode{LLMFields: ir.LLMFields{Backend: "claude_code"}},
			want: "anthropic",
		},
		{
			name: "unknown backend yields no hint",
			node: &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "kimi", Model: "kimi-for-coding"}},
			want: "",
		},
		{
			// kimi's canonical model alias carries a slash that is NOT a
			// provider prefix (kimiMapModel); it must not become a hint.
			name: "non-provider alias prefix yields no hint",
			node: &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "kimi", Model: "kimi-code/kimi-for-coding"}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if llm, ok := tc.node.(ir.LLMNode); ok {
				f := llm.GetLLMFields()
				assertNoHostAnsweredDial(t, f.Model, f.Backend, f.Provider)
			}
			specs := SpecsFromWorkflow(wf(tc.node), nil, iterlog.Nop())
			if len(specs) != 1 {
				t.Fatalf("specs = %d, want 1", len(specs))
			}
			if got := specs[0].ProviderHint; got != tc.want {
				t.Fatalf("ProviderHint = %q, want %q", got, tc.want)
			}
		})
	}
}

// assertNoHostAnsweredDial fatals when a case's own field text is still
// answered by the process environment: the derivation's reader
// (ir.ExpandEnvWithDefault) must give what a reader answering nothing gives.
//
// Asked of the READER rather than of a list of names, because the freeze
// above is one line for one dial and the table it protects grows: a case
// that adds a dial — in any form the expander resolves, `${NAME}`,
// `${NAME:-default}` or the bare `$NAME` — then refuses the run on the host
// that would have flipped its hint, instead of asserting the host's answer.
// The same rule guards the routing fixtures of
// pkg/server/cloudpublisher (freezeHostDials); each package keeps its own
// copy because a test helper is not shared API.
func assertNoHostAnsweredDial(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		got, want := ir.ExpandEnvWithDefault(s), ir.ExpandWithDefault(s, func(string) string { return "" })
		if got != want {
			t.Fatalf("this host still answers a reference the case carries: the derivation's reader gives %q where an unset environment gives %q — freeze that dial", got, want)
		}
	}
}

// A pinned supervisor model must not grow a hint: the pin already decides.
func TestSpecsFromWorkflowPinnedModelSkipsHint(t *testing.T) {
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{"campaign": &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "claude_code"}}},
		Supervisors: []*ir.Supervisor{{
			Name:    "persy",
			Model:   "openai/gpt-5.4-mini",
			Watches: []string{"campaign"},
		}},
	}
	specs := SpecsFromWorkflow(wf, nil, iterlog.Nop())
	if len(specs) != 1 || specs[0].ProviderHint != "" {
		t.Fatalf("pinned model must not derive a hint, got %+v", specs)
	}
}

// resolveModelWith precedence: pin → env → hinted provider (when the
// detector sees it OR the run's ctx credentials fund it) → first
// available provider.
func TestResolveModelWithHintPrecedence(t *testing.T) {
	providers := []detect.ProviderStatus{
		{Name: "openai", Available: true, SuggestedModel: "openai/gpt-5.4-mini"},
		{Name: "anthropic", Available: true, SuggestedModel: "anthropic/claude-opus-5"},
	}
	noCtx := func(string) bool { return false }

	if got, _ := resolveModelWith("anthropic/claude-opus-5", "openai", providers, noCtx); got != "anthropic/claude-opus-5" {
		t.Fatalf("pin must win, got %q", got)
	}
	if got, _ := resolveModelWith("", "anthropic", providers, noCtx); got != "anthropic/claude-opus-5" {
		t.Fatalf("hinted provider must win over detector order, got %q", got)
	}
	// A hint for a provider with no credential anywhere must fall back, not fail.
	if got, _ := resolveModelWith("", "zai", providers, noCtx); got != "openai/gpt-5.4-mini" {
		t.Fatalf("unfunded hint must fall back to the first available provider, got %q", got)
	}
	if got, _ := resolveModelWith("", "", providers, noCtx); got != "openai/gpt-5.4-mini" {
		t.Fatalf("no hint keeps the detector order, got %q", got)
	}
	if _, err := resolveModelWith("", "anthropic", nil, noCtx); err == nil {
		t.Fatal("no providers at all must return ErrNoSupervisorModel")
	}
}

// The local gateway row (#2646) carries no suggested model — a gateway's
// model id is the deployment's own naming — so a host whose ONLY detected
// credential is the gateway must still resolve no supervisor model rather
// than invent one.
func TestResolveModelWithGatewayRowSuggestsNothing(t *testing.T) {
	providers := []detect.ProviderStatus{
		{Name: "openai_compatible", Available: true, Source: "OPENAI_COMPATIBLE_BASE_URL"},
	}
	noCtx := func(string) bool { return false }
	if _, err := resolveModelWith("", "", providers, noCtx); !errors.Is(err, ErrNoSupervisorModel) {
		t.Errorf("gateway-only host: err = %v, want ErrNoSupervisorModel", err)
	}
	if got, err := resolveModelWith("", "openai_compatible", providers, noCtx); err == nil || got != "" {
		t.Errorf("a gateway hint with no suggested model must not resolve, got (%q, %v)", got, err)
	}
}

// The pod scenario the hint exists for: the run's credentials live in ctx
// (sealed bundle / OAuth-forfait files), NOT in the process env — so the
// detector reports the hinted provider unavailable while the claw
// registry can perfectly authenticate it from ctx at call time. The hint
// must be honoured on ctx funding, using the provider's suggested model
// (populated by detect regardless of availability).
func TestResolveModelWithHonoursCtxFundedHint(t *testing.T) {
	providers := []detect.ProviderStatus{
		{Name: "openai", Available: true, SuggestedModel: "openai/gpt-5.4-mini"},
		{Name: "anthropic", Available: false, SuggestedModel: "anthropic/claude-opus-5"},
	}
	ctxFunded := func(p string) bool { return p == "anthropic" }

	got, err := resolveModelWith("", "anthropic", providers, ctxFunded)
	if err != nil || got != "anthropic/claude-opus-5" {
		t.Fatalf("ctx-funded hint must be honoured over the env detector, got (%q, %v)", got, err)
	}
	// Without ctx funding the same hint falls back — no auth-error swap.
	if got, _ := resolveModelWith("", "anthropic", providers, func(string) bool { return false }); got != "openai/gpt-5.4-mini" {
		t.Fatalf("unfunded unavailable hint must fall back, got %q", got)
	}
}

// ctxFundsProvider: a per-provider API key funds its provider, and each
// subscription forfait funds the provider ResolveWithContext has a ctx branch
// for — codex → openai, claude_code → anthropic. This map must stay in step
// with those branches in both directions: claiming a provider nothing can
// build resolves an unauthenticated client (401 per eval), and denying one
// that IS buildable parks supervision for no reason.
func TestCtxFundsProvider(t *testing.T) {
	creds := secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{"zai": "zk"},
		OAuthCredentialFiles: map[string]string{"claude_code": "/tmp/cc", "codex": "/tmp/cx"},
	}

	t.Run("forfaits fund their provider", func(t *testing.T) {
		t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
		t.Setenv("ANTHROPIC_BASE_URL", "")
		t.Setenv("OPENAI_BASE_URL", "")
		t.Setenv("ITERION_OPENAI_USE_OAUTH", "")
		f := ctxFundsProvider(creds)
		for provider, want := range map[string]bool{
			"zai":       true,
			"anthropic": true, // via the claude_code forfait
			"openai":    true, // via the codex forfait
			"xai":       false,
		} {
			if got := f(provider); got != want {
				t.Errorf("ctxFundsProvider(%q) = %v, want %v", provider, got, want)
			}
		}
	})

	// Each kill-switch that makes the registry's ctx branch decline must make
	// this predicate decline too, or the hint resolves a model the branch
	// refuses to build.
	for name, tc := range map[string]struct{ key, val, provider string }{
		"FORBID_SUBSCRIPTION_OAUTH": {"ITERION_FORBID_SUBSCRIPTION_OAUTH", "1", "anthropic"},
		"anthropic z.ai facade":     {"ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic", "anthropic"},
		"openai oauth disabled":     {"ITERION_OPENAI_USE_OAUTH", "0", "openai"},
		"openai base url override":  {"OPENAI_BASE_URL", "http://localhost:1234", "openai"},
	} {
		t.Run(name+" → unfunded", func(t *testing.T) {
			t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
			t.Setenv("ANTHROPIC_BASE_URL", "")
			t.Setenv("OPENAI_BASE_URL", "")
			t.Setenv("ITERION_OPENAI_USE_OAUTH", "")
			t.Setenv(tc.key, tc.val)
			if ctxFundsProvider(creds)(tc.provider) {
				t.Errorf("ctxFundsProvider(%q) = true with %s=%s, want false", tc.provider, tc.key, tc.val)
			}
		})
	}
}

// The composition the pod runs: a ctx-sealed BYOK key with an empty env
// must yield a working evaluator client for that provider (Resolve alone
// — the env-only path — cannot build it). The runner wires the
// credentials lookup at boot; the test wires the same canonical closure.
func TestEvaluatorClientResolvesFromCtxCredentials(t *testing.T) {
	t.Cleanup(model.SetCredentialsLookup(func(ctx context.Context) (func(provider string) string, bool) {
		creds, ok := secrets.CredentialsFromContext(ctx)
		if !ok {
			return nil, false
		}
		return func(provider string) string { return creds.APIKeys[secrets.Provider(provider)] }, true
	}))
	e := NewLLMEvaluator()
	ctx := secrets.WithCredentials(context.Background(),
		secrets.Credentials{APIKeys: map[secrets.Provider]string{"anthropic": "sk-test"}})
	client, err := e.registry.ResolveWithContext(ctx, "anthropic/claude-opus-5")
	if err != nil || client == nil {
		t.Fatalf("ResolveWithContext with a ctx BYOK key = (%v, %v); want a client", client, err)
	}
}

// A whole-run supervisor (no watches:) supervises every node — its hint
// derives from the workflow's agent/judge nodes generally, in
// deterministic (sorted) order.
func TestProviderHintWholeRunDerivesFromAllNodes(t *testing.T) {
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{
			"a-tool":     &ir.ToolNode{},
			"b-campaign": &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "claude_code", Model: "claude-opus-5"}},
		},
		Supervisors: []*ir.Supervisor{{Name: "persy"}},
	}
	specs := SpecsFromWorkflow(wf, nil, iterlog.Nop())
	if len(specs) != 1 || specs[0].ProviderHint != "anthropic" {
		t.Fatalf("whole-run supervisor must derive its hint from the workflow's LLM nodes, got %+v", specs)
	}
}

// A provider: chain is walked until an entry claw can route — the first
// element is not returned verbatim when it is not a claw provider.
func TestProviderHintWalksProviderChain(t *testing.T) {
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{
			"campaign": &ir.AgentNode{LLMFields: ir.LLMFields{Provider: "not-a-provider,anthropic", Backend: "claude_code"}},
		},
		Supervisors: []*ir.Supervisor{{Name: "persy", Watches: []string{"campaign"}}},
	}
	specs := SpecsFromWorkflow(wf, nil, iterlog.Nop())
	if len(specs) != 1 || specs[0].ProviderHint != "anthropic" {
		t.Fatalf("provider chain must be walked to the first claw-routable entry, got %+v", specs)
	}
}

// A bogus hint from an alias prefix must not stop a LATER watched node
// from supplying the real hint.
func TestProviderHintSkipsBogusPrefixAndContinues(t *testing.T) {
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{
			"scout":    &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "kimi", Model: "kimi-code/kimi-for-coding"}},
			"campaign": &ir.AgentNode{LLMFields: ir.LLMFields{Backend: "claude_code", Model: "claude-opus-5"}},
		},
		Supervisors: []*ir.Supervisor{{Name: "persy", Watches: []string{"scout", "campaign"}}},
	}
	specs := SpecsFromWorkflow(wf, nil, iterlog.Nop())
	if len(specs) != 1 || specs[0].ProviderHint != "anthropic" {
		t.Fatalf("bogus alias prefix must be skipped and the next watched node consulted, got %+v", specs)
	}
}

// TestEvaluateStampsProviderHintOnResolution: the evaluator's registry
// resolution reads the same hint the supervised nodes were routed with.
// `provider: "anthropic"` + ZAI_API_KEY in the env must REFUSE by type (the
// z.ai env synthesis skipped) — never evaluate on z.ai while the supervised
// nodes are forced Anthropic-direct (#1718's class, supervisor seam).
func TestEvaluateStampsProviderHintOnResolution(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "zai-key-not-for-evals")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	e := NewLLMEvaluator()
	_, _, err := e.Evaluate(context.Background(), EvalInput{Spec: Spec{
		Name:         "persy",
		Model:        "anthropic/claude-sonnet-4-6",
		ProviderHint: "anthropic",
	}})
	var unfunded *model.ErrProviderHintUnfunded
	if !errors.As(err, &unfunded) {
		t.Fatalf("err = %v, want a wrapped *model.ErrProviderHintUnfunded — the eval must not resolve z.ai under an anthropic hint", err)
	}
}
