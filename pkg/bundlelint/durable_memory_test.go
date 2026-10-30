package bundlelint_test

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// durableStoreTools recomputes, from the shared vocabulary, the tools that
// name the durable STORE itself — the members toolcatalog's spelling table
// does not project onto any backend roster. The lint derives the same set the
// same way; recomputing it here rather than restating a literal is what makes
// these tests fail the day the derivation stops naming anything, instead of
// passing over a check that has become a no-op.
func durableStoreTools(t *testing.T) []string {
	t.Helper()
	rows := toolcatalog.CanonicalRows()
	var out []string
	for _, canonical := range toolcatalog.DurableWriteCanonicals() {
		if _, projected := rows[canonical]; !projected {
			out = append(out, canonical)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("no member of toolcatalog.DurableWriteCanonicals names the durable store (every member has a spelling row) — the durable-memory check has no vocabulary left and every case below would pass over a no-op")
	}
	return out
}

// skillPage is a bundle skill the lint can read: the frontmatter the
// routability checks need, plus the body C274 reads for tool names.
func skillPage(body string) []bundlelint.SkillDoc {
	return []bundlelint.SkillDoc{{
		Path:        "skills/session-continuity.md",
		Name:        "session-continuity",
		Description: "How this bot keeps a knowledge tree across sessions.",
		Body:        body,
	}}
}

// agentDeclaring is one LLM node with a declared `tools:` list — a
// declaration, never "no restriction".
func agentDeclaring(id, backend string, tools []string) ir.Node {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Backend: backend},
		Tools:     tools,
	}
}

// durableMemoryDiags runs the whole consistency check over a
// manifest-bearing bundle, the way `iterion validate` does, and returns the
// durable-memory findings alone — through the public entry, so a case here
// also proves the check is reachable from it.
func durableMemoryDiags(w *ir.Workflow, skills []bundlelint.SkillDoc) []bundlelint.Diag {
	diags := bundlelint.CheckConsistency(bundlelint.Input{
		Manifest: &bundle.Manifest{Name: "probe"},
		Workflow: w,
		Skills:   skills,
	})
	var out []bundlelint.Diag
	for _, d := range diags {
		if d.Code == bundlelint.DiagSkillDurableMemoryUnserved {
			out = append(out, d)
		}
	}
	return out
}

// pageTeaching is a skill page that teaches a write with the store's own
// tool, the way bots/evolve's session-continuity page does.
func pageTeaching(tool string) string {
	return "# Session continuity\n\nCall `" + tool + "(path, content)` with YAML frontmatter, and the auto-index picks the file up on the next run.\n"
}

// TestASkillNamingTheStoresWriteToolIsNamedWhereNoNodeCanServeIt is the
// agreement iterion#2832 asks of `iterion validate`: a bundle whose skill
// teaches a durable-memory write, in a bundle where no node can put bytes in
// a memory tree, is named — the skill FILE and the NODE both, so the fix is
// read off the message.
//
// The corrected page is quiet, which is the half that proves the finding is
// the page's and not the bundle's shape.
func TestASkillNamingTheStoresWriteToolIsNamedWhereNoNodeCanServeIt(t *testing.T) {
	for _, tool := range durableStoreTools(t) {
		w := wf("probe", nil, nil, nil, agentDeclaring("nexie", "claude_code", []string{"read_file", "glob", "grep"}))

		got := durableMemoryDiags(w, skillPage(pageTeaching(tool)))
		if len(got) != 1 {
			t.Fatalf("%s: got %d durable-memory diagnostics, want 1: %+v", tool, len(got), got)
		}
		d := got[0]
		if d.Severity != bundlelint.SeverityWarning {
			t.Errorf("%s: severity %v — a skill authoring gap may not change `iterion validate`'s exit code (ARB-98)", tool, d.Severity)
		}
		if d.Field != "skills/session-continuity.md" {
			t.Errorf("%s: field %q, want the skill file", tool, d.Field)
		}
		for _, want := range []string{tool, `node "nexie"`, "claude_code", "read_file"} {
			if !strings.Contains(d.Message, want) {
				t.Errorf("%s: message does not name %q: %s", tool, want, d.Message)
			}
		}
		if !strings.Contains(d.Hint, "memory:") {
			t.Errorf("%s: hint does not name the fix: %s", tool, d.Hint)
		}

		quiet := "# Session continuity\n\nThis bot keeps nothing across sessions: the board is its durable record.\n"
		if got := durableMemoryDiags(w, skillPage(quiet)); len(got) != 0 {
			t.Errorf("%s: the corrected page still draws %+v", tool, got)
		}
	}
}

