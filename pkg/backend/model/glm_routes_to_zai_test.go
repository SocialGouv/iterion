package model

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// A GLM id is a z.ai model whatever prefix it rides — claw spells it
// `anthropic/glm-*`, claude_code takes it bare — and every site that decides
// which credential a route spends reads it that way. Pinning `anthropic` for
// it would fund the Claude forfait, which api.anthropic.com refuses for a
// model it does not serve — GLM would stop working on claw the moment a
// forfait held the wire.

func TestGLMRouteIsFundedWithTheZAIKey(t *testing.T) {
	for _, tc := range []struct {
		name, backend, model string
	}{
		{"claw anthropic/glm", "claw", "anthropic/glm-5.3"},
		{"claude_code bare glm, no hint", "claude_code", "glm-5.3"},
		{"a GLM dial's default", "claude_code", "${ITERION_TEST_GLM_DIAL:-glm-5.3}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := &ir.Workflow{Nodes: map[string]ir.Node{"n": &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "n"},
				LLMFields: ir.LLMFields{Backend: tc.backend, Model: tc.model},
			}}}
			res := EffectiveProviders(wf, ModelOverrides{}, nil, map[string]bool{"anthropic": true, "zai": true, "openai": true})
			if !slices.Contains(res.Providers, "zai") {
				t.Errorf("providers = %v, want zai — a GLM route must be funded with the z.ai key", res.Providers)
			}
			if slices.Contains(res.Providers, "anthropic") {
				t.Errorf("providers = %v — a GLM route funded the anthropic credential, which cannot serve it", res.Providers)
			}
		})
	}
}

// A GLM id another provider serves is THAT provider's route: z.ai is the
// anthropic wire's GLM, not every GLM's.
func TestGLMOnAnotherProviderKeepsItsOwnRoute(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"n": &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "n"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "openrouter/z-ai/glm-4.6"},
	}}}
	res := EffectiveProviders(wf, ModelOverrides{}, nil, map[string]bool{"anthropic": true, "zai": true, "openrouter": true})
	if !slices.Contains(res.Providers, "openrouter") || slices.Contains(res.Providers, "zai") {
		t.Errorf("providers = %v, want openrouter alone — an OpenRouter GLM route was funded with the z.ai key", res.Providers)
	}
	if got := clawPinnedProvider("openrouter/z-ai/glm-4.6"); got != secrets.ProviderOpenRouter {
		t.Errorf("clawPinnedProvider(openrouter/z-ai/glm-4.6) = %q, want openrouter", got)
	}
}

// The sandboxed claw node carries the key its route is funded with: for
// `anthropic/glm-*` that is ZAI_API_KEY, from which the in-container registry
// synthesises z.ai's endpoint.
func TestClawPinnedProviderForGLMIsZAI(t *testing.T) {
	if got := clawPinnedProvider("anthropic/glm-5.3"); got != secrets.ProviderZAI {
		t.Errorf("clawPinnedProvider(anthropic/glm-5.3) = %q, want zai", got)
	}
	if got := clawPinnedProvider("anthropic/claude-opus-5-5"); got != secrets.ProviderAnthropic {
		t.Errorf("clawPinnedProvider(anthropic/claude-opus-5-5) = %q, want anthropic", got)
	}
}

// In process, `anthropic/glm-*` spends the run's z.ai key and never reaches
// for the Claude forfait: the OAuth-dir lookup that feeds the forfait path is
// not even consulted.
func TestClawInProcessGLMSpendsTheZAIKey(t *testing.T) {
	forfaitConsulted := false
	SetCredentialsLookup(func(context.Context) (func(string) string, bool) {
		return func(provider string) string {
			if provider == string(secrets.ProviderZAI) {
				return "sk-zai-run"
			}
			return ""
		}, true
	})
	SetOAuthDirLookup(func(context.Context) (func(string) string, bool) {
		return func(string) string {
			forfaitConsulted = true
			return "/nonexistent/forfait"
		}, true
	})
	t.Cleanup(func() {
		SetCredentialsLookup(func(context.Context) (func(string) string, bool) { return nil, false })
		SetOAuthDirLookup(func(context.Context) (func(string) string, bool) { return nil, false })
	})

	client, err := NewRegistry().ResolveWithContext(context.Background(), "anthropic/glm-5.3")
	if err != nil {
		t.Fatalf("ResolveWithContext(anthropic/glm-5.3): %v", err)
	}
	cc, ok := client.(*api.Client)
	if !ok {
		t.Fatalf("client is %T, want the anthropic-protocol client", client)
	}
	if cc.APIKey != "sk-zai-run" || cc.BaseURL != secrets.ZAIDefaultBaseURL {
		t.Errorf("client = {key set: %v, base %q}, want the run's z.ai key against %q", cc.APIKey == "sk-zai-run", cc.BaseURL, secrets.ZAIDefaultBaseURL)
	}
	if forfaitConsulted {
		t.Error("the Claude forfait path was consulted for a GLM model — api.anthropic.com cannot serve it")
	}
}

