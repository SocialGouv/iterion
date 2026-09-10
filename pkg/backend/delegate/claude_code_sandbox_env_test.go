package delegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// captureSandboxCmdRun fakes a real (kubernetes-shaped) sandbox Run and
// records every exec's argv / WorkDir / env so tests can assert exactly
// what a sandboxed CLI spawn receives.
type captureSandboxCmdRun struct {
	sandbox.Run
	argvs [][]string
	cwds  []string
	envs  []map[string]string
}

func (c *captureSandboxCmdRun) Driver() string { return "kubernetes" }

func (c *captureSandboxCmdRun) Command(ctx context.Context, cmd []string, opts sandbox.ExecOpts) *exec.Cmd {
	c.argvs = append(c.argvs, append([]string(nil), cmd...))
	c.cwds = append(c.cwds, opts.WorkDir)
	env := map[string]string{}
	for k, v := range opts.Env {
		env[k] = v
	}
	c.envs = append(c.envs, env)
	// A no-output command: claudesdk.Prompt reads EOF and returns a
	// ProcessError — the capture has happened by then.
	return exec.CommandContext(ctx, "true")
}

// TestAnthropicCredEnv_SandboxForwardsAmbientCreds pins the fix for the
// first sandboxed cloud run (019f8a6c): prod delivers the Claude forfait
// as CLAUDE_CODE_OAUTH_TOKEN in the RUNNER pod env (no sealed claude
// OAuth in the bundle). A host spawn inherits os.Environ() so that
// always authenticated; a sandboxed spawn gets only the SDK env map, so
// every claude exec ran env_keys=1 and died "Not logged in". Under
// sandbox the ambient Anthropic env must be forwarded verbatim; on the
// host the nil return (inheritance) stays.
func TestAnthropicCredEnv_SandboxForwardsAmbientCreds(t *testing.T) {
	t.Run("ambient forfait token", func(t *testing.T) {
		resetClaudeCredEnv(t)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-RUNNERPOD")

		got := anthropicCredEnvForCLI(context.Background(), "", true)
		if got["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-RUNNERPOD" {
			t.Fatalf("sandboxed spawn must carry the ambient forfait token, got %v", got)
		}
		if host := anthropicCredEnvForCLI(context.Background(), "", false); host != nil {
			t.Fatalf("host spawn keeps env inheritance (nil), got %v", host)
		}
	})

	t.Run("ambient API key", func(t *testing.T) {
		resetClaudeCredEnv(t)
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-AMBIENT")

		got := anthropicCredEnvForCLI(context.Background(), "", true)
		if got["ANTHROPIC_API_KEY"] != "sk-ant-api-AMBIENT" {
			t.Fatalf("sandboxed spawn must carry the ambient API key, got %v", got)
		}
	})

	t.Run("hint anthropic still forwards ambient under sandbox", func(t *testing.T) {
		resetClaudeCredEnv(t)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-RUNNERPOD")
		t.Setenv("ANTHROPIC_BASE_URL", "https://api.z.ai/api/anthropic") // stale z.ai — must stay suppressed

		got := anthropicCredEnvForCLI(context.Background(), "anthropic", true)
		if got["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat-RUNNERPOD" {
			t.Fatalf("hint=anthropic sandboxed must forward the ambient token, got %v", got)
		}
		if v, present := got["ANTHROPIC_BASE_URL"]; !present || v != "" {
			t.Fatalf("z.ai suppression must survive the forwarding: %v", got)
		}
	})

	t.Run("nothing ambient stays nil", func(t *testing.T) {
		resetClaudeCredEnv(t)
		if got := anthropicCredEnvForCLI(context.Background(), "", true); got != nil {
			t.Fatalf("no ambient creds → nil (loud downstream no-credential error), got %v", got)
		}
	})
}

// TestFormatOutput_SandboxExecParity pins Pass-2 parity with the main
// pass: the formatting exec must route through the sandbox Run and carry
// the SAME credential env the main pass computes (setupCredsAndSession
// and formatOutput share anthropicCredEnvForCLI), with cwd left empty so
// the driver applies the container workingDir — the workspace — exactly
// like the main pass (the claude session key derives from it, so the
// resumed session is found).
func TestFormatOutput_SandboxExecParity(t *testing.T) {
	assertParity := func(t *testing.T, ctx context.Context, wantEnvKeys []string) {
		t.Helper()
		fake := &captureSandboxCmdRun{}
		task := Task{
			NodeID:       "campaign",
			WorkDir:      "/host/worktree",
			Sandbox:      fake,
			OutputSchema: []byte(`{"type":"object","properties":{"docs_aligned":{"type":"boolean"}}}`),
		}
		b := &ClaudeCodeBackend{Logger: iterlog.Nop()}
		_, _ = b.formatOutput(ctx, task, "sess-643e6598")

		if len(fake.argvs) != 1 {
			t.Fatalf("expected exactly one sandbox exec, got %d", len(fake.argvs))
		}
		argv := strings.Join(fake.argvs[0], " ")
		if !strings.Contains(argv, "--resume sess-643e6598") {
			t.Errorf("fmt exec must resume the Pass-1 session, argv: %s", argv)
		}
		if fake.cwds[0] != "" {
			t.Errorf("fmt exec cwd = %q — must stay empty so the driver applies the container workingDir (same session key as the main pass)", fake.cwds[0])
		}
		// The exec env must match what the MAIN pass computes from the same
		// inputs, plus the effort dial.
		mainCredEnv := anthropicCredEnvForCLI(ctx, task.ProviderHint, taskSandboxed(task))
		for k, v := range mainCredEnv {
			if fake.envs[0][k] != v {
				t.Errorf("fmt env[%s] = %q, want the main pass value %q", k, fake.envs[0][k], v)
			}
		}
		if _, ok := fake.envs[0]["CLAUDE_CODE_EFFORT_LEVEL"]; !ok {
			t.Error("fmt env missing CLAUDE_CODE_EFFORT_LEVEL")
		}
		for _, k := range wantEnvKeys {
			if fake.envs[0][k] == "" {
				t.Errorf("fmt env missing %s (keys: %v)", k, envKeys(fake.envs[0]))
			}
		}
	}

	t.Run("ambient runner-pod forfait (the live prod shape)", func(t *testing.T) {
		resetClaudeCredEnv(t)
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat-RUNNERPOD")
		assertParity(t, context.Background(), []string{"CLAUDE_CODE_OAUTH_TOKEN"})
	})

	t.Run("materialised ctx forfait (sealed-bundle shape)", func(t *testing.T) {
		resetClaudeCredEnv(t)
		dir := t.TempDir()
		blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat-BUNDLE","refreshToken":"r","expiresAt":4102444800000}}`
		if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(blob), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx := secrets.WithCredentials(context.Background(), secrets.Credentials{
			OAuthCredentialFiles: map[string]string{string(secrets.OAuthKindClaudeCode): dir},
		})
		assertParity(t, ctx, []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"})
	})
}

func envKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