// TestEveryDurableWriteConceptSilencesTheWarning is the guard on the half a
// hand-written list would break: the capability is the WHOLE shared
// durable-write vocabulary, read from toolcatalog, so a member added there is
// honoured here the day it lands rather than the day someone remembers.
//
// It is also the `memory:`-block narrowing's witness: `bash` is a member
// because the memory tree is a directory under `$ITERION_HOME`, so a
// claude_code node holding the shell DOES keep a durable file — exactly what
// a bash recipe used to tell a bot to do (iterion#2773). A capability half
// narrowed to the `memory:` block would name that bundle, and this case
// reddens on it.
func TestEveryDurableWriteConceptSilencesTheWarning(t *testing.T) {
	store := durableStoreTools(t)
	page := skillPage(pageTeaching(store[0]))
	for _, canonical := range toolcatalog.DurableWriteCanonicals() {
		w := wf("probe", nil, nil, nil, agentDeclaring("worker", "claude_code", []string{"read_file", canonical}))
		got := durableMemoryDiags(w, page)
		// A store builtin is the exception, and it is the question itself: a
		// `tools:` entry does not install it (a `memory:` block does), so a
		// bundle that answered the page with `tools: [memory_write]` has no
		// path to the tree and must still be named.
		if slices.Contains(store, canonical) {
			if len(got) != 1 {
				t.Errorf("a node declaring the store builtin %q in its `tools:` list was read as a path to the tree: got %d diagnostics, want 1", canonical, len(got))
			}
			continue
		}
		if len(got) != 0 {
			t.Errorf("a node declaring %q (a member of toolcatalog.DurableWriteCanonicals) still draws %s: %+v",
				canonical, bundlelint.DiagSkillDurableMemoryUnserved, got)
		}
	}
	// Every member, under a spelling the table folds: the vocabulary is
	// concepts, so `write_file` and `apply_patch` must count as `write` and
	// `edit` do.
	for _, spelling := range []string{"write_file", "apply_patch", "str_replace", "run_command"} {
		canonical := toolcatalog.CanonicalToolName(spelling)
		if !slices.Contains(toolcatalog.DurableWriteCanonicals(), canonical) {
			t.Fatalf("%s folds onto %q, which is no longer a durable-write concept — this case is asserting nothing", spelling, canonical)
		}
		w := wf("probe", nil, nil, nil, agentDeclaring("worker", "claude_code", []string{spelling}))
		if got := durableMemoryDiags(w, page); len(got) != 0 {
			t.Errorf("a node declaring %q (folds onto %q) still draws %+v", spelling, canonical, got)
		}
	}
}

// TestTheTwoOtherPathsToTheTreeSilenceTheWarning covers the declarations that
// are a path without naming a writer: the store's own block, and a list that
// was never declared at all — which on a CLI route means "no restriction",
// so the native roster keeps the shell and the writers.
func TestTheTwoOtherPathsToTheTreeSilenceTheWarning(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))

	// `Write: true` is what the compiler PRODUCES for a block that does not
	// say otherwise (ir.compileMemory defaults read and write to true), so a
	// hand-built block must carry it or it stands for `write: false` — the
	// one shape that is NOT a path. TestCompileMemoryWriteToolInstallation
	// (pkg/dsl/ir) holds that default against the real compiler.
	withBlock := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "evoly"},
		LLMFields: ir.LLMFields{Backend: "claw"},
		Tools:     []string{"read_file"},
		Memory:    &ir.Memory{Enabled: true, Scope: "session-continuity", Read: true, Write: true},
	}
	if got := durableMemoryDiags(wf("probe", nil, nil, nil, withBlock), page); len(got) != 0 {
		t.Errorf("a node carrying a `memory:` block still draws %+v", got)
	}

	undeclared := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}, LLMFields: ir.LLMFields{Backend: "claude_code"}}
	if got := durableMemoryDiags(wf("probe", nil, nil, nil, undeclared), page); len(got) != 0 {
		t.Errorf("a node declaring no `tools:` list still draws %+v", got)
	}
	// `tools: []` IS a declaration — reading it as "no declaration" is the
	// defect that let a node asking for no tools keep the whole roster.
	empty := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}, LLMFields: ir.LLMFields{Backend: "claude_code"}, Tools: []string{}}
	if got := durableMemoryDiags(wf("probe", nil, nil, nil, empty), page); len(got) != 1 {
		t.Errorf("a node declaring `tools: []` was read as undeclared: got %d diagnostics, want 1", len(got))
	}
}

