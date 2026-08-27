package supervise

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/detect"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// resolveModelWith precedence: pin → env → hinted provider (only when that
// provider is detected available) → first available provider.
func TestResolveModelWithHintPrecedence(t *testing.T) {
	providers := []detect.ProviderStatus{
		{Name: "openai", Available: true, SuggestedModel: "openai/gpt-5.4-mini"},
		{Name: "anthropic", Available: true, SuggestedModel: "anthropic/claude-opus-5"},
	}

	if got, _ := resolveModelWith("anthropic/claude-opus-5", "openai", providers); got != "anthropic/claude-opus-5" {
		t.Fatalf("pin must win, got %q", got)
	}
	if got, _ := resolveModelWith("", "anthropic", providers); got != "anthropic/claude-opus-5" {
		t.Fatalf("hinted provider must win over detector order, got %q", got)
	}
	// A hint for a provider with no credential must fall back, not fail.
	if got, _ := resolveModelWith("", "zai", providers); got != "openai/gpt-5.4-mini" {
		t.Fatalf("unavailable hint must fall back to the first available provider, got %q", got)
	}
	if got, _ := resolveModelWith("", "", providers); got != "openai/gpt-5.4-mini" {
		t.Fatalf("no hint keeps the detector order, got %q", got)
	}
	if _, err := resolveModelWith("", "anthropic", nil); err == nil {
		t.Fatal("no providers at all must return ErrNoSupervisorModel")
	}
}
