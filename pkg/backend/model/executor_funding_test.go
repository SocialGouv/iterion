package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func clearAnthropicFunding(t *testing.T) {
	t.Helper()
	for _, n := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ZAI_API_KEY", "ITERION_DEFAULT_SUPERVISOR_MODEL"} {
		t.Setenv(n, "")
	}
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	// An unset backend otherwise resolves by probing THIS machine's
	// credentials (claude_code wherever the CLI is logged in, claw on a bare
	// runner) — the witnesses pin claw so the claw-unfunded refusal is the
	// same case on every machine.
	t.Setenv("ITERION_DEFAULT_BACKEND", string(delegate.BackendClaw))
}

// An LLM router with NO model falls through to the anthropic default; when
// nothing funds that wire the router is refused UP FRONT, naming the knob —
// not dispatched into a 401 loop. Funded, it proceeds past the guard. Red
// when the guard is deleted.
func TestLLMRouter_AnUnfundedDefaultIsRefusedUpFront(t *testing.T) {
	clearAnthropicFunding(t)
	reg := NewRegistry()
	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	backendReg := delegate.NewRegistry()
	backendReg.Register(delegate.BackendClaw, NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{MaxAttempts: 1}))
	exec := NewClawExecutor(reg, wf, WithBackendRegistry(backendReg))

	router := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "rt"}, RouterMode: ir.RouterLLM}
	_, err := exec.executeLLMRouterUnified(context.Background(), router, map[string]any{
		"_route_candidates": []string{"alpha", "beta"},
	})
	if err == nil || !strings.Contains(err.Error(), defaultRouterModel) || !strings.Contains(err.Error(), "fund the anthropic wire") {
		t.Fatalf("err = %v, want the up-front refusal naming the default and the remedy", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	if _, err := exec.executeLLMRouterUnified(context.Background(), router, map[string]any{
		"_route_candidates": []string{"alpha", "beta"},
	}); err != nil && strings.Contains(err.Error(), "fund the anthropic wire") {
		t.Fatalf("a funded wire must pass the guard, got: %v — the funded half never reached the guard", err)
	}
}

// The recovery rungs refuse an unfunded DEFAULT before dispatching; the
// ladder already treats a rung error as abort-and-log, so the refusal
// surfaces as the named reason instead of a doomed call. Red when either
// guard is deleted.
func TestVerifiedAction_AnUnfundedDefaultRecoveryIsRefused(t *testing.T) {
	clearAnthropicFunding(t)
	exec := NewClawExecutor(NewRegistry(), &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}})
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "v"}, Goal: "g", Recovery: &ir.RecoverySpec{MaxAgentAttempts: 1}}

	if _, _, err := exec.selfRepair(context.Background(), node, "out", "err", "cmd"); err == nil || !strings.Contains(err.Error(), "ITERION_VERIFIED_ACTION_MODEL") {
		t.Fatalf("selfRepair err = %v, want the default-model refusal", err)
	}
	if err := exec.agentRecovery(context.Background(), node, map[string]any{}, "recipe"); err == nil || !strings.Contains(err.Error(), "ITERION_VERIFIED_ACTION_MODEL") {
		t.Fatalf("agentRecovery err = %v, want the default-model refusal", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	if err := exec.agentRecovery(context.Background(), node, map[string]any{}, "recipe"); err != nil && strings.Contains(err.Error(), "ITERION_VERIFIED_ACTION_MODEL") {
		t.Fatalf("a funded wire must pass the recovery guard, got: %v", err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) {
		return newMockClient(textEvents("fixed", 10, 5)), nil
	})
	execFunded := NewClawExecutor(reg, &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}})
	if _, _, err := execFunded.selfRepair(context.Background(), node, "out", "err", "cmd"); err != nil && strings.Contains(err.Error(), "ITERION_VERIFIED_ACTION_MODEL") {
		t.Fatalf("a funded wire must pass the guard, got: %v", err)
	}
}