// TestAPageNamingAWorkspaceWriterPromisesNoDurableMemory is the asymmetry the
// check rests on, and the reason the shipped catalogue is quiet: a write to
// the WORKSPACE is not durable memory, so a page documenting the DSL's own
// `bash -c` recipe, or naming the writers a permission gate DENIES, promises
// nothing — bots/whats-next ships both pages.
func TestAPageNamingAWorkspaceWriterPromisesNoDurableMemory(t *testing.T) {
	w := wf("probe", nil, nil, nil, agentDeclaring("nexie", "claude_code", []string{"read_file", "glob", "grep"}))
	pages := map[string]string{
		"the DSL quick reference": "A `command:` runs through `bash -c`, host and sandbox alike. `run_command` is NOT a tool — use `bash`.\n",
		"a refusal":               "The runtime permission gate denies `Bash`, `Write` and `Edit`, so this bundle keeps nothing.\n",
		"an editing recipe":       "Use `write_file` and `apply_patch` to land the change in the worktree.\n",
	}
	for what, body := range pages {
		if got := durableMemoryDiags(w, skillPage(body)); len(got) != 0 {
			t.Errorf("%s draws a durable-memory promise: %+v", what, got)
		}
	}
}

// TestTheToolMustBeNamedAsTheTool holds the reference check to a token: the
// name bounded by characters an identifier cannot carry. A page about
// `memory_writer` or a tenant's `team_memory_write` names neither tool, and a
// call form does.
func TestTheToolMustBeNamedAsTheTool(t *testing.T) {
	tool := durableStoreTools(t)[0]
	w := wf("probe", nil, nil, nil, agentDeclaring("nexie", "claude_code", []string{"read_file"}))

	for _, body := range []string{
		"The `" + tool + "r` role is held by the operator.\n",
		"A tenant calls `team_" + tool + "` through the REST surface.\n",
	} {
		if got := durableMemoryDiags(w, skillPage(body)); len(got) != 0 {
			t.Errorf("a longer identifier containing %q was read as the tool: %q drew %+v", tool, body, got)
		}
	}
	for _, body := range []string{
		"Call `" + tool + "(path, content)`.\n",
		"Call " + strings.ToUpper(tool) + " with the body.\n",
		"- **" + tool + "** — write/replace a file in the scope.\n",
	} {
		if got := durableMemoryDiags(w, skillPage(body)); len(got) != 1 {
			t.Errorf("%q named the tool and drew %d diagnostics, want 1", body, len(got))
		}
	}
}

// TestABundleWithNoReaderAsksNothingOfItsSkills keeps the check off the two
// shapes where it would speak into the void: a workflow with no LLM node (no
// model reads a mirrored skill at all) and a caller that supplied no page
// body (bundlelint is I/O-free — a caller that did not read the bytes asks
// only the frontmatter checks).
func TestABundleWithNoReaderAsksNothingOfItsSkills(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	toolOnly := wf("probe", nil, nil, nil, &ir.ToolNode{BaseNode: ir.BaseNode{ID: "build"}, Command: "make"})
	if got := durableMemoryDiags(toolOnly, page); len(got) != 0 {
		t.Errorf("a workflow with no LLM node drew %+v", got)
	}

	w := wf("probe", nil, nil, nil, agentDeclaring("nexie", "claude_code", []string{"read_file"}))
	noBody := []bundlelint.SkillDoc{{Path: "skills/x.md", Name: "x", Description: "A description long enough to route on cleanly."}}
	if got := durableMemoryDiags(w, noBody); len(got) != 0 {
		t.Errorf("a skill supplied without its body drew %+v", got)
	}
	if got := durableMemoryDiags(nil, page); len(got) != 0 {
		t.Errorf("no workflow at all drew %+v", got)
	}
}

