package tool

import (
	"slices"
	"strings"
	"testing"
)

// The FQN spelling cannot be split unambiguously: claude_code writes
// `mcp__<server>__<tool>` and an MCP tool name may itself contain "__", so
// `mcp__a__b__c` is server `a` tool `b__c` AND server `a__b` tool `c`.
// Resolution has to pick one (a map key is one key); the callers that ask
// "is this name's server in that SET" must see every reading, or a guard
// misses the server it was built to withhold.
func TestEveryReadingOfAnAmbiguousFQNIsOffered(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"mcp.srv.tool", []string{"srv"}},
		// A dotted name is unambiguous: the server ends at the FIRST dot.
		{"mcp.srv.tool.with.dots", []string{"srv"}},
		{"mcp__srv__tool", []string{"srv"}},
		{"mcp__srv__tool_with_underscores", []string{"srv"}},
		{"mcp__a__b__c", []string{"a", "a__b"}},
		{"mcp__a__b__c__d", []string{"a", "a__b", "a__b__c"}},
		// Nothing to split: no inner "__", or an empty half.
		{"bash", nil},
		{"mcp__srv", nil},
		{"mcp__srv__", nil},
		{"mcp____tool", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := MCPServerCandidatesOf(tc.name)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("MCPServerCandidatesOf(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// And the two readers must not disagree about the name resolution uses: the
// LAST reading is exactly SplitMCPFQN's pick, so a tool that resolves is
// always among the candidates a guard checks. A candidate list that drifted
// from the splitter would let a refused server's tool resolve.
func TestTheCandidatesEndOnTheNameResolutionUses(t *testing.T) {
	for _, ref := range []string{
		"mcp__srv__tool",
		"mcp__a__b__c",
		"mcp__a__b__c__d",
		"mcp__a____c",
	} {
		t.Run(ref, func(t *testing.T) {
			server, _, ok := SplitMCPFQN(ref)
			candidates := MCPServerCandidatesOf(ref)
			if !ok {
				if len(candidates) != 0 {
					t.Fatalf("nothing resolves for %q, yet candidates were offered: %v", ref, candidates)
				}
				return
			}
			if len(candidates) == 0 || candidates[len(candidates)-1] != server {
				t.Errorf("the name resolution uses (%q) must be the last candidate, got %v", server, candidates)
			}
			// And the first candidate is claude_code's own reading: the
			// shortest server, everything after it the tool name.
			if want := strings.SplitN(strings.TrimPrefix(ref, "mcp__"), "__", 2)[0]; candidates[0] != want {
				t.Errorf("the first candidate must be the shortest server %q, got %q", want, candidates[0])
			}
		})
	}
}
