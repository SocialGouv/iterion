package model

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// clientStringField reads an exported string field (BaseURL, APIKey, …) off
// the concrete client behind api.APIClient, so tests assert on the resolved
// request destination rather than on an error string (#1718's requirement).
func clientStringField(t *testing.T, c api.APIClient, name string) string {
	t.Helper()
	v := reflect.ValueOf(c)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	f := v.FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.String {
		t.Fatalf("client %T has no string field %s", c, name)
	}
	return f.String()
}

// isolateHintTest pins every global the credential resolution can read:
// the ambient anthropic-wire env, the desktop forfait dir, and the per-run
// credentials lookup.
func isolateHintTest(t *testing.T) {
	t.Helper()
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := credentialsLookupFn
	credentialsLookupFn = func(context.Context) (credentialsResolver, bool) { return nil, false }
	t.Cleanup(func() { credentialsLookupFn = orig })
}

// TestResolveWithContext_HintAnthropicSkipsZAI is the #1718 mutation test:
// `provider: "anthropic"` + ZAI_API_KEY present must NOT select z.ai. With
// no Anthropic-direct credential reachable the resolution refuses by type —
// the pre-fix code silently built a z.ai client here.
func TestResolveWithContext_HintAnthropicSkipsZAI(t *testing.T) {
	isolateHintTest(t)
	t.Setenv("ZAI_API_KEY", "zai-key-should-not-be-spent")

	r := NewRegistry()
	ctx := WithProviderHint(context.Background(), "anthropic")
	client, err := r.ResolveWithContext(ctx, "anthropic/claude-sonnet-4-6")
	var unfunded *ErrProviderHintUnfunded
	if !errors.As(err, &unfunded) {
		t.Fatalf("err = %v, want *ErrProviderHintUnfunded (z.ai must not serve a hint-anthropic node)", err)
	}
	if client != nil {
		t.Fatalf("client = %v, want nil beside the refusal", client)
	}
}

// TestResolveWithContext_HintAnthropicUsesDirectKey: with an Anthropic key
// reachable, the hint path spends THAT — on api.anthropic.com — even when a
// z.ai key AND a z.ai base URL are set (the exact ambient trap of #1718).
func TestResolveWithContext_HintAnthropicUsesDirectKey(t *testing.T) {
	isolateHintTest(t)
	t.Setenv("ZAI_API_KEY", "zai-key")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-direct")

	r := NewRegistry()
	ctx := WithProviderHint(context.Background(), "anthropic")
	client, err := r.ResolveWithContext(ctx, "anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("ResolveWithContext: %v", err)
	}
	if got := clientStringField(t, client, "BaseURL"); got != "https://api.anthropic.com" {
		t.Fatalf("BaseURL = %q, want Anthropic-direct", got)
	}
	if got := clientStringField(t, client, "APIKey"); got != "sk-ant-direct" {
		t.Fatalf("APIKey = %q, want the Anthropic key, not the z.ai one", got)
	}
}

// TestResolveWithContext_HintAnthropicRunKeyWins: the run's own credential
// outranks the ambient z.ai key, on the direct endpoint.
func TestResolveWithContext_HintAnthropicRunKeyWins(t *testing.T) {
	isolateHintTest(t)
	t.Setenv("ZAI_API_KEY", "zai-key")
	credentialsLookupFn = func(context.Context) (credentialsResolver, bool) {
		return func(provider string) string {
			if provider == string(secrets.ProviderAnthropic) {
				return "run-key"
			}
			return ""
		}, true
	}

	r := NewRegistry()
	ctx := WithProviderHint(context.Background(), "anthropic")
	client, err := r.ResolveWithContext(ctx, "anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("ResolveWithContext: %v", err)
	}
	if got := clientStringField(t, client, "APIKey"); got != "run-key" {
		t.Fatalf("APIKey = %q, want the run's key", got)
	}
	if got := clientStringField(t, client, "BaseURL"); got != "https://api.anthropic.com" {
		t.Fatalf("BaseURL = %q, want Anthropic-direct", got)
	}
}

