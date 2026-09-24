package bots

import (
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// THE AGENTS THAT ONLY READ HOLD NO SHELL. `readonly: true` is honoured by
// the codex and pi backends and ignored by claude_code, which runs with
// bypassPermissions: a node that declared the flag and a shell held an
// unlocked shell on a checkout of an untrusted repository. What carries the
// property is the TOOL SET, so the tool set is what is pinned — an allowlist
// of canonical tools, because a shell has many spellings (`bash`, `shell`,
// `run_command`, …) and a denylist would enumerate them.
func TestAssessmentReadingAgentsHoldNoShell(t *testing.T) {
	pr := parseBotUnit("assessment/main.bot")
	if pr.File == nil {
		t.Fatal("parse produced no File")
	}
	compiled := ir.Compile(pr.File)
	if compiled.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}
	for node, allowed := range map[string][]string{
		// Names stacks and a perimeter from what it reads; run_extractors is
		// the node that executes anything.
		"survey": {"read", "glob", "grep", "skill"},
		// Writes judgement prose into its structured output, nothing to disk.
		"judgement": {"skill"},
	} {
		agent, ok := compiled.Workflow.Nodes[node].(*ir.AgentNode)
		if !ok {
			t.Fatalf("%s is %T, want *ir.AgentNode", node, compiled.Workflow.Nodes[node])
		}
		// Undeclared is the backend's whole roster, shell included.
		if len(agent.Tools) == 0 {
			t.Fatalf("%s declares no tools: an undeclared list is the backend's full roster, "+
				"shell included", node)
		}
		permitted := map[string]bool{}
		for _, name := range allowed {
			permitted[name] = true
		}
		var extra []string
		for _, declared := range agent.Tools {
			if !permitted[toolcatalog.CanonicalToolName(declared)] {
				extra = append(extra, declared)
			}
		}
		sort.Strings(extra)
		if len(extra) > 0 {
			t.Errorf("%s declares %v beyond what it needs (%v): on claude_code nothing but the tool "+
				"list bounds it, and it reads a repository its own prompt calls untrusted",
				node, extra, allowed)
		}
	}
}
