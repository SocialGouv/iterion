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
	refusal := ClawUnwiredSubagentRefusal
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
	remedy := ClawUnwiredSubagentRefusal
	if !strings.Contains(remedy.Remedy, "Agent tool") {
		t.Errorf("the remedy does not name claude_code's current subagent tool (`Agent`): %s", remedy.Remedy)
	}
	if i := strings.Index(remedy.Remedy, "Task"); i >= 0 && !strings.Contains(remedy.Remedy[:i], "legacy") {
		t.Errorf("the remedy names `Task` without marking it the legacy spelling: %s", remedy.Remedy)
	}
}

// TestTheShippedPagesSayTheCapabilityIsServed: the pages are the delivered
// surface — there is no other check between this catalog and what a reader is
// told — and #1940 turned every sentence about claw subagents on its head.
// Each page below must now state the capability, and none may state the stale
// fact that iterion wires no runner: a reader who believes it routes work to
// claude_code it could have run in place, and an author who believes it reads
// a diagnostic that no longer fires.
//
// Two claims stay forbidden on every page:
//
//   - "iterion wires no subagent runner" and its kin. The hosts wire one, in
//     process (pkg/runview) and in the container (cmd/iterion), which is where
//     the placement of `agent` puts it.
//   - "no host wires" — the universal form, which denies the escape hatch
//     TestTheRefusalNamesTheOverrideThatServesIt exists to keep greppable
//     (philosophy #1): wiring the runner is a host's call, and an embedding
//     that wires none earns the typed refusal rather than a stub.
//
// All seven pages that speak of claw subagents are here; a new one joins the
// table.
func TestTheShippedPagesSayTheCapabilityIsServed(t *testing.T) {
	// Claims no page may make any more, each one false since #1940.
	stale := []string{"no host wires", "iterion wires none", "iterion wires no subagent runner"}
	for _, tc := range []struct {
		page      string
		forbidden string
		required  string
	}{
		{
			page:     "docs/agents/backends/prompt-engineering-dials.md",
			required: "iterion wires the subagent runner at both hosts",
		},
		{
			page:      "docs/agents/backends/fallbacks-and-parity.md",
			forbidden: "One registered name is typed-refused",
			required:  "iterion wires one at both of its hosts",
		},
		{
			page:      "docs/backends.md",
			forbidden: "its `agent` tool is registered and **typed-refused**",
			required:  "iterion wires one at both of its hosts",
		},
		{
			page:     "docs/ultracode.md",
			required: "and claw **serves it**",
		},
		{
			page:     "docs/references/diagnostics.md",
			required: "**No name is in that state today**",
		},
		{
			page:     "docs/plugins.md",
			required: "claw serves the `agent` tool",
		},
		{
			page:     "docs/sandbox.md",
			required: "wires the subagent runner there",
		},
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", tc.page))
		if err != nil {
			t.Fatalf("%s: %v", tc.page, err)
		}
		page := collapseProse(b)
		for _, claim := range stale {
			if strings.Contains(page, claim) {
				t.Errorf("%s still states %q, which #1940 made false", tc.page, claim)
			}
		}
		if tc.forbidden != "" && strings.Contains(page, tc.forbidden) {
			t.Errorf("%s still states %q, so a reader is told the capability is refused", tc.page, tc.forbidden)
		}
		if !strings.Contains(page, tc.required) {
			t.Errorf("%s no longer states that claw serves subagents: %q is gone", tc.page, tc.required)
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

// TestTheRefusalCarriesNoBackticks: doc pages and the live witness quote the
// rendered sentence, and a backtick inside it would make every quotation an
// inexact one. The compile-time list is empty, so the unwired-subagent
// refusal is taken explicitly rather than through a loop that would range
// over nothing and pass.
func TestTheRefusalCarriesNoBackticks(t *testing.T) {
	refusals := map[string]ClawRefusal{"the unwired subagent runner": ClawUnwiredSubagentRefusal}
	for _, name := range ClawRefusedBuiltins() {
		r, _ := ClawRefusesBuiltin(name)
		refusals[name] = r
	}
	for name, r := range refusals {
		if strings.Contains(r.String(), "`") {
			t.Errorf("%q: the rendered refusal carries a backtick, so the pages quoting it cannot be verbatim: %s", name, r.String())
		}
	}
}

// TestTheCompileTimeRefusalListIsEmpty is the flip of #1940 on this side: the
// list drives C159, and `agent` left it when the subagent runner was wired at
// both hosts. A name here warns an author about a tool that works.
func TestTheCompileTimeRefusalListIsEmpty(t *testing.T) {
	if names := ClawRefusedBuiltins(); len(names) != 0 {
		t.Fatalf("the compile-time refusal list carries %v; every name claw registers, iterion serves", names)
	}
	if _, refused := ClawRefusesBuiltin("agent"); refused {
		t.Error("`agent` is still refused at compile time, so every claw node declaring it warns about a capability both hosts serve")
	}
}

// TestTheRefusalSeamStillFires: with an empty list, the readers of this
// catalog (C159 in pkg/dsl/ir, the registry side in pkg/backend/tool) are fed
// by nothing, and a mechanism no test can fire is a mechanism nobody knows
// still works. The injection hook is what keeps them exercised, so it owes
// its own guard: it must install, be read back, and restore.
func TestTheRefusalSeamStillFires(t *testing.T) {
	const probe = "web_search"
	if !IsBuiltin(probe) {
		t.Fatalf("%q is not a catalog builtin, so a refusal over it would describe a tool the registry does not hold", probe)
	}
	want := ClawRefusal{Reason: "a probe reason", Remedy: "A probe remedy"}
	restore := SetClawRefusedBuiltinForTest(probe, want)
	got, refused := ClawRefusesBuiltin(probe)
	if !refused || got != want {
		t.Fatalf("the injected refusal reads back as (%+v, %v); want (%+v, true)", got, refused, want)
	}
	if names := ClawRefusedBuiltins(); len(names) != 1 || names[0] != probe {
		t.Errorf("the list reads %v while the probe is installed; want exactly [%s]", names, probe)
	}
	restore()
	if _, refused := ClawRefusesBuiltin(probe); refused {
		t.Errorf("%q is still refused after restore, so a test would leak a refusal into the next one", probe)
	}
	if names := ClawRefusedBuiltins(); len(names) != 0 {
		t.Errorf("the list reads %v after restore; want the empty production list back", names)
	}
}
