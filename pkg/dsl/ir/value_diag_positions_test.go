package ir

import (
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A refused switch value names its line: the `workflow <name>:` header for
// a workflow-level property (the declaration keeps no per-property
// position), the node's own `<kind> <name>:` line for a node-level one.
// These were emitted with no attribution at all, so `iterion validate`
// printed a bare code where C142 — the same class — printed a file:line.
func TestValueDiagnosticsCarryASourcePosition(t *testing.T) {
	const wf = "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  %s: %s\n  a -> done\n"
	const node = "agent a:\n  model: \"m\"\n  %s: %s\nworkflow w:\n  entry: a\n  a -> done\n"
	cases := []struct {
		src  string
		code DiagCode
		line int
	}{
		{fmt.Sprintf(wf, "compress", "zz"), DiagInvalidCompress, 3},
		{fmt.Sprintf(wf, "auto_memory", "zz"), DiagInvalidAutoMemory, 3},
		{fmt.Sprintf(wf, "loop_budget_guard", "zz"), DiagInvalidLoopBudgetGuard, 3},
		{fmt.Sprintf(wf, "repo_devbox", "zz"), DiagInvalidRepoDevbox, 3},
		{fmt.Sprintf(wf, "workspace_checkpoint", "zz"), DiagInvalidWorkspaceCheckpoint, 3},
		{fmt.Sprintf(wf, "worktree", "zz"), DiagInvalidWorktree, 3},
		{fmt.Sprintf(wf, "permission", "zz"), DiagInvalidPermission, 3},
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  compaction:\n    threshold: 2\n  a -> done\n", DiagInvalidCompaction, 3},
		// The workflow-level `on` nothing honours: the header, not the node.
		{"agent a:\n  model: \"m\"\n  backend: \"codex\"\nworkflow w:\n  entry: a\n  auto_memory: on\n  a -> done\n", DiagAutoMemoryNotSupported, 4},
		{fmt.Sprintf(node, "compress", "zz"), DiagInvalidCompress, 1},
		{fmt.Sprintf(node, "auto_memory", "zz"), DiagInvalidAutoMemory, 1},
		{fmt.Sprintf(node, "permission", "zz"), DiagInvalidPermission, 1},
		{fmt.Sprintf(node, "timeout", "\"abc\""), DiagInvalidNodeTimeout, 1},
		{fmt.Sprintf(node, "timeout", "\"-1s\""), DiagInvalidNodeTimeout, 1},
		{"agent a:\n  model: \"m\"\n  compaction:\n    threshold: 2\nworkflow w:\n  entry: a\n  a -> done\n", DiagInvalidCompaction, 1},
		{"agent a:\n  model: \"m\"\n  backend: \"codex\"\n  auto_memory: on\nworkflow w:\n  entry: a\n  a -> done\n", DiagAutoMemoryNotSupported, 1},
		// The SECOND node's line, so the position is the node's and not
		// the file's first declaration by accident.
		{"agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n  compress: zz\nworkflow w:\n  entry: a\n  a -> b\n  b -> done\n", DiagInvalidCompress, 3},
		// A sandbox block's value (C044), workflow and node scope — and the
		// workflow NAMED LIKE A NODE, where a node-table lookup of the
		// workflow's name pointed at the node.
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    mode: zz\n  a -> done\n", DiagInvalidSandboxMode, 3},
		{"agent a:\n  model: \"m\"\n  sandbox:\n    mode: zz\nworkflow w:\n  entry: a\n  a -> done\n", DiagInvalidSandboxMode, 1},
		{"agent a:\n  model: \"m\"\nworkflow a:\n  entry: a\n  sandbox:\n    mode: zz\n  a -> done\n", DiagInvalidSandboxMode, 3},
		// The rest of the class: every check that names a node, a workflow
		// or an mcp_server declaration.
		{"tool t:\n  command: \"true\"\n  permission: deny\nworkflow w:\n  entry: t\n  t -> done\n", DiagToolNodePermissionInert, 1},
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  allow: [\"Read(**)\"]\n  a -> done\n", DiagPermissionRulesNoGate, 3},
		{"agent a:\n  model: \"m\"\nhuman h:\n  interaction: review\nworkflow w:\n  entry: a\n  worktree: none\n  a -> h\n  h -> done\n", DiagReviewNeedsWorktree, 3},
		{"agent a:\n  model: \"m\"\n  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    visibility: \"zz\"\nworkflow w:\n  entry: a\n  a -> done\n", DiagMemoryInvalidVisibility, 1},
		{"agent a:\n  model: \"m\"\n  backend: \"claw\"\n  memory:\n    enabled: true\nworkflow w:\n  entry: a\n  a -> done\n", DiagMemoryMissingScope, 1},
		{"agent a:\n  model: \"m\"\n  backend: \"codex\"\n  memory:\n    enabled: true\n    scope: \"s\"\nworkflow w:\n  entry: a\n  a -> done\n", DiagMemoryNotSupported, 1},
		{"agent a:\n  model: \"m\"\n  reasoning_effort: ultracode\nworkflow w:\n  entry: a\n  a -> done\n", DiagUltracodeModelGate, 1},
		{"agent a:\n  model: \"m\"\n  max_tokens: 10\nworkflow w:\n  entry: a\n  budget:\n    max_tokens: 5\n  a -> done\n", DiagNodeMaxTokensVsBudget, 1},
		{"mcp_server s:\n  transport: stdio\n  command: \"x\"\n  auth:\n    type: \"basic\"\nagent a:\n  model: \"m\"\n  mcp:\n    servers: [s]\nworkflow w:\n  entry: a\n  a -> done\n", DiagUnsupportedMCPAuth, 1},
	}
	for _, tc := range cases {
		pr := parser.Parse("p.bot", tc.src)
		if len(pr.Diagnostics) != 0 {
			t.Fatalf("%q: parse: %v", tc.src, pr.Diagnostics)
		}
		found := false
		for _, d := range Compile(pr.File).Diagnostics {
			if d.Code != tc.code {
				continue
			}
			found = true
			if d.File != "p.bot" || d.Line != tc.line {
				t.Errorf("%s for %q: at %s:%d:%d, want p.bot:%d", tc.code, tc.src, d.File, d.Line, d.Column, tc.line)
			}
		}
		if !found {
			t.Errorf("%s: not raised for %q", tc.code, tc.src)
		}
	}
}
