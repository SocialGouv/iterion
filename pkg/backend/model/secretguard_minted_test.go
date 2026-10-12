package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// blankHostCredentials keeps the host's own credentials out of the guard a
// test builds: BuildSecretGuard registers every secret-named env var.
func blankHostCredentials(t *testing.T) {
	t.Helper()
	for _, raw := range os.Environ() {
		if name, _, _ := strings.Cut(raw, "="); store.IsSecretEnvName(name) {
			t.Setenv(name, "")
		}
	}
}

func randomToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// A credential the server minted for the run rides the launch vars; the guard
// scrubs it from every sink and hands it back nowhere: its placeholder is
// deterministic, so resolving it would give the value to whoever writes it.
func TestBuildSecretGuard_TheMintedGrantIsRedactOnly(t *testing.T) {
	blankHostCredentials(t)
	tok := randomToken(t)
	g := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: tok}, nil)
	ph := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	if red := g.Redact("X-Iterion-Publish: " + tok); strings.Contains(red, tok) || !strings.Contains(red, ph) {
		t.Fatalf("Redact left the grant or named no placeholder: %q", red)
	}
	if got := g.Materialize("printf %s " + ph); strings.Contains(got, tok) {
		t.Error("Materialize resolved the grant's placeholder")
	}
	if got, env := g.MaterializeShellEnv("PUB_TOKEN='" + ph + "' publish"); strings.Contains(got, tok) || len(env) != 0 {
		t.Error("MaterializeShellEnv resolved the grant's placeholder")
	}
	if got := g.MaterializeShell("pub_token = '" + ph + "'"); strings.Contains(got, tok) {
		t.Error("MaterializeShell resolved the grant's placeholder")
	}
	if got := g.MaterializeForHost("Authorization: "+ph, "attacker.example"); strings.Contains(got, tok) {
		t.Error("MaterializeForHost resolved the grant's placeholder")
	}
	if (&ClawExecutor{secretGuard: g}).secretMaterializer() != nil {
		t.Error("a guard with nothing materialisable hands agents a materializer")
	}

	masked := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: store.RedactedLaunchVar}, nil)
	if masked != nil && masked.ContainsSecret(store.RedactedLaunchVar) {
		t.Error("the read surfaces' mask was registered as a secret value")
	}
}

// A lineage's records may hold several grants — a fork's source, an
// ancestor's, the run's own: the guard redacts every one, and resolves none.
func TestBuildSecretGuard_EveryRecordedGrantIsRedacted(t *testing.T) {
	blankHostCredentials(t)
	launch, own, ancestor := randomToken(t), randomToken(t), randomToken(t)
	g := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: launch}, map[string][]string{store.ForgePublishTokenVar: {own, ancestor}})
	red := g.Redact(strings.Join([]string{launch, own, ancestor}, " "))
	for name, tok := range map[string]string{"launch": launch, "own record": own, "ancestor": ancestor} {
		if strings.Contains(red, tok) {
			t.Errorf("the %s grant is not redacted: %q", name, red)
		}
	}
	if got := g.Materialize(secretguard.PlaceholderForName(store.ForgePublishTokenVar)); strings.Contains(got, own) || strings.Contains(got, ancestor) {
		t.Error("Materialize resolved a recorded grant's placeholder")
	}
}