// The container's env factory makes the same choice as the in-process path:
// a sandboxed claw node receives the run's default keys beside the one its
// route is funded with, and a GLM id sent with the Anthropic key reached
// api.anthropic.com, which does not serve it. A claude id keeps its key.
func TestClawEnvFactoryServesGLMThroughZAI(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-fixture")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ZAI_API_KEY", "sk-zai-fixture")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")

	for _, tc := range []struct {
		spec, key, base string
	}{
		{"anthropic/glm-5.3", "sk-zai-fixture", secrets.ZAIDefaultBaseURL},
		{"anthropic/claude-opus-5-5", "sk-ant-fixture", "https://api.anthropic.com"},
	} {
		client, err := NewRegistry().Resolve(tc.spec)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", tc.spec, err)
		}
		cc, ok := client.(*api.Client)
		if !ok {
			t.Fatalf("%s: client is %T", tc.spec, client)
		}
		if cc.APIKey != tc.key || cc.BaseURL != tc.base {
			t.Errorf("%s: client = {key %q, base %q}, want {%q, %q}", tc.spec, cc.APIKey, cc.BaseURL, tc.key, tc.base)
		}
	}
}

// A GLM node spends the z.ai key, never the subscription: beside a Claude
// forfait (a forfait-first bundle, the z.ai key pinned for its route) the
// subscription guard must not refuse it, while it still refuses a claude node
// that has nothing but the forfait.
func TestClawSubscriptionGuardSparesAGLMNode(t *testing.T) {
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) {
		return &execMockClient{streams: []<-chan api.StreamEvent{mockStreamEvents("ok", "end_turn")}}, nil
	})
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{})
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): t.TempDir()},
		PinnedAPIKeys:        map[secrets.Provider]string{secrets.ProviderZAI: "sk-zai-pinned"},
	})

	if _, err := backend.Execute(ctx, delegate.Task{NodeID: "glm", Model: "anthropic/glm-5.3", UserPrompt: "hi"}); err != nil {
		t.Fatalf("GLM node beside a forfait: %v — it never spends the subscription", err)
	}
	_, err := backend.Execute(ctx, delegate.Task{NodeID: "claude", Model: "anthropic/claude-opus-5-5", UserPrompt: "hi"})
	if !errors.Is(err, secrets.ErrSubscriptionOAuthForbidden) {
		t.Fatalf("claude node on the forfait alone: %v, want the subscription refusal", err)
	}
}

// The dispatched task carries the hint its route was funded with: a GLM id
// with no hint of its own leaves claude_code (and pi) on zai, never on the
// default precedence that hands it to whatever holds the wire. An explicit
// hint is the author's, and stays.
func TestGLMTaskWithoutHintIsRoutedToZAI(t *testing.T) {
	head := &backendScriptedBackend{name: delegate.BackendClaudeCode}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, head)
	e := newFallbackExecutor(reg, EventHooks{})
	withZAI := secrets.WithCredentials(context.Background(), secrets.Credentials{
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "sk-zai-pinned"},
	})
	for _, tc := range []struct {
		name, model, hint, envKey, want string
		ctx                             context.Context
	}{
		{"bare glm, no hint", "glm-5.3", "", "", "zai", withZAI},
		{"glm with auto", "glm-5.3", "auto", "", "zai", withZAI},
		{"claude, no hint", "claude-opus-5-5", "", "", "", withZAI},
		{"glm with an explicit hint", "glm-5.3", "moonshot", "", "moonshot", withZAI},
		{"glm another provider serves", "openrouter/z-ai/glm-4.6", "", "", "", withZAI},
		{"glm, the process's ZAI_API_KEY", "glm-5.3", "", "sk-zai-env", "zai", context.Background()},
		// No z.ai key anywhere: the hint would refuse the node before it
		// spawns; the default path still inherits an ambient z.ai setup.
		{"glm, no z.ai key reachable", "glm-5.3", "", "", "", context.Background()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZAI_API_KEY", tc.envKey)
			build := e.newElementBuilder("review", delegate.BackendClaudeCode, head,
				func(_ context.Context, _ string) (*delegate.Task, error) {
					return &delegate.Task{NodeID: "review", Model: tc.model}, nil
				})
			_, _, task, err := build(tc.ctx, 0, chainElement{Provider: tc.hint}, "")
			if err != nil {
				t.Fatal(err)
			}
			if task.ProviderHint != tc.want {
				t.Errorf("ProviderHint = %q, want %q", task.ProviderHint, tc.want)
			}
		})
	}
}