// TestResolveWithContext_NoHintKeepsZAIFallback is the control: with NO hint
// the ZAI_API_KEY env-fallback still synthesises the z.ai route — the fix
// narrows the hint path, it must not move the default one.
func TestResolveWithContext_NoHintKeepsZAIFallback(t *testing.T) {
	isolateHintTest(t)
	t.Setenv("ZAI_API_KEY", "zai-key")

	r := NewRegistry()
	client, err := r.ResolveWithContext(context.Background(), "anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("ResolveWithContext: %v", err)
	}
	if got := clientStringField(t, client, "BaseURL"); got != secrets.ZAIDefaultBaseURL {
		t.Fatalf("BaseURL = %q, want the z.ai fallback %q", got, secrets.ZAIDefaultBaseURL)
	}
	if got := clientStringField(t, client, "APIKey"); got != "zai-key" {
		t.Fatalf("APIKey = %q, want the z.ai key", got)
	}
}

// TestResolveWithContext_HintDoesNotReadTheCache: the registry cache is keyed
// by model spec alone. A hint-bearing resolve must not be served the z.ai
// client a previous hint-less resolve of the SAME spec cached.
func TestResolveWithContext_HintDoesNotReadTheCache(t *testing.T) {
	isolateHintTest(t)
	t.Setenv("ZAI_API_KEY", "zai-key")

	r := NewRegistry()
	// Prime the cache with the hint-less (z.ai) resolution.
	if _, err := r.ResolveWithContext(context.Background(), "anthropic/claude-sonnet-4-6"); err != nil {
		t.Fatalf("priming resolve: %v", err)
	}
	ctx := WithProviderHint(context.Background(), "anthropic")
	if _, err := r.ResolveWithContext(ctx, "anthropic/claude-sonnet-4-6"); err == nil {
		t.Fatal("hint-anthropic resolve was served the cached z.ai client — the hint must bypass the cache")
	}
}

// TestWithProviderHint_Folding: the hint is operator text; case and padding
// fold before the switch reads it.
func TestWithProviderHint_Folding(t *testing.T) {
	ctx := WithProviderHint(context.Background(), "  Anthropic ")
	if got := providerHintFromContext(ctx); got != "anthropic" {
		t.Fatalf("providerHintFromContext = %q, want folded %q", got, "anthropic")
	}
	if got := providerHintFromContext(context.Background()); got != "" {
		t.Fatalf("unstamped ctx hint = %q, want empty", got)
	}
}

// TestResolveWithContext_HintAnthropicGLMKeepsZAI is the round-3 HIGH
// mutation test: `anthropic/glm-*` is served by z.ai BY DEFINITION — the
// hint has nothing to narrow there. hint + glm + ZAI alone must resolve the
// z.ai client, not refuse a route that IS funded.
func TestResolveWithContext_HintAnthropicGLMKeepsZAI(t *testing.T) {
	isolateHintTest(t)
	t.Setenv("ZAI_API_KEY", "zai-key")

	r := NewRegistry()
	ctx := WithProviderHint(context.Background(), "anthropic")
	client, err := r.ResolveWithContext(ctx, "anthropic/glm-5.3")
	if err != nil {
		t.Fatalf("hint + glm + ZAI must resolve, got %v (the route IS funded — by z.ai, by definition)", err)
	}
	if got := clientStringField(t, client, "BaseURL"); got != secrets.ZAIDefaultBaseURL {
		t.Fatalf("BaseURL = %q, want z.ai — a GLM id is served there whatever the hint", got)
	}
	if got := clientStringField(t, client, "APIKey"); got != "zai-key" {
		t.Fatalf("APIKey = %q, want the z.ai key", got)
	}
}

// TestResolveWithContext_HintAnthropicGLMWithAnthropicKey: hint + glm with
// BOTH keys set must still reach z.ai — api.anthropic.com does not serve
// GLM, and the hint must not route it there.
func TestResolveWithContext_HintAnthropicGLMWithAnthropicKey(t *testing.T) {
	isolateHintTest(t)
	t.Setenv("ZAI_API_KEY", "zai-key")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-direct")

	r := NewRegistry()
	ctx := WithProviderHint(context.Background(), "anthropic")
	client, err := r.ResolveWithContext(ctx, "anthropic/glm-5.3")
	if err != nil {
		t.Fatalf("ResolveWithContext: %v", err)
	}
	if got := clientStringField(t, client, "BaseURL"); got != secrets.ZAIDefaultBaseURL {
		t.Fatalf("BaseURL = %q, want z.ai — GLM must not be routed to api.anthropic.com, which does not serve it", got)
	}
	if got := clientStringField(t, client, "APIKey"); got != "zai-key" {
		t.Fatalf("APIKey = %q, want the z.ai key", got)
	}
	// Control: the non-GLM hint path is untouched — a claude model under the
	// same env still goes Anthropic-direct (#1718).
	client2, err := r.ResolveWithContext(ctx, "anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("claude model under hint: %v", err)
	}
	if got := clientStringField(t, client2, "BaseURL"); got != "https://api.anthropic.com" {
		t.Fatalf("BaseURL = %q, want Anthropic-direct for the non-GLM model", got)
	}
}