// TestTheWarningNamesEveryReadingNodeBounded keeps the message useful on a
// multi-node bundle: each reading node with what it declares, in a stable
// order, and a count past the fourth rather than an unbounded wall.
func TestTheWarningNamesEveryReadingNodeBounded(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	var nodes []ir.Node
	for i := 0; i < 6; i++ {
		nodes = append(nodes, agentDeclaring(fmt.Sprintf("n%d", i), "claude_code", []string{"read_file"}))
	}
	got := durableMemoryDiags(wf("probe", nil, nil, nil, nodes...), page)
	if len(got) != 1 {
		t.Fatalf("got %d diagnostics, want 1", len(got))
	}
	msg := got[0].Message
	if !strings.Contains(msg, `node "n0"`) || !strings.Contains(msg, `node "n3"`) {
		t.Errorf("the first four nodes are not named: %s", msg)
	}
	if strings.Contains(msg, `node "n4"`) || !strings.Contains(msg, "+2 more") {
		t.Errorf("the list is not bounded at four with a count: %s", msg)
	}
}

// agentWithMemory is one LLM node declaring a `memory:` block beside a
// `tools:` list that names no writer — the shape C319's remedies produce.
func agentWithMemory(id string, mem *ir.Memory) ir.Node {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Backend: "claw"},
		Tools:     []string{"read_file", "glob", "grep"},
		Memory:    mem,
	}
}

// TestAMemoryBlockIsAPathOnlyWhereItInstallsTheWriteTool is the reading
// iterion#2832 asks of the `memory:` arm: a BLOCK is not a write path, the
// write TOOL is. The runtime installs it on `spec.Write && the space is
// writable by a run` (model.installWorkspaceMemory), so the two declarations
// that withhold it must leave the warning on:
//
//   - `write: false` — a first-class property, and the very remedy C319
//     prescribes;
//   - `visibility: "global"` — the instance-wide space no run may write (a
//     run holds no platform authority), which C319 says in words: memory_write
//     is NOT installed for a run. Crediting the block there made C274 and C319
//     contradict each other in the same output.
//
// The positive control is the same block with the write tool kept: quiet.
func TestAMemoryBlockIsAPathOnlyWhereItInstallsTheWriteTool(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	cases := []struct {
		what string
		mem  *ir.Memory
		want int
	}{
		{"write: false (the remedy C319 prescribes)", &ir.Memory{Enabled: true, Scope: "vision", Read: true, Visibility: "bot"}, 1},
		{`visibility: "global" (C319: memory_write is NOT installed for a run)`, &ir.Memory{Enabled: true, Scope: "catalogue", Read: true, Write: true, Visibility: "global"}, 1},
		{"a block that keeps its write tool", &ir.Memory{Enabled: true, Scope: "vision", Read: true, Write: true, Visibility: "bot"}, 0},
		{"a block with no visibility (the legacy per-bot space)", &ir.Memory{Enabled: true, Scope: "vision", Read: true, Write: true}, 0},
	}
	for _, c := range cases {
		got := durableMemoryDiags(wf("probe", nil, nil, nil, agentWithMemory("nexie", c.mem)), page)
		if len(got) != c.want {
			t.Errorf("%s: got %d durable-memory diagnostics, want %d: %+v", c.what, len(got), c.want, got)
			continue
		}
		if c.want == 1 && !strings.Contains(got[0].Message, "memory:") {
			t.Errorf("%s: the message does not say what the block is missing: %s", c.what, got[0].Message)
		}
	}
}