// A GLM call goes where the operator's ANTHROPIC_BASE_URL points — a
// self-hosted z.ai-compatible endpoint, a regional facade, a proxy — the rule
// the claude_code delegate applies to every z.ai key
// (TestAnthropicCredEnv_ZAIHonoursTheOperatorsBaseURL), in the sandbox's env
// factory and in process alike; unset, z.ai's own endpoint.
func TestGLMHonoursTheOperatorsBaseURL(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ZAI_API_KEY", "sk-zai-fixture")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	SetCredentialsLookup(func(context.Context) (func(string) string, bool) {
		return func(p string) string {
			if p == string(secrets.ProviderZAI) {
				return "sk-zai-run"
			}
			return ""
		}, true
	})
	t.Cleanup(func() { SetCredentialsLookup(func(context.Context) (func(string) string, bool) { return nil, false }) })

	for _, tc := range []struct{ base, want string }{
		{"https://zai.internal.example/api/anthropic", "https://zai.internal.example/api/anthropic"},
		{"https://open.bigmodel.cn/api/anthropic", "https://open.bigmodel.cn/api/anthropic"},
		{"", secrets.ZAIDefaultBaseURL},
	} {
		t.Setenv("ANTHROPIC_BASE_URL", tc.base)
		for _, path := range []string{"env factory", "in process"} {
			var client api.APIClient
			var err error
			if path == "env factory" {
				client, err = NewRegistry().Resolve("anthropic/glm-5.3")
			} else {
				client, err = NewRegistry().ResolveWithContext(context.Background(), "anthropic/glm-5.3")
			}
			if err != nil {
				t.Fatalf("%s, base %q: %v", path, tc.base, err)
			}
			cc, ok := client.(*api.Client)
			if !ok {
				t.Fatalf("%s: client is %T", path, client)
			}
			if cc.BaseURL != tc.want {
				t.Errorf("%s, ANTHROPIC_BASE_URL %q: GLM sent to %q, want %q", path, tc.base, cc.BaseURL, tc.want)
			}
		}
	}
}

// The GLM → z.ai hint is set for the backends that route on a hint — claude_code
// and pi — and for no other: claw routes `anthropic/glm-*` to z.ai through its
// own provider, and a hint there changes nothing but the accounting.
func TestRouteProviderHintSendsGLMToZAIOnTheHintedBackends(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "")
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		PinnedAPIKeys: map[secrets.Provider]string{secrets.ProviderZAI: "sk-zai-pinned"},
	})
	for _, tc := range []struct{ backend, model, want string }{
		{delegate.BackendClaudeCode, "glm-5.3", "zai"},
		{delegate.BackendPi, "glm-5.3", "zai"},
		{delegate.BackendPi, "anthropic/glm-5.3", "zai"},
		{delegate.BackendClaw, "anthropic/glm-5.3", ""},
		{delegate.BackendCodex, "glm-5.3", ""},
		{delegate.BackendPi, "claude-opus-5-5", ""},
	} {
		if got := RouteProviderHint(ctx, tc.backend, "", tc.model); got != tc.want {
			t.Errorf("RouteProviderHint(%s, %s) = %q, want %q", tc.backend, tc.model, got, tc.want)
		}
	}
}
