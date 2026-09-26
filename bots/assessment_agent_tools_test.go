package bots

import (
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// NO AGENT OF THIS BUNDLE HOLDS A SHELL. `readonly: true` is honoured by
// the codex and pi backends and ignored by claude_code, which runs with
// bypassPermissions: a node that declared the flag and a shell held an
// unlocked shell on a checkout of an untrusted repository. The drafting agent
// writes two files and needs no shell to do it. What carries the
// property is the TOOL SET, so the tool set is what is pinned — an allowlist
// of canonical tools, because a shell has many spellings (`bash`, `shell`,
// `run_command`, …) and a denylist would enumerate them.
func TestAssessmentAgentsHoldNoShell(t *testing.T) {
	pr := parseBotUnit("assessment/main.bot")
	if pr.File == nil {
		t.Fatal("parse produced no File")
	}
	compiled := ir.Compile(pr.File)
	if compiled.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}
	allowlists := map[string][]string{
		// Names stacks and a perimeter from what it reads; run_extractors is
		// the node that executes anything.
		"survey": {"read", "glob", "grep", "skill"},
		// Writes judgement prose into its structured output, nothing to disk.
		"judgement": {"skill"},
		// Writes two files — the contract and its outcomes — and runs none of
		// the gates it writes: contract_lint does, in a bounded environment.
		"plan_draft": {"read", "glob", "grep", "skill", "write", "edit"},
	}
	// EVERY agent of the bundle is classified here: an agent added later with
	// no entry would be the one holding a shell nobody decided to give it.
	for id, node := range compiled.Workflow.Nodes {
		if _, isAgent := node.(*ir.AgentNode); isAgent {
			if _, classified := allowlists[id]; !classified {
				t.Errorf("agent %q has no tool allowlist here — decide what it may hold, and write it down", id)
			}
		}
	}
	for node, allowed := range allowlists {
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

// THE ASSESSED REPOSITORY DOES NOT CHOOSE THE BINARIES. Provisioned, the
// target's devbox.json is prepended to PATH ahead of the bundle's, so the
// repository under assessment would pick the `python3` every tool node runs
// under and the `yq` and `git` they call — packages it declares, flakes
// included. This run reads the repository; it does not build it.
func TestAssessmentDoesNotProvisionTheAssessedRepositorysToolchain(t *testing.T) {
	pr := parseBotUnit("assessment/main.bot")
	if pr.File == nil {
		t.Fatal("parse produced no File")
	}
	compiled := ir.Compile(pr.File)
	if compiled.Workflow == nil {
		t.Fatal("compile produced no Workflow")
	}
	if got := compiled.Workflow.RepoDevbox; got != "off" {
		t.Fatalf("repo_devbox = %q, want off: the assessed repository's declared toolchain would come "+
			"first on the PATH this bundle's deterministic nodes resolve their interpreters from", got)
	}
}
