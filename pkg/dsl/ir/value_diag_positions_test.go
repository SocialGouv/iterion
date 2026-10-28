package ir

import (
	"fmt"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"testing"
)

// A diagnostic about a property's VALUE names the line the value is written
// on — the property's own line in the workflow or in the node, where the
// declaration's header was the only position before — and names the
// property, its span closed past the value (#1094). A diagnostic about the
// declaration itself keeps the header. These were emitted with no
// attribution at all once, so `iterion validate` printed a bare code.
func TestValueDiagnosticsCarryASourcePosition(t *testing.T) {
	const wf = "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  %s: %s\n  a -> done\n"
	const node = "agent a:\n  model: \"m\"\n  %s: %s\nworkflow w:\n  entry: a\n  a -> done\n"
	cases := []struct {
		src      string
		code     DiagCode
		line     int
		column   int
		property string
		// endLine and endColumn, when set, are the span's end — the column
		// after the value's last rune.
		endLine, endColumn int
	}{
		{fmt.Sprintf(wf, "compress", "zz"), DiagInvalidCompress, 5, 3, "compress", 5, 15},
		{fmt.Sprintf(wf, "auto_memory", "zz"), DiagInvalidAutoMemory, 5, 3, "auto_memory", 5, 18},
		{fmt.Sprintf(wf, "loop_budget_guard", "zz"), DiagInvalidLoopBudgetGuard, 5, 3, "loop_budget_guard", 0, 0},
		{fmt.Sprintf(wf, "repo_devbox", "zz"), DiagInvalidRepoDevbox, 5, 3, "repo_devbox", 0, 0},
		{fmt.Sprintf(wf, "workspace_checkpoint", "zz"), DiagInvalidWorkspaceCheckpoint, 5, 3, "workspace_checkpoint", 0, 0},
		{fmt.Sprintf(wf, "worktree", "zz"), DiagInvalidWorktree, 5, 3, "worktree", 5, 15},
		{fmt.Sprintf(wf, "permission", "zz"), DiagInvalidPermission, 5, 3, "permission", 0, 0},
		// A block property spans its body.
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  compaction:\n    threshold: 2\n  a -> done\n", DiagInvalidCompaction, 5, 3, "compaction", 6, 17},
		// The workflow-level `on` nothing honours: the workflow's line, not the node's.
		{"agent a:\n  model: \"m\"\n  backend: \"codex\"\nworkflow w:\n  entry: a\n  auto_memory: on\n  a -> done\n", DiagAutoMemoryNotSupported, 6, 3, "auto_memory", 6, 18},
		{fmt.Sprintf(node, "compress", "zz"), DiagInvalidCompress, 3, 3, "compress", 3, 15},
		{fmt.Sprintf(node, "auto_memory", "zz"), DiagInvalidAutoMemory, 3, 3, "auto_memory", 0, 0},
		{fmt.Sprintf(node, "permission", "zz"), DiagInvalidPermission, 3, 3, "permission", 0, 0},
		{fmt.Sprintf(node, "timeout", "\"abc\""), DiagInvalidNodeTimeout, 3, 3, "timeout", 3, 17},
		{fmt.Sprintf(node, "timeout", "\"-1s\""), DiagInvalidNodeTimeout, 3, 3, "timeout", 0, 0},
		{"agent a:\n  model: \"m\"\n  compaction:\n    threshold: 2\nworkflow w:\n  entry: a\n  a -> done\n", DiagInvalidCompaction, 3, 3, "compaction", 4, 17},
		{"agent a:\n  model: \"m\"\n  backend: \"codex\"\n  auto_memory: on\nworkflow w:\n  entry: a\n  a -> done\n", DiagAutoMemoryNotSupported, 4, 3, "auto_memory", 0, 0},
		// The SECOND node's line, so the position is the node's and not
		// the file's first declaration by accident.
		{"agent a:\n  model: \"m\"\nagent b:\n  model: \"m\"\n  compress: zz\nworkflow w:\n  entry: a\n  a -> b\n  b -> done\n", DiagInvalidCompress, 5, 3, "compress", 0, 0},
		// A group member's line, under the id the instance gives it.
		{"group g(p):\n  agent m:\n    model: \"m\"\n    compress: zz\nuse g as x with { p: \"v\" }\nworkflow w:\n  entry: x.m\n  x.m -> done\n", DiagInvalidCompress, 4, 5, "compress", 4, 17},
		// A sandbox block's value (C044), workflow and node scope — and the
		// workflow NAMED LIKE A NODE, where a node-table lookup of the
		// workflow's name pointed at the node.
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    mode: zz\n  a -> done\n", DiagInvalidSandboxMode, 5, 3, "sandbox", 6, 13},
		{"agent a:\n  model: \"m\"\n  sandbox:\n    mode: zz\nworkflow w:\n  entry: a\n  a -> done\n", DiagInvalidSandboxMode, 3, 3, "sandbox", 4, 13},
		{"agent a:\n  model: \"m\"\n  sandbox:\n    mode: none\nworkflow a:\n  entry: a\n  sandbox:\n    mode: zz\n  a -> done\n", DiagInvalidSandboxMode, 7, 3, "sandbox", 8, 13},
		// The rest of the class: every check about a node's or the
		// workflow's property.
		{"tool t:\n  command: \"true\"\n  permission: deny\nworkflow w:\n  entry: t\n  t -> done\n", DiagToolNodePermissionInert, 3, 3, "permission", 0, 0},
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  allow: [\"Read(**)\"]\n  a -> done\n", DiagPermissionRulesNoGate, 5, 3, "allow", 5, 22},
		{"agent a:\n  model: \"m\"\nhuman h:\n  interaction: review\nworkflow w:\n  entry: a\n  worktree: none\n  a -> h\n  h -> done\n", DiagReviewNeedsWorktree, 4, 3, "interaction", 0, 0},
		{"agent a:\n  model: \"m\"\n  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    visibility: \"zz\"\nworkflow w:\n  entry: a\n  a -> done\n", DiagMemoryInvalidVisibility, 4, 3, "memory", 7, 21},
		{"agent a:\n  model: \"m\"\n  backend: \"claw\"\n  memory:\n    enabled: true\nworkflow w:\n  entry: a\n  a -> done\n", DiagMemoryMissingScope, 4, 3, "memory", 5, 18},
		{"agent a:\n  model: \"m\"\n  backend: \"codex\"\n  memory:\n    enabled: true\n    scope: \"s\"\nworkflow w:\n  entry: a\n  a -> done\n", DiagMemoryNotSupported, 4, 3, "memory", 6, 15},
		{"agent a:\n  model: \"m\"\n  reasoning_effort: ultracode\nworkflow w:\n  entry: a\n  a -> done\n", DiagUltracodeModelGate, 3, 3, "reasoning_effort", 0, 0},
		{"agent a:\n  model: \"m\"\n  max_tokens: 10\nworkflow w:\n  entry: a\n  budget:\n    max_tokens: 5\n  a -> done\n", DiagNodeMaxTokensVsBudget, 3, 3, "max_tokens", 0, 0},
		// The permission rule lists, by their own line: an unreadable rule
		// (C154) on the workflow and on a node; the inert lists (C111) on the
		// list written FIRST in the source, not first in the registry; the
		// class-wide grant (C167).
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  allow: [\"class:shel\"]\n  a -> done\n", DiagInvalidPermissionRule, 5, 3, "allow", 5, 24},
		{"agent a:\n  model: \"m\"\n  allow: [\"class:shel\"]\nworkflow w:\n  entry: a\n  a -> done\n", DiagInvalidPermissionRule, 3, 3, "allow", 0, 0},
		{"tool t:\n  command: \"true\"\nworkflow w:\n  entry: t\n  deny: [\"Read(**)\"]\n  t -> done\n", DiagPermissionRulesNoGate, 5, 3, "deny", 0, 0},
		{"tool t:\n  command: \"true\"\nworkflow w:\n  entry: t\n  deny: [\"Read(**)\"]\n  allow: [\"Bash(ls)\"]\n  t -> done\n", DiagPermissionRulesNoGate, 5, 3, "deny", 0, 0},
		{"agent a:\n  model: \"m\"\n  allow: [\"Read(**)\"]\nworkflow w:\n  entry: a\n  permission: ask\n  allow: [\"Bash(ls)\"]\n  a -> done\n", DiagPermissionRulesNoGate, 7, 3, "allow", 0, 0},
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  permission: ask\n  allow: [\"class:shell\"]\n  a -> done\n", DiagPermissionClassRisk, 6, 3, "allow", 0, 0},
		{"agent a:\n  model: \"m\"\n  permission: ask\n  allow: [\"class:shell\"]\nworkflow w:\n  entry: a\n  a -> done\n", DiagPermissionClassRisk, 4, 3, "allow", 0, 0},
		// The tool bodies, by the body's property (C157, C158, C188, C315).
		{"tool t:\n  command: \"git add {{run.tree_noise}}\"\nworkflow w:\n  entry: t\n  t -> done\n", DiagTreeNoiseRefInExecBody, 2, 3, "command", 0, 0},
		{"tool t:\n  command: \"git add '$ITERION_TREE_NOISE'\"\nworkflow w:\n  entry: t\n  t -> done\n", DiagTreeNoiseEnvQuoted, 2, 3, "command", 0, 0},
		{"tool t:\n  command: \"PORT=1; echo ${PORT}\"\nworkflow w:\n  entry: t\n  t -> done\n", DiagBracedEnvDefeatsAssignment, 2, 3, "command", 0, 0},
		{"tool t:\n  command: \"echo ${FOO}\"\nworkflow w:\n  entry: t\n  t -> done\n", DiagEnvDialInRunSetting, 2, 3, "command", 0, 0},
		{"agent a:\n  model: \"${M:-x}\"\nworkflow w:\n  entry: a\n  a -> done\n", DiagEnvDialInRunSetting, 2, 3, "model", 0, 0},
		// The ambient context: its own line when the node writes it (C184,
		// C185, C187); a value INHERITED from the workflow has no line on
		// the node, so the node's header and no property claimed.
		{"agent a:\n  model: \"m\"\n  ambient_context: zz\nworkflow w:\n  entry: a\n  a -> done\n", DiagInvalidAmbientContext, 3, 3, "ambient_context", 0, 0},
		{"agent a:\n  model: \"m\"\n  backend: \"gemini\"\n  ambient_context: all\nworkflow w:\n  entry: a\n  a -> done\n", DiagAmbientContextNotEnforced, 4, 3, "ambient_context", 0, 0},
		{"agent a:\n  model: \"m\"\n  backend: \"kimi\"\n  ambient_context: operator\nworkflow w:\n  entry: a\n  a -> done\n", DiagAmbientContextRefused, 4, 3, "ambient_context", 0, 0},
		{"agent a:\n  model: \"m\"\n  backend: \"kimi\"\nworkflow w:\n  entry: a\n  ambient_context: operator\n  a -> done\n", DiagAmbientContextRefused, 1, 1, "", 0, 0},
		// The other node values: effort, posture and merge strategy, policy,
		// a fail code, an action's retry, a wait's and an await's timeout.
		{"agent a:\n  model: \"m\"\nhuman h:\n  interaction: review\n  posture: zz\nworkflow w:\n  entry: a\n  worktree: auto\n  a -> h\n  h -> done\n", DiagInvalidReviewGateValue, 5, 3, "posture", 0, 0},
		{"agent a:\n  model: \"m\"\nhuman h:\n  interaction: review\n  merge_strategy: zz\nworkflow w:\n  entry: a\n  worktree: auto\n  a -> h\n  h -> done\n", DiagInvalidReviewGateValue, 5, 3, "merge_strategy", 0, 0},
		{"tool t:\n  command: \"true\"\n  policy: zz\nworkflow w:\n  entry: t\n  t -> done\n", DiagInvalidPolicy, 3, 3, "policy", 0, 0},
		{"agent a:\n  model: \"m\"\nfail f:\n  code: lowercase\nworkflow w:\n  entry: a\n  a -> f\n", DiagInvalidFailCode, 4, 3, "code", 0, 0},
		{"agent a:\n  model: \"m\"\nwait w:\n  event: \"x\"\n  timeout: \"abc\"\nworkflow wf:\n  entry: a\n  a -> w\n  w -> done\n", DiagWaitNoTimeout, 5, 3, "timeout", 0, 0},
		{"agent a:\n  model: \"m\"\nawait_answers aa:\n  timeout: \"abc\"\nworkflow wf:\n  entry: a\n  a -> aa\n  aa -> done\n", DiagAwaitAnswersNoTimeout, 4, 3, "timeout", 0, 0},
		// A compute expression that does not parse: the entry's own line,
		// the block holding one line per field.
		{"compute c:\n  expr:\n    f: \"1 +\"\nworkflow w:\n  entry: c\n  c -> done\n", DiagBadExpr, 3, 5, "", 0, 0},
		// The sandbox block: a network without a mode (C312) and the
		// browser image Playwright needs (C060).
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    mode: inline\n    image: \"img\"\n    network:\n      rules: [\"example.org\"]\n  a -> done\n", DiagNetworkModeMissing, 5, 3, "sandbox", 0, 0},
		{"mcp_server pw:\n  transport: stdio\n  command: \"playwright-mcp\"\nagent a:\n  model: \"m\"\n  mcp:\n    servers: [pw]\nworkflow w:\n  entry: a\n  sandbox:\n    mode: inline\n    image: \"ghcr.io/x/slim\"\n  a -> done\n", DiagPlaywrightNeedsBrowserImage, 10, 3, "sandbox", 0, 0},
		// Two declarations of one id: the compiled node is the FIRST, and so
		// is the table its diagnostics read.
		{"agent a:\n  model: \"m\"\n  compress: zz\nagent a:\n  model: \"m\"\n  compress: yy\nworkflow w:\n  entry: a\n  a -> done\n", DiagInvalidCompress, 3, 3, "compress", 0, 0},
		// A span the parser records with an end that is its start (a var
		// field's) closes nothing: only a property span has an end.
		{"vars:\n  count: int [matching: \"^[0-9]+$\"]\nagent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  a -> done\n", DiagVarMatchingNonString, 2, 3, "", 0, 0},
		// The rule lists written in another order: the deny: list refused
		// (C154) and the inert lists (C111) on the ask: written first, at the
		// workflow and at a node whose gate is off.
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  deny: [\"class:shel\"]\n  a -> done\n", DiagInvalidPermissionRule, 5, 3, "deny", 0, 0},
		{"agent a:\n  model: \"m\"\n  deny: [\"class:shel\"]\nworkflow w:\n  entry: a\n  a -> done\n", DiagInvalidPermissionRule, 3, 3, "deny", 0, 0},
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  ask: [\"Bash(ls)\"]\n  allow: [\"Read(**)\"]\n  a -> done\n", DiagPermissionRulesNoGate, 5, 3, "ask", 0, 0},
		{"agent a:\n  model: \"m\"\n  ask: [\"Bash(ls)\"]\nworkflow w:\n  entry: a\n  permission: ask\n  ask: [\"Bash(rm)\"]\n  allow: [\"Read(**)\"]\n  a -> done\n", DiagPermissionRulesNoGate, 7, 3, "ask", 0, 0},
		{"agent a:\n  model: \"m\"\n  permission: off\n  deny: [\"Read(**)\"]\nworkflow w:\n  entry: a\n  permission: ask\n  a -> done\n", DiagPermissionRulesNoGate, 4, 3, "deny", 0, 0},
		// The workflow's ambient_context (C184 invalid, C185 nothing enforces it).
		{fmt.Sprintf(wf, "ambient_context", "zz"), DiagInvalidAmbientContext, 5, 3, "ambient_context", 0, 0},
		{"agent a:\n  model: \"m\"\n  backend: \"gemini\"\nworkflow w:\n  entry: a\n  ambient_context: all\n  a -> done\n", DiagAmbientContextNotEnforced, 6, 3, "ambient_context", 0, 0},
		// The sandbox block's other refusals (C044): inline without an image,
		// image and build together, a host_state.
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    mode: inline\n  a -> done\n", DiagInvalidSandboxMode, 5, 3, "sandbox", 0, 0},
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    mode: inline\n    image: \"img\"\n    build:\n      dockerfile: \"Dockerfile\"\n  a -> done\n", DiagInvalidSandboxMode, 5, 3, "sandbox", 0, 0},
		{"agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  sandbox:\n    mode: auto\n    host_state: zz\n  a -> done\n", DiagInvalidSandboxMode, 5, 3, "sandbox", 0, 0},
		// The script body (C157) and the double-quoted env read (C158).
		{"tool t:\n  script: |\n    git add {{run.tree_noise}}\n  language: sh\nworkflow w:\n  entry: t\n  t -> done\n", DiagTreeNoiseRefInExecBody, 2, 3, "script", 0, 0},
		{"tool t:\n  command: |\n    git add \"$ITERION_TREE_NOISE\"\nworkflow w:\n  entry: t\n  t -> done\n", DiagTreeNoiseEnvQuoted, 2, 3, "command", 0, 0},
		// auto_memory: on that every node opts out of (C132), a non-positive
		// wait timeout (C197), a quoted effort the model has no word for
		// (C027), the memory block's visibility arms (C171, C319).
		{"agent a:\n  model: \"m\"\n  auto_memory: off\nworkflow w:\n  entry: a\n  auto_memory: on\n  a -> done\n", DiagAutoMemoryNotSupported, 6, 3, "auto_memory", 0, 0},
		{"agent a:\n  model: \"m\"\nwait w:\n  event: \"x\"\n  timeout: \"0s\"\nworkflow wf:\n  entry: a\n  a -> w\n  w -> done\n", DiagWaitNoTimeout, 5, 3, "timeout", 0, 0},
		{"agent a:\n  model: \"m\"\n  reasoning_effort: \"bogus\"\nworkflow w:\n  entry: a\n  a -> done\n", DiagInvalidReasoningEffort, 3, 3, "reasoning_effort", 0, 0},
		{"agent a:\n  model: \"m\"\n  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    visibility: \"global\"\nworkflow w:\n  entry: a\n  a -> done\n", DiagMemoryGlobalWriteGated, 4, 3, "memory", 0, 0},
		{"agent a:\n  model: \"m\"\n  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    visibility: \"bot\"\n    project_root: true\nworkflow w:\n  entry: a\n  a -> done\n", DiagMemoryVisibilityConflict, 4, 3, "memory", 0, 0},
		// An action's timeout and retry (C265), a property the recipe alone
		// reads (C266), a reserved fail code (C248).
		{"tool t:\n  action: \"github.issue\"\n  timeout: \"abc\"\nworkflow w:\n  entry: t\n  t -> done\n", DiagActionBadTimeout, 3, 3, "timeout", 0, 0},
		{"tool t:\n  action: \"github.issue\"\n  retry: \"many\"\nworkflow w:\n  entry: t\n  t -> done\n", DiagActionBadTimeout, 3, 3, "retry", 0, 0},
		{"tool t:\n  command: \"true\"\n  retry: \"3\"\nworkflow w:\n  entry: t\n  t -> done\n", DiagActionOnlyProperty, 3, 3, "retry", 0, 0},
		{"agent a:\n  model: \"m\"\nfail f:\n  code: BUDGET_EXCEEDED\nworkflow w:\n  entry: a\n  a -> f\n", DiagReservedFailCode, 4, 3, "code", 0, 0},
		// A route dial inside a block: the block's line (recovery:, fallbacks:).
		{"tool t:\n  command: \"true\"\n  recovery:\n    model: \"${M:-x}\"\nworkflow w:\n  entry: t\n  t -> done\n", DiagEnvDialInRunSetting, 3, 3, "recovery", 0, 0},
		{"agent a:\n  model: \"m\"\n  fallbacks:\n    spare:\n      backend: \"claw\"\n      model: \"${FB:-x}\"\nworkflow w:\n  entry: a\n  a -> done\n", DiagEnvDialInRunSetting, 3, 3, "fallbacks", 0, 0},
		// About a declaration kind that keeps no table (an mcp_server): its
		// header, no property.
		{"mcp_server s:\n  transport: stdio\n  command: \"x\"\n  auth:\n    type: \"basic\"\nagent a:\n  model: \"m\"\n  mcp:\n    servers: [s]\nworkflow w:\n  entry: a\n  a -> done\n", DiagUnsupportedMCPAuth, 1, 1, "", 0, 0},
	}
	for _, tc := range cases {
		pr := parsertest.Parse("p.bot", tc.src)
		if len(pr.Diagnostics) != 0 {
			t.Fatalf("%q: parse: %v", tc.src, pr.Diagnostics)
		}
		found := false
		for _, d := range Compile(pr.File).Diagnostics {
			if d.Code != tc.code {
				continue
			}
			found = true
			if d.File != "p.bot" || d.Line != tc.line || d.Column != tc.column {
				t.Errorf("%s for %q: at %s:%d:%d, want p.bot:%d:%d", tc.code, tc.src, d.File, d.Line, d.Column, tc.line, tc.column)
			}
			if d.Property != tc.property {
				t.Errorf("%s for %q: property %q, want %q", tc.code, tc.src, d.Property, tc.property)
			}
			switch {
			case tc.property == "":
				if d.EndLine != 0 || d.EndColumn != 0 {
					t.Errorf("%s for %q: a declaration diagnostic closes a span (%d:%d)", tc.code, tc.src, d.EndLine, d.EndColumn)
				}
			case tc.endLine != 0:
				if d.EndLine != tc.endLine || d.EndColumn != tc.endColumn {
					t.Errorf("%s for %q: span ends at %d:%d, want %d:%d", tc.code, tc.src, d.EndLine, d.EndColumn, tc.endLine, tc.endColumn)
				}
			default:
				if d.EndLine < d.Line || d.EndColumn == 0 {
					t.Errorf("%s for %q: span ends at %d:%d, before it starts", tc.code, tc.src, d.EndLine, d.EndColumn)
				}
			}
		}
		if !found {
			t.Errorf("%s: not raised for %q", tc.code, tc.src)
		}
	}
}