// TestResolveWithContext_HintAnthropicGLMNoKeyRefuses is the round-4 MEDIUM
// mutation test: hint + glm + NO key anywhere must refuse by type. Without
// the refusal the GLM exemption falls through to the env factory, which
// builds an unauthenticated client (#687's opaque-401 shape) that says
// nothing about z.ai being the missing credential.
func TestResolveWithContext_HintAnthropicGLMNoKeyRefuses(t *testing.T) {
	isolateHintTest(t)

	r := NewRegistry()
	ctx := WithProviderHint(context.Background(), "anthropic")
	client, err := r.ResolveWithContext(ctx, "anthropic/glm-5.3")
	var unfunded *ErrGLMHintUnfunded
	if !errors.As(err, &unfunded) {
		t.Fatalf("err = %v, want *ErrGLMHintUnfunded (no silent unauthenticated client)", err)
	}
	if client != nil {
		t.Fatalf("client = %v, want nil beside the refusal", client)
	}
	if !strings.Contains(unfunded.Error(), "ZAI_API_KEY") {
		t.Fatalf("the refusal must name the missing credential, got: %q", unfunded.Error())
	}
}

// TestResolveWithContext_HintAnthropicGLMGatewayExempt: an explicit
// anthropic-wire gateway (ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN) is the
// operator's chosen destination — the hint never refuses it (philosophy:
// an explicit choice is honoured).
func TestResolveWithContext_HintAnthropicGLMGatewayExempt(t *testing.T) {
	isolateHintTest(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://glm.gateway.example.com/anthropic")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "gateway-token")

	r := NewRegistry()
	ctx := WithProviderHint(context.Background(), "anthropic")
	client, err := r.ResolveWithContext(ctx, "anthropic/glm-5.3")
	if err != nil {
		t.Fatalf("an explicit gateway must not be refused: %v", err)
	}
	if got := clientStringField(t, client, "BaseURL"); got != "https://glm.gateway.example.com/anthropic" {
		t.Fatalf("BaseURL = %q, want the operator's gateway", got)
	}
}

// TestResolveWithContext_HintAnthropicGLMRunKeyExempt: the run's own z.ai
// key funds the route under the hint — no refusal, run key spent.
func TestResolveWithContext_HintAnthropicGLMRunKeyExempt(t *testing.T) {
	isolateHintTest(t)
	credentialsLookupFn = func(context.Context) (credentialsResolver, bool) {
		return func(provider string) string {
			if provider == string(secrets.ProviderZAI) {
				return "run-zai-key"
			}
			return ""
		}, true
	}

	r := NewRegistry()
	ctx := WithProviderHint(context.Background(), "anthropic")
	client, err := r.ResolveWithContext(ctx, "anthropic/glm-5.3")
	if err != nil {
		t.Fatalf("the run's z.ai key funds the route: %v", err)
	}
	if got := clientStringField(t, client, "APIKey"); got != "run-zai-key" {
		t.Fatalf("APIKey = %q, want the run's z.ai key", got)
	}
}

// TestModelServedByZAI_PrefixAnchored: the predicate is anchored on the
// "glm" prefix — substring matches were false positives routing ids no
// z.ai endpoint serves.
func TestModelServedByZAI_PrefixAnchored(t *testing.T) {
	for id, want := range map[string]bool{
		"glm-5.3":                 true,
		"GLM-4.6":                 true,
		"glm-4.6v":                true,
		"notglm":                  false,
		"claude-glm-experimental": false,
		"claude-sonnet-4-6":       false,
	} {
		if got := modelServedByZAI(id); got != want {
			t.Errorf("modelServedByZAI(%q) = %v, want %v", id, got, want)
		}
	}
	// Spec level: the prefix anchor holds through GLMOnAnthropicWire.
	if GLMOnAnthropicWire("anthropic/notglm") {
		t.Error("GLMOnAnthropicWire(anthropic/notglm) = true, want false")
	}
	if !GLMOnAnthropicWire("anthropic/glm-5.3") {
		t.Error("GLMOnAnthropicWire(anthropic/glm-5.3) = false, want true")
	}
}