// An agent that writes a placeholder gets the value only for a secret its
// workflow declares for it. The runner's own env, the run's provider keys and
// the minted grant stay placeholders in whatever the agent runs.
func TestAnAgentMaterializesOnlyTheSecretsItsWorkflowDeclares(t *testing.T) {
	blankHostCredentials(t)
	ambient := "Ambient-" + randomToken(t)
	provider := "sk-ant-" + randomToken(t)
	grant := randomToken(t)
	declared := "Declared-" + randomToken(t)
	t.Setenv("ITERION_SECRETS_KEY", ambient)
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{secrets.Provider("anthropic"): provider},
	})
	wf := &ir.Workflow{Secrets: map[string]*ir.Secret{"deploy_key": {Value: declared}}}
	guard := BuildSecretGuard(ctx, wf, map[string]string{store.ForgePublishTokenVar: grant}, nil)

	dir := t.TempDir()
	tr := tool.NewRegistry()
	if err := tool.RegisterClawAll(tr, tool.ClawDefaults{Workspace: dir}); err != nil {
		t.Fatal(err)
	}
	cmd := "printf '%s\\n' __ITERION_SECRET_env_ITERION_SECRETS_KEY__ __ITERION_SECRET_provider_key_0__ " +
		secretguard.PlaceholderForName(store.ForgePublishTokenVar) + " " + secretguard.PlaceholderForName("deploy_key") + " > exfil.txt"
	in, _ := json.Marshal(map[string]string{"command": cmd})
	mock := newMockClient(toolUseEvents("t1", "bash", string(in), 10, 10), textEvents("done", 10, 10))
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	exec := newTestClawExecutor(reg, wf, WithSecretGuard(guard), WithToolRegistry(tr), WithWorkDir(dir))
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "reviewer"}, LLMFields: ir.LLMFields{Model: "anthropic/claude-sonnet-4-6"}, Tools: []string{"bash"}}
	if _, err := exec.Execute(ctx, node, map[string]any{"diff": "untrusted pull request content"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "exfil.txt"))
	if err != nil {
		t.Fatalf("the agent's command did not run: %v", err)
	}
	got := string(raw)
	for what, value := range map[string]string{"the runner's ambient env": ambient, "the run's provider key": provider, "the minted grant": grant} {
		if strings.Contains(got, value) {
			t.Errorf("%s was materialised into the agent's command", what)
		}
	}
	if !strings.Contains(got, declared) {
		t.Error("the declared secret's placeholder no longer resolves for the agent")
	}
}

// The self-repair model sees the failed command, its stdout and its stderr —
// never the minted token in them, not even a prefix cut at the truncation
// bound. The command it proposes from the redacted text runs with the
// placeholder unresolved.
func TestSelfRepairNeverShowsTheModelAMintedToken(t *testing.T) {
	blankHostCredentials(t)
	// The self-repair rung refuses an unfunded default before dispatching;
	// this test's registry is a mock, so fund the wire nominally.
	t.Setenv("ANTHROPIC_API_KEY", "mock-funding")
	tok := randomToken(t)
	dir := t.TempDir()
	placeholder := secretguard.PlaceholderForName(store.ForgePublishTokenVar)
	corrected := fmt.Sprintf("printf '%%s' '%s' > repaired_marker", placeholder)
	reg := NewRegistry()
	mock := newMockClient(toolUseEvents("t1", "structured_output",
		fmt.Sprintf(`{"corrected_command":%q}`, corrected), 50, 10))
	reg.Register("anthropic", func(string) (api.APIClient, error) { return mock, nil })
	guard := BuildSecretGuard(t.Context(), &ir.Workflow{}, map[string]string{store.ForgePublishTokenVar: tok}, nil)
	exec := newTestClawExecutor(reg, &ir.Workflow{}, WithWorkDir(dir), WithSecretGuard(guard))

	node := &ir.ToolNode{
		BaseNode: ir.BaseNode{ID: "publish"},
		// stdout carries the token across the prompt's 4000-byte bound.
		Command:       "printf '%s' '" + strings.Repeat("x", 3970) + tok + "'; echo refused " + tok + " >&2",
		Goal:          "publish the review",
		Postcondition: "test -f repaired_marker",
		Policy:        ir.PolicyRecover,
		Recovery:      &ir.RecoverySpec{MaxRepairAttempts: 1},
	}
	if _, err := exec.Execute(context.Background(), node, map[string]any{}); err != nil {
		t.Fatalf("Execute: %v — the corrected command did not run", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "repaired_marker"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), tok) || strings.TrimSpace(string(raw)) != placeholder {
		t.Errorf("repaired_marker = %q, want the unresolved placeholder", raw)
	}
	calls := mock.getCalls()
	if len(calls) == 0 {
		t.Fatal("the self-repair model was never asked")
	}
	sawPlaceholder := false
	for _, call := range calls {
		for _, msg := range call.Messages {
			for _, block := range msg.Content {
				if strings.Contains(block.Text, tok[:24]) {
					t.Fatalf("the self-repair prompt carries the minted token (or a prefix of it): %q", block.Text)
				}
				sawPlaceholder = sawPlaceholder || strings.Contains(block.Text, placeholder)
			}
		}
	}
	if !sawPlaceholder {
		t.Error("the self-repair prompt does not show the placeholder in the token's place")
	}
}
