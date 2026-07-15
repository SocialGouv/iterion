package delegate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

func TestCodexCredEnvForCLI_APIKeyWinsOverOAuth(t *testing.T) {
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		APIKeys: map[secrets.Provider]string{
			secrets.ProviderOpenAI: "sk-run",
		},
		OAuthCredentialFiles: map[string]string{
			string(secrets.OAuthKindCodex): "/tmp/codex-oauth",
		},
	})

	got := codexCredEnvForCLI(ctx)
	if got["OPENAI_API_KEY"] != "sk-run" {
		t.Fatalf("OPENAI_API_KEY = %q, want per-run key", got["OPENAI_API_KEY"])
	}
	if _, ok := got["CODEX_HOME"]; ok {
		t.Fatal("CODEX_HOME must not be set when the per-run API key wins")
	}
	if value, ok := got["CODEX_API_KEY"]; !ok || value != "" {
		t.Fatalf("CODEX_API_KEY must be explicitly cleared, present=%v value=%q", ok, value)
	}
}

func TestCodexCredEnvForCLI_OAuthSuppressesInheritedKeys(t *testing.T) {
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{
			string(secrets.OAuthKindCodex): "/tmp/codex-oauth",
		},
	})

	got := codexCredEnvForCLI(ctx)
	if got["CODEX_HOME"] != "/tmp/codex-oauth" {
		t.Fatalf("CODEX_HOME = %q, want /tmp/codex-oauth", got["CODEX_HOME"])
	}
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY"} {
		if value, ok := got[key]; !ok || value != "" {
			t.Errorf("%s must be explicitly cleared, present=%v value=%q", key, ok, value)
		}
	}
}

func TestCodexCredEnvForCLI_NoPerRunScope(t *testing.T) {
	if got := codexCredEnvForCLI(context.Background()); got != nil {
		t.Fatalf("got %v, want nil without per-run credentials", got)
	}
}

func TestInspectCodexRolloutUsesPerRunCodexHome(t *testing.T) {
	codexHome := t.TempDir()
	threadID := "thread-test"
	dir := filepath.Join(codexHome, "sessions", "2026", "07", "14")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(dir, "rollout-test-"+threadID+".jsonl")
	line := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":42},"model_context_window":1000}}}` + "\n"
	if err := os.WriteFile(rollout, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
		OAuthCredentialFiles: map[string]string{
			string(secrets.OAuthKindCodex): codexHome,
		},
	})

	got := inspectCodexRollout(ctx, threadID)
	if got == "" {
		t.Fatal("rollout diagnostic was not found under per-run CODEX_HOME")
	}
}
