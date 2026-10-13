package toolcatalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheRefusalNamesTheOverrideThatServesIt: the refusal is a load-bearing
// limit, and philosophy #1 asks such a limit to keep a GREPPABLE escape hatch.
// Every reader-facing surface renders this one sentence — the model's tool
// error, C159's message, the diagnostics reference — so if it does not name
// the override, nothing delivered does, and an operator cannot tell why the
// same .bot delegates under a host that wires the runner (e2e does) and
// refuses under iterion's own.
func TestTheRefusalNamesTheOverrideThatServesIt(t *testing.T) {
	refusal, refused := ClawRefusesBuiltin("agent")
	if !refused {
		t.Fatal("`agent` is no longer refused; this guard is about the sentence the refusal renders")
	}
	for _, want := range []string{"ClawDefaults.Subagent", "subagent runner"} {
		if !strings.Contains(refusal.String(), want) {
			t.Errorf("the rendered refusal does not name %q: %s", want, refusal.String())
		}
	}
}

// TestTheRemedyNamesClaudeCodesCurrentSubagentTool: the remedy routes the
// author and the model to claude_code, so it has to name the tool they will
// find there. `Task` is the LEGACY SDK spelling (pkg/backend/tooldisplay
// resolves both, pkg/backend/delegate withholds `Agent` and `Task` together,
// and docs/ultracode.md names `Agent` ten lines from where this refusal is
// documented) — naming the legacy one alone sends a reader looking for a tool
// under a name the current CLI no longer advertises.
func TestTheRemedyNamesClaudeCodesCurrentSubagentTool(t *testing.T) {
	remedy, _ := ClawRefusesBuiltin("agent")
	if !strings.Contains(remedy.Remedy, "Agent tool") {
		t.Errorf("the remedy does not name claude_code's current subagent tool (`Agent`): %s", remedy.Remedy)
	}
	if i := strings.Index(remedy.Remedy, "Task"); i >= 0 && !strings.Contains(remedy.Remedy[:i], "legacy") {
		t.Errorf("the remedy names `Task` without marking it the legacy spelling: %s", remedy.Remedy)
	}
}

// TestTheShippedPagesKeepTheRefusalScoped: the refusal carries two scopes, and
// a page that drops either turns a per-deployment fact into a universal one.
//
//   - WHO does not wire the runner. `ClawDefaults.Subagent` is a host's to
//     set, and a host in this tree sets it (e2e/claw_tool_coverage_live_test.go
//     builds one with model.NewSubagentRunner) — so the true sentence is that
//     iterion wires none, never that no host does. The second form denies the
//     escape hatch TestTheRefusalNamesTheOverrideThatServesIt exists to keep
//     greppable (philosophy #1), and no page may state it: that one is checked
//     against every page below, not against the one that happened to say it.
//   - WHERE the compiler can warn. C159 reads the lists the AUTHOR writes; the
//     grant that reaches every ultracode claw node comes from the engine
//     (model.withClawOrchestrationTools), and no diagnostic sees that line. An
//     unqualified "the compiler warns" promises a canary the dominant case
//     never gets.
//
// The pages are the delivered surface — there is no other check between this
// catalog and what a reader is told. All seven that state the refusal are
// here; a new one joins the table.
func TestTheShippedPagesKeepTheRefusalScoped(t *testing.T) {
	// The universal form of the WHO claim, refused on every page below.
	const universalDenial = "no host wires"
	for _, tc := range []struct {
		page      string
		forbidden string
		required  string
	}{
		{
			page:     "docs/agents/backends/prompt-engineering-dials.md",
			required: "iterion wires no subagent runner",
		},
		{
			page:      "docs/agents/backends/fallbacks-and-parity.md",
			forbidden: "the compiler warns first (C159)",
			required:  "where the `.bot` names it",
		},
		{
			page:      "docs/backends.md",
			forbidden: "(compile-time warning: C159)",
			required:  "where a `.bot` names the tool",
		},
		{
			page:     "docs/ultracode.md",
			required: "on a declared `agent`",
		},
		{
			page:     "docs/references/diagnostics.md",
			required: "The tools the ENGINE appends are out of scope",
		},
		{
			page:     "docs/plugins.md",
			required: "no iterion run wires a runner into it",
		},
		{
			page:     "docs/sandbox.md",
			required: "with no subagent runner wired, it refuses every call",
		},
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", tc.page))
		if err != nil {
			t.Fatalf("%s: %v", tc.page, err)
		}
		page := collapseProse(b)
		if strings.Contains(page, universalDenial) {
			t.Errorf("%s states %q, denying the escape hatch a host in this tree uses", tc.page, universalDenial)
		}
		if tc.forbidden != "" && strings.Contains(page, tc.forbidden) {
			t.Errorf("%s states %q, which reads as a fact about every deployment or every call site",
				tc.page, tc.forbidden)
		}
		if !strings.Contains(page, tc.required) {
			t.Errorf("%s no longer scopes the refusal: %q is gone", tc.page, tc.required)
		}
	}
}

// collapseProse renders a page as one line to match a sentence over its wraps.
// Leading blockquote markers go first: docs/sandbox.md states the refusal
// inside a `>` block, where collapsing whitespace alone leaves a `>` token in
// the middle of the sentence and nothing matches.
func collapseProse(b []byte) string {
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimLeft(line, " \t>")
	}
	return strings.Join(strings.Fields(strings.Join(lines, "\n")), " ")
}

// TestTheRefusalCarriesNoBackticks: three doc pages and the live witness quote
// the rendered sentence, and a backtick inside it would make every quotation
// an inexact one.
func TestTheRefusalCarriesNoBackticks(t *testing.T) {
	for _, name := range ClawRefusedBuiltins() {
		r, _ := ClawRefusesBuiltin(name)
		if strings.Contains(r.String(), "`") {
			t.Errorf("%q: the rendered refusal carries a backtick, so the pages quoting it cannot be verbatim: %s", name, r.String())
		}
	}
}
