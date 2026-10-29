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

	withBlock := &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "evoly"},
		LLMFields: ir.LLMFields{Backend: "claw"},
		Tools:     []string{"read_file"},
		Memory:    &ir.Memory{Enabled: true, Scope: "session-continuity"},
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

// TestADeniedWriterIsNoPathToTheTree reads the gate that bounds what RUNS: a
// bundle whose policy denies the writer its own list names has no write path,
// and a SCOPED rule bounds a subset of the calls rather than removing the
// tool.
func TestADeniedWriterIsNoPathToTheTree(t *testing.T) {
	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	build := func(mode string, deny []string) *ir.Workflow {
		w := wf("probe", nil, nil, nil, agentDeclaring("worker", "claude_code", []string{"write_file"}))
		w.Permission = mode
		w.PermissionDeny = deny
		return w
	}
	if got := durableMemoryDiags(build("enforce", []string{"Write"}), page); len(got) != 1 {
		t.Errorf("a denied `write_file` was still read as a write path: got %d diagnostics, want 1", len(got))
	}
	if got := durableMemoryDiags(build("enforce", []string{"Write(docs/**)"}), page); len(got) != 0 {
		t.Errorf("a SCOPED deny removed the tool outright: %+v", got)
	}
	// An unset gate leaves the rule lists inert: read as "not denied", never
	// guessed.
	if got := durableMemoryDiags(build("", []string{"Write"}), page); len(got) != 0 {
		t.Errorf("a deny rule under an unset gate removed the tool: %+v", got)
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