// TestAutoMemoryIsAPathToTheTree closes the false POSITIVE half: `auto_memory:
// on` IS a durable memory, maintained with ordinary file tools the executor
// appends to a restricted claw list (automemory.ClawToolGrant, appended by
// model.ClawExecutor.assembleEffectiveTools) and folded back into the store by
// the mirror. A bundle whose page teaches the store's tool while auto-memory
// is on keeps a tree, so the warning is wrong there.
//
// The mode is resolved as the runtime resolves it (node over workflow). The
// GRANT, not the switch, is what this arm credits, so the route decides:
// `claude_code` serves auto-memory and receives no grant — its node writes
// MEMORY.md with the native roster its `tools:` list bounds, and a
// writer-free list leaves it none. Measured from both ends on the shape
// below: `model.NewProgramExecutor(w).EffectiveToolNames(node, true)` =
// [read_file glob grep] there against [read_file glob grep todo_write
// write_file agent] on claw, and the spawn's own `--disallowedTools` removes
// Write, Edit, MultiEdit, NotebookEdit and Bash for it.
func TestAutoMemoryIsAPathToTheTree(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	build := func(backend, node, workflow string) *ir.Workflow {
		n := &ir.AgentNode{
			BaseNode:   ir.BaseNode{ID: "nexie"},
			LLMFields:  ir.LLMFields{Backend: backend},
			Tools:      []string{"read_file", "glob", "grep"},
			AutoMemory: node,
		}
		w := wf("probe", nil, nil, nil, n)
		w.AutoMemory = workflow
		return w
	}
	cases := []struct {
		what                    string
		backend, node, workflow string
		want                    int
	}{
		{"auto_memory: on, on the node", "claw", "on", "", 0},
		{"auto_memory: on, inherited from the workflow", "claw", "", "on", 0},
		{"auto_memory: on, route undecided (the resolver falls through)", "", "on", "", 0},
		{"auto_memory: on on a serving backend the grant does not reach", "claude_code", "on", "", 1},
		{"the node's off wins over the workflow's on", "claw", "off", "on", 1},
		{"auto_memory: on on a backend that does NOT serve it (C132)", "codex", "on", "", 1},
		{"no auto_memory at all", "claw", "", "", 1},
	}
	for _, c := range cases {
		got := durableMemoryDiags(build(c.backend, c.node, c.workflow), page)
		if len(got) != c.want {
			t.Errorf("%s: got %d durable-memory diagnostics, want %d: %+v", c.what, len(got), c.want, got)
		}
	}
}

// TestAutoMemoryCreditsOnlyTheGrantThatLands is the gate and the list the
// auto-memory arm used to skip (iterion#2832 round 2): the switch was read as
// a write path from the declaration alone, so three shapes that cannot write
// MEMORY.md were credited.
//
// Each case is measured against the engine, not against this arm's own
// reading: `EffectiveToolNames` holds [read_file glob grep todo_write
// write_file agent] on the gated claw node, whose only writer the real gate
// answers `deny` for; it holds NOTHING at all for `tools: []` (the executor's
// append sits behind `len(effectiveTools) > 0`); and on claude_code it holds
// the three readers the list named.
func TestAutoMemoryCreditsOnlyTheGrantThatLands(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	build := func(backend string, tools []string, mode string, allow []string) *ir.Workflow {
		n := &ir.AgentNode{
			BaseNode:   ir.BaseNode{ID: "nexie"},
			LLMFields:  ir.LLMFields{Backend: backend},
			Tools:      tools,
			AutoMemory: "on",
		}
		w := wf("probe", nil, nil, nil, n)
		w.Permission = mode
		w.PermissionAllow = allow
		return w
	}
	restricted := []string{"read_file", "glob", "grep"}
	cases := []struct {
		what    string
		backend string
		tools   []string
		mode    string
		allow   []string
		want    int
	}{
		{"the grant lands and no gate bounds it", "claw", restricted, "", nil, 0},
		{"`permission: deny` with a writer-free allow list denies the granted writer", "claw", restricted, "deny", []string{"Read(**)", "Glob"}, 1},
		{"`permission: deny` whose allow list covers the granted writer", "claw", restricted, "deny", []string{"Write"}, 0},
		{"a declared-EMPTY list receives no grant at all", "claw", []string{}, "", nil, 1},
		{"an UNDECLARED list is the native roster, which the other arm reads", "claw", nil, "", nil, 0},
	}
	for _, c := range cases {
		got := durableMemoryDiags(build(c.backend, c.tools, c.mode, c.allow), page)
		if len(got) != c.want {
			t.Errorf("%s: got %d durable-memory diagnostics, want %d: %+v", c.what, len(got), c.want, got)
		}
	}
}