// A claude_code-serving deployment funds the recovery agent on the CLI's
// own login even with the subscription switch ON — the CLI is the vendor's
// own binary, which is exactly what FORBID protects. The agent rung must
// NOT be refused for funding there, or a funded deployment loses the rung.
// Red when the agentRecovery guard loses its backend scoping.
func TestVerifiedAction_RecoveryDefaultOnClaudeCodeIsNotRefused(t *testing.T) {
	clearAnthropicFunding(t)
	t.Setenv("ITERION_DEFAULT_BACKEND", string(delegate.BackendClaudeCode))
	exec := NewClawExecutor(NewRegistry(), &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}})
	node := &ir.ToolNode{BaseNode: ir.BaseNode{ID: "v"}, Goal: "g", Recovery: &ir.RecoverySpec{MaxAgentAttempts: 1}}
	if err := exec.agentRecovery(context.Background(), node, map[string]any{}, "recipe"); err != nil && strings.Contains(err.Error(), "ITERION_VERIFIED_ACTION_MODEL") {
		t.Fatalf("a claude_code-serving deployment funds the rung on the CLI's own login — got the funding refusal: %v", err)
	}
}

// The probe reads the inputs the factory reads — presence in ANY of them
// funds the wire; a ZAI key does NOT fund the non-GLM default. Red when the
// probe drops a source or invents one.
func TestAnthropicFunding_ReadsWhatTheFactoryReads(t *testing.T) {
	clearAnthropicFunding(t)
	if got := anthropicFunding(context.Background()); got != "" {
		t.Errorf("an unfunded host reads %q, want empty", got)
	}
	t.Setenv("ANTHROPIC_API_KEY", "k")
	if got := anthropicFunding(context.Background()); got == "" {
		t.Error("an env key must fund the wire")
	}
	clearAnthropicFunding(t)
	t.Setenv("ZAI_API_KEY", "k")
	if got := anthropicFunding(context.Background()); got != "" {
		t.Errorf("a ZAI key read %q — the non-GLM default is not z.ai's to serve", got)
	}
	// The disk forfait funds only when the operator's kill switch is off —
	// a fixture Claude login in a temp CLAUDE_CONFIG_DIR, both ways.
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	credsJSON := `{"claudeAiOauth":{"accessToken":"disk-token","expiresAt":` + fmt.Sprintf("%d", time.Now().Add(time.Hour).UnixMilli()) + `}}`
	if werr := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(credsJSON), 0o600); werr != nil {
		t.Fatal(werr)
	}
	clearAnthropicFunding(t)
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "") // the switch OFF for this row
	if got := anthropicFunding(context.Background()); got != "this host's Claude forfait" {
		t.Errorf("a disk forfait with the switch off reads %q, want it funding", got)
	}
	t.Setenv("ITERION_FORBID_SUBSCRIPTION_OAUTH", "1")
	if got := anthropicFunding(context.Background()); got != "" {
		t.Errorf("a disk forfait with FORBID=1 reads %q — a switch-off forfait funds nothing", got)
	}
	// The run's own key funds regardless of the environment.
	clearAnthropicFunding(t)
	creds := secrets.Credentials{APIKeys: map[secrets.Provider]string{secrets.ProviderAnthropic: "run-key"}}
	ctx := secrets.WithCredentials(context.Background(), creds)
	if got := anthropicFunding(ctx); got == "" {
		t.Error("the run's anthropic key must fund the wire")
	}
	// The run's ctx forfait funds only with a NON-blank token: the resolver
	// (anthropicFromCtxForfaitOnWire) falls through to the env factory on
	// ("", nil), so a dir whose blob merely parses must NOT bless the wire —
	// otherwise the guard passes and the unauthenticated-client 401 loop
	// returns through the side door.
	forfaitJSON := func(tok string) []byte {
		return []byte(`{"claudeAiOauth":{"accessToken":"` + tok + `","expiresAt":` + fmt.Sprintf("%d", time.Now().Add(time.Hour).UnixMilli()) + `}}`)
	}
	forfaitDir := t.TempDir()
	if werr := os.WriteFile(filepath.Join(forfaitDir, ".credentials.json"), forfaitJSON("   "), 0o600); werr != nil {
		t.Fatal(werr)
	}
	forfaitCreds := secrets.Credentials{OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): forfaitDir}}
	clearAnthropicFunding(t)
	if got := anthropicFunding(secrets.WithCredentials(context.Background(), forfaitCreds)); got != "" {
		t.Errorf("a blank-token ctx forfait read %q — the resolver would not serve it", got)
	}
	if werr := os.WriteFile(filepath.Join(forfaitDir, ".credentials.json"), forfaitJSON("run-tok"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if got := anthropicFunding(secrets.WithCredentials(context.Background(), forfaitCreds)); got != "the run's Claude forfait" {
		t.Errorf("a real ctx forfait token read %q, want it funding", got)
	}
}
