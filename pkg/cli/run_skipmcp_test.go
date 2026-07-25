package cli

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

type authorityHealthExecutor struct {
	healthAt time.Time
}

func (e *authorityHealthExecutor) MCPHealthCheck(context.Context, []string) error {
	e.healthAt = time.Now().UTC()
	return nil
}

func (*authorityHealthExecutor) Execute(
	context.Context,
	ir.Node,
	map[string]any,
) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestSkipMCPHealthFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"", false},
		{"0", false},
		{"false", false},
		{"yes", false}, // only "1" / "true" (any case) are truthy
	}
	for _, c := range cases {
		t.Setenv("ITERION_SKIP_MCP_HEALTH", c.val)
		if got := skipMCPHealthFromEnv(); got != c.want {
			t.Errorf("skipMCPHealthFromEnv() with ITERION_SKIP_MCP_HEALTH=%q = %v, want %v", c.val, got, c.want)
		}
	}
}

func TestRunCapturesWorktreeAuthorityBeforeMCPHealthCheck(t *testing.T) {
	t.Setenv("ITERION_MCP_HEALTHCHECK", "true")
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	runGit("config", "commit.gpgsign", "false")

	botPath := filepath.Join(repo, "authority.bot")
	writeFile(t, botPath, `mcp_server early:
  transport: stdio
  command: "not-started-by-this-test"

workflow authority:
  entry: fail
  mcp:
    servers: [early]
`)
	runGit("add", "authority.bot")
	runGit("commit", "-m", "test fixture")
	t.Chdir(repo)

	executor := &authorityHealthExecutor{}
	storeDir := filepath.Join(t.TempDir(), "store")
	runErr := RunRun(
		context.Background(),
		RunOptions{
			File:     botPath,
			StoreDir: storeDir,
			RunID:    "run-mcp-authority",
			Executor: executor,
		},
		&Printer{W: io.Discard, Format: OutputJSON},
	)
	if runErr == nil {
		t.Fatal("RunRun reached the explicit fail node without an error")
	}
	if executor.healthAt.IsZero() {
		t.Fatal("MCP health check was not invoked")
	}
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	r, err := s.LoadRun(context.Background(), "run-mcp-authority")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.WorktreeCreatedAt.IsZero() {
		t.Fatal("run has no trusted worktree process boundary")
	}
	if r.WorktreeCreatedAt.After(executor.healthAt) {
		t.Fatalf(
			"worktree authority %s was captured after MCP health check %s",
			r.WorktreeCreatedAt.Format(time.RFC3339Nano),
			executor.healthAt.Format(time.RFC3339Nano),
		)
	}
}