// TestTheStoresWriteToolIsBoundedByTheGateLikeAnyOther is the `memory:` arm's
// own half of iterion#2832 round 2: the block installs `memory_write`, and
// the gate decides whether a call of it RUNS. It is a gated tool — the
// installer appends it to the generation's tool list and every claw tool_use
// passes the policy first — so a block under `permission: deny` whose allow
// list names no writer buys a tool no call of which runs.
//
// The positive controls keep the arm from being read as "a gate kills every
// block": an allow row covering the store tool, and no gate at all, both stay
// quiet.
func TestTheStoresWriteToolIsBoundedByTheGateLikeAnyOther(t *testing.T) {
	store := durableStoreTools(t)[0]
	page := skillPage(pageTeaching(store))
	build := func(mode string, allow []string) *ir.Workflow {
		w := wf("probe", nil, nil, nil, agentWithMemory("nexie",
			&ir.Memory{Enabled: true, Scope: "vision", Read: true, Write: true, Visibility: "bot"}))
		w.Permission = mode
		w.PermissionAllow = allow
		return w
	}
	cases := []struct {
		what  string
		mode  string
		allow []string
		want  int
	}{
		{"no gate at all", "", nil, 0},
		{"`permission: deny` with a writer-free allow list", "deny", []string{"Read(**)", "Glob"}, 1},
		{"`permission: deny` whose allow list names the store tool", "deny", []string{store}, 0},
		{"`ask` mode: the human may approve", "ask", nil, 0},
	}
	for _, c := range cases {
		got := durableMemoryDiags(build(c.mode, c.allow), page)
		if len(got) != c.want {
			t.Errorf("%s: got %d durable-memory diagnostics, want %d: %+v", c.what, len(got), c.want, got)
		}
	}
	// A bare deny of the store tool is the same bound, written the other way.
	denied := build("ask", nil)
	denied.PermissionDeny = []string{store}
	if got := durableMemoryDiags(denied, page); len(got) != 1 {
		t.Errorf("a bare deny of %q left the block credited: got %d diagnostics, want 1: %+v", store, len(got), got)
	}
}

// TestTheGateIsReadInTheModeThatRuns is the permission arm, exercised in the
// two modes a .bot may actually declare — C110 refuses anything else, so a
// case written in an invented mode exercises nothing:
//
//   - `ask` bounds nothing by itself (the human is asked), so only a rule
//     removes a writer: a bare rule, or a scope that covers every call
//     (`Write(**)`, which permission.Evaluate denies for every path);
//   - `deny` HARD-DENIES everything no allow rule matches
//     (permission.Evaluate → `case ModeDeny: return Deny`), so a declared
//     writer is a path only where an allow (or ask) row covers it.
//
// Each case names a real shape: bots/whats-next and bots/copilot both ship
// `permission: deny` with a writer-free allow list.
func TestTheGateIsReadInTheModeThatRuns(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	build := func(mode string, allow, ask, deny []string) *ir.Workflow {
		w := wf("probe", nil, nil, nil, agentDeclaring("worker", "claude_code", []string{"write_file"}))
		w.Permission = mode
		w.PermissionAllow = allow
		w.PermissionAsk = ask
		w.PermissionDeny = deny
		return w
	}
	cases := []struct {
		what             string
		mode             string
		allow, ask, deny []string
		want             int
	}{
		{"ask + a bare deny of the writer", "ask", nil, nil, []string{"Write"}, 1},
		{"ask + a deny scoped to a subset", "ask", nil, nil, []string{"Write(docs/**)"}, 0},
		{"ask + a deny scoped to every call", "ask", nil, nil, []string{"Write(**)"}, 1},
		{"ask + no rule at all (the human may approve)", "ask", nil, nil, nil, 0},
		{"deny + an allow list naming no writer (whats-next's shape)", "deny", []string{"Read(**)", "Glob"}, nil, nil, 1},
		{"deny + an allow row covering the writer", "deny", []string{"Write(**)"}, nil, nil, 0},
		{"deny + a bare allow of the writer", "deny", []string{"Write"}, nil, nil, 0},
		{"deny + an ask row covering the writer (the run can still ask)", "deny", nil, []string{"Write"}, nil, 0},
		{"deny + an allow row AND a bare deny (deny wins in every mode)", "deny", []string{"Write"}, nil, []string{"Write"}, 1},
		{"deny + no rule at all", "deny", nil, nil, nil, 1},
		{"an unset gate leaves the rule lists inert", "", nil, nil, []string{"Write"}, 0},
		{"off is off", "off", nil, nil, []string{"Write"}, 0},
	}
	for _, c := range cases {
		got := durableMemoryDiags(build(c.mode, c.allow, c.ask, c.deny), page)
		if len(got) != c.want {
			t.Errorf("%s: got %d durable-memory diagnostics, want %d: %+v", c.what, len(got), c.want, got)
		}
	}
}

