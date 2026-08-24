package delegate

import (
	"bytes"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate/claudesdk"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The MCP set iterion resolves for a node travels via --mcp-config; without
// --strict-mcp-config the CLI merges the operator's personal user-scope
// servers (~/.claude.json) on top — undeclared tools reaching the agent,
// per-visit npx/server boots on loop-heavy bots, personal API keys on the
// subprocess argv (issue #506). The transport options must therefore emit
// the flag by default.
func TestClaudeCodeSpawn_StrictMCPConfigByDefault(t *testing.T) {
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	opts, cleanup := b.buildTransportOptions(Task{})
	if cleanup != nil {
		defer cleanup()
	}
	_, args := claudesdk.ResolveSpawn(opts...)
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Error("claude_code spawns must carry --strict-mcp-config by default — without it the operator's ~/.claude.json MCP servers boot inside every bot node")
	}
}

// ITERION_CLAUDE_CODE_STRICT_MCP=0 is the greppable escape hatch for an
// operator who deliberately wants their host MCP config inherited by nodes.
func TestClaudeCodeSpawn_StrictMCPConfigEnvOptOut(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_STRICT_MCP", "0")
	b := &ClaudeCodeBackend{Logger: iterlog.New(iterlog.LevelError, &bytes.Buffer{})}
	opts, cleanup := b.buildTransportOptions(Task{})
	if cleanup != nil {
		defer cleanup()
	}
	_, args := claudesdk.ResolveSpawn(opts...)
	if slices.Contains(args, "--strict-mcp-config") {
		t.Error("ITERION_CLAUDE_CODE_STRICT_MCP=0 must restore host MCP inheritance (no --strict-mcp-config)")
	}
}

func TestStrictMCPFromEnv_Values(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", true}, // set-but-empty is not an opt-out
		{"1", true},
		{"true", true},
		{"on", true},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"off", false},
		{"no", false},
		{" off ", false},
	}
	for _, c := range cases {
		t.Setenv("ITERION_CLAUDE_CODE_STRICT_MCP", c.val)
		if got := strictMCPFromEnv(); got != c.want {
			t.Errorf("strictMCPFromEnv() with %q = %v, want %v", c.val, got, c.want)
		}
	}
}
