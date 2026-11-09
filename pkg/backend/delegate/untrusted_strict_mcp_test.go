package delegate

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestClaudeCode_AForkIsAlwaysStrictMCP.
//
// ITERION_CLAUDE_CODE_STRICT_MCP=0 is the operator's escape hatch back to host
// MCP inheritance. On a workspace its tenant did not write it lifts nothing:
// every spawn of the node — the main pass and the formatting pass — carries
// --strict-mcp-config. The trusted row is the control that the knob works.
func TestClaudeCode_AForkIsAlwaysStrictMCP(t *testing.T) {
	t.Setenv("ITERION_CLAUDE_CODE_STRICT_MCP", "0")
	for _, row := range []struct {
		name   string
		trust  store.RunTrust
		strict bool
	}{
		{"trusted, the knob off (control)", store.RunTrustDefault, false},
		{"fork, the knob off", store.RunTrustFork, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			spawns := spawnArgv(t, Task{NodeID: "n", WorkspaceTrust: row.trust, OutputSchema: []byte(`{"type":"object"}`)})
			if len(spawns) == 0 {
				t.Fatal("no spawn recorded — this row proves nothing")
			}
			for i, argv := range spawns {
				if got := strings.Contains(argv, "--strict-mcp-config"); got != row.strict {
					t.Errorf("spawn %d: --strict-mcp-config = %v, want %v\n%s", i+1, got, row.strict, argv)
				}
			}
		})
	}
}
