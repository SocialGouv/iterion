package supervise

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
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
			specs := SpecsFromWorkflow(wf(tc.node), iterlog.Nop())
			if len(specs) != 1 {
				t.Fatalf("specs = %d, want 1", len(specs))
			}
			if got := specs[0].ProviderHint; got != tc.want {
				t.Fatalf("ProviderHint = %q, want %q", got, tc.want)
			}
		})
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
	specs := SpecsFromWorkflow(wf, iterlog.Nop())
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

// ctxFundsProvider maps the run's ctx credentials onto claw providers:
// a per-provider API key funds its provider; an OAuth-forfait file funds
// the provider its CLI belongs to (claude_code → anthropic,
// codex → openai).
func TestCtxFundsProvider(t *testing.T) {
	f := ctxFundsProvider(secrets.Credentials{
		APIKeys:              map[secrets.Provider]string{"zai": "zk"},
		OAuthCredentialFiles: map[string]string{"claude_code": "/tmp/cc"},
	})
	for provider, want := range map[string]bool{
		"zai":       true,
		"anthropic": true,
		"openai":    false,
		"xai":       false,
	} {
		if got := f(provider); got != want {
			t.Errorf("ctxFundsProvider(%q) = %v, want %v", provider, got, want)
		}
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
	specs := SpecsFromWorkflow(wf, iterlog.Nop())
	if len(specs) != 1 || specs[0].ProviderHint != "anthropic" {
		t.Fatalf("bogus alias prefix must be skipped and the next watched node consulted, got %+v", specs)
	}
}