// TestAnInvalidPermissionModeIsNotThisWarningsDefect keeps the warning off a
// defect another diagnostic owns: a mode no gate can parse is C110's error,
// and a warning that read it as a bound would name the wrong fix.
func TestAnInvalidPermissionModeIsNotThisWarningsDefect(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	w := wf("probe", nil, nil, nil, agentDeclaring("worker", "claude_code", []string{"write_file"}))
	w.Permission = "enforce"
	w.PermissionDeny = []string{"Write"}
	if got := durableMemoryDiags(w, page); len(got) != 0 {
		t.Errorf("an unparseable permission mode (C110's error) was read as a gate: %+v", got)
	}
}

// TestAnUndeclaredToolListIsStillBoundedByTheGate reads the arm that credits
// the ABSENCE of a `tools:` list: on a CLI route the native roster keeps the
// shell and the writers, but the gate still bounds them — under
// `permission: deny` with a writer-free allow list, nothing of that roster
// runs.
func TestAnUndeclaredToolListIsStillBoundedByTheGate(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	build := func(mode string, allow []string) *ir.Workflow {
		n := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}, LLMFields: ir.LLMFields{Backend: "claude_code"}}
		w := wf("probe", nil, nil, nil, n)
		w.Permission = mode
		w.PermissionAllow = allow
		return w
	}
	if got := durableMemoryDiags(build("deny", []string{"Read(**)", "Glob"}), page); len(got) != 1 {
		t.Errorf("an unrestricted roster under `permission: deny` with no writer allowed was read as a write path: got %d diagnostics, want 1", len(got))
	}
	if got := durableMemoryDiags(build("deny", []string{"Bash"}), page); len(got) != 0 {
		t.Errorf("an unrestricted roster whose allow list keeps the shell still draws %+v", got)
	}
	if got := durableMemoryDiags(build("", nil), page); len(got) != 0 {
		t.Errorf("an unrestricted roster under no gate still draws %+v", got)
	}
}

// TestThePermissionPredicateReadsANodeAlone keeps the exported predicate
// usable by the corpus twin and by any later reader with no workflow in hand:
// a nil workflow is the node alone, not a panic, and the node's own mode and
// rule lists still decide.
func TestThePermissionPredicateReadsANodeAlone(t *testing.T) {
	node := &ir.AgentNode{
		BaseNode:   ir.BaseNode{ID: "worker"},
		LLMFields:  ir.LLMFields{Backend: "claude_code"},
		Tools:      []string{"write_file"},
		Permission: "deny",
	}
	if bundlelint.PermissionGateLeaves(node, nil, "write_file") {
		t.Error("a node in `deny` mode with no allow row was read as keeping its writer")
	}
	node.PermissionAllow = []string{"Write(**)"}
	if !bundlelint.PermissionGateLeaves(node, nil, "write_file") {
		t.Error("a node whose own allow row covers the writer was read as denied")
	}
	node.PermissionDeny = []string{"Write"}
	if bundlelint.PermissionGateLeaves(node, nil, "write_file") {
		t.Error("a bare deny row did not win over the allow row")
	}
}
