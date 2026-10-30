package bundlelint_test

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
	"github.com/SocialGouv/iterion/pkg/bundlelint"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// gateCase is one rule FORM of the permission grammar, asked of the lint and
// of the gate at once.
//
// call and inputs are the DISPATCH side, written here rather than derived:
// what reaches the gate is the name the backend's model emits — `Write` on a
// claude_code route whatever spelling the `tools:` list used, `write_file` on
// claw — and the argument keys the gate summarises (`file_path` for the
// file-writing family, `command` for the shell, the store builtin's own pair).
// A case that re-derived them from the lint's projection could not catch the
// projection being wrong, which is the defect this table exists for.
type gateCase struct {
	form             string // the grammar form this row exercises
	what             string
	backend          string // the node's primary route
	fallback         string // a `fallbacks:` route, "" for none
	declared         string // the spelling the `tools:` list carries
	call             string // the name the gate sees at dispatch
	inputs           []map[string]any
	mode             string
	allow, ask, deny []string
}

// gateCases enumerate the grammar of pkg/backend/permission — every rule
// shape parseRule accepts — against both routes whose spellings differ.
//
// Read off the parser, not off a bug report: a bare row, a tool CLASS row, a
// tool GLOB (in each route's own spelling, which is the pair that diverged),
// an argument SCOPE bounding a subset and one bounding every call, the
// catch-all, the `:*` prefix idiom with an empty prefix, and the two modes
// with their allow/ask rows — plus `off` and unset, where the lists are
// inert.
func gateCases() []gateCase {
	writePaths := []map[string]any{
		{"file_path": "/w/notes.md"},
		{"file_path": "/w/docs/notes.md"},
		{"file_path": "/elsewhere/y.txt"},
	}
	shellCmds := []map[string]any{
		{"command": "echo hi >> /w/notes.md"},
		{"command": "git diff"},
	}
	storeCalls := []map[string]any{{"path": "notes.md", "content": "x"}}
	cc := func(c gateCase) gateCase {
		c.backend, c.declared, c.call, c.inputs = "claude_code", "write_file", "Write", writePaths
		return c
	}
	claw := func(c gateCase) gateCase {
		c.backend, c.declared, c.call, c.inputs = "claw", "write_file", "write_file", writePaths
		return c
	}
	return []gateCase{
		// No gate: the lists are inert whatever they say.
		cc(gateCase{form: "unset mode", what: "a deny row under no mode at all", mode: "", deny: []string{"Write"}}),
		cc(gateCase{form: "off mode", what: "a deny row under `off`", mode: "off", deny: []string{"Write"}}),

		// A bare row, in each mode.
		cc(gateCase{form: "bare row", what: "`ask` + a bare deny of the writer", mode: "ask", deny: []string{"Write"}}),
		cc(gateCase{form: "bare row", what: "`deny` + a bare allow of the writer", mode: "deny", allow: []string{"Write"}}),
		cc(gateCase{form: "bare row", what: "`deny` + an allow list naming no writer", mode: "deny", allow: []string{"Read(**)", "Glob"}}),
		cc(gateCase{form: "bare row", what: "`deny` + an ask row covering the writer", mode: "deny", ask: []string{"Write"}}),
		cc(gateCase{form: "bare row", what: "`deny` + an allow row AND a bare deny", mode: "deny", allow: []string{"Write"}, deny: []string{"Write"}}),
		cc(gateCase{form: "bare row", what: "`ask` + no rule at all", mode: "ask"}),
		cc(gateCase{form: "bare row", what: "`deny` + no rule at all", mode: "deny"}),

		// An argument scope: a subset leaves the other calls, every call does not.
		cc(gateCase{form: "arg scope (subset)", what: "`ask` + a deny scoped to a subset", mode: "ask", deny: []string{"Write(/w/docs/**)"}}),
		cc(gateCase{form: "arg scope (every call)", what: "`ask` + a deny scoped to every path", mode: "ask", deny: []string{"Write(**)"}}),
		cc(gateCase{form: "arg scope (every call)", what: "`ask` + a deny scoped with the single star", mode: "ask", deny: []string{"Write(*)"}}),
		cc(gateCase{form: "arg scope (subset)", what: "`deny` + an allow scoped to a subset", mode: "deny", allow: []string{"Write(/w/docs/**)"}}),

		// The catch-all tool row.
		cc(gateCase{form: "catch-all", what: "`deny` + a catch-all allow", mode: "deny", allow: []string{"*"}}),
		cc(gateCase{form: "catch-all", what: "`ask` + a catch-all deny", mode: "ask", deny: []string{"*"}}),

		// A tool GLOB, in each route's own spelling — the pair that diverged.
		cc(gateCase{form: "tool glob (native spelling)", what: "`deny` + allow: [\"Write*\"] on a claude_code route", mode: "deny", allow: []string{"Write*"}}),
		cc(gateCase{form: "tool glob (native spelling)", what: "`ask` + deny: [\"Write*\"] on a claude_code route", mode: "ask", deny: []string{"Write*"}}),
		cc(gateCase{form: "tool glob (catalogue spelling)", what: "`deny` + allow: [\"write_*\"] on a claude_code route, which never sends that name", mode: "deny", allow: []string{"write_*"}}),
		cc(gateCase{form: "tool glob (catalogue spelling)", what: "`ask` + deny: [\"write_*\"] on a claude_code route, which never sends that name", mode: "ask", deny: []string{"write_*"}}),
		claw(gateCase{form: "tool glob (catalogue spelling)", what: "`deny` + allow: [\"write_*\"] on a claw route", mode: "deny", allow: []string{"write_*"}}),
		claw(gateCase{form: "tool glob (catalogue spelling)", what: "`ask` + deny: [\"write_*\"] on a claw route", mode: "ask", deny: []string{"write_*"}}),
		claw(gateCase{form: "tool glob (native spelling)", what: "`deny` + allow: [\"Write*\"] on a claw route, which never sends that name", mode: "deny", allow: []string{"Write*"}}),
		claw(gateCase{form: "tool glob (native spelling)", what: "`ask` + deny: [\"Write*\"] on a claw route, which never sends that name", mode: "ask", deny: []string{"Write*"}}),

		// A tool CLASS row, read from the registry's capability metadata.
		{form: "class row", what: "`ask` + deny: [\"class:shell\"] over the shell", backend: "claude_code",
			declared: "bash", call: "Bash", inputs: shellCmds, mode: "ask", deny: []string{"class:shell"}},
		{form: "class row", what: "`deny` + allow: [\"class:shell\"] over the shell", backend: "claude_code",
			declared: "bash", call: "Bash", inputs: shellCmds, mode: "deny", allow: []string{"class:shell"}},
		{form: "class row (scoped)", what: "`ask` + a class deny scoped to one command family", backend: "claude_code",
			declared: "bash", call: "Bash", inputs: shellCmds, mode: "ask", deny: []string{"class:shell(git diff:*)"}},

		// The prefix idiom, with a prefix and with none.
		{form: "prefix idiom", what: "`ask` + deny: [\"Bash(git diff:*)\"]", backend: "claude_code",
			declared: "bash", call: "Bash", inputs: shellCmds, mode: "ask", deny: []string{"Bash(git diff:*)"}},
		{form: "prefix idiom (empty prefix)", what: "`ask` + deny: [\"Bash(:*)\"], which matches every command", backend: "claude_code",
			declared: "bash", call: "Bash", inputs: shellCmds, mode: "ask", deny: []string{"Bash(:*)"}},

		// A `fallbacks:` route the node may take. The verdict is read over
		// every route — a route that keeps the writer is a run that writes —
		// so the probe is that route's own spelling: `write_*` covers nothing
		// a claude_code spawn sends, and everything the claw fallback does.
		{form: "fallback route", what: "`deny` + allow: [\"write_*\"], reached on the claw fallback", backend: "claude_code", fallback: "claw",
			declared: "write_file", call: "write_file", inputs: writePaths, mode: "deny", allow: []string{"write_*"}},

		// The store builtin, which no roster projects: the name as declared
		// is the name the gate sees.
		{form: "store builtin", what: "`deny` + an allow list naming no writer, over the store tool", backend: "claw",
			declared: "memory_write", call: "memory_write", inputs: storeCalls, mode: "deny", allow: []string{"Read(**)", "Glob"}},
		{form: "store builtin", what: "`deny` + an allow row naming the store tool", backend: "claw",
			declared: "memory_write", call: "memory_write", inputs: storeCalls, mode: "deny", allow: []string{"memory_write"}},
		{form: "store builtin", what: "`ask` + a bare deny of the store tool", backend: "claw",
			declared: "memory_write", call: "memory_write", inputs: storeCalls, mode: "ask", deny: []string{"memory_write"}},
	}
}

// TestTheLintsGateQuestionAgreesWithTheGateOverEveryRuleForm is the witness
// iterion#2832 round 2 asks for: the question bundlelint asks of the
// permission gate, compared with the GATE — permission.Policy.Evaluate, built
// the way model.ClawExecutor.resolvePermissionPolicy builds it — case by
// case, over every rule form of the grammar, in BOTH directions.
//
//   - the lint says the tool is LEFT: at least one call of it must reach a
//     verdict other than Deny, or the lint credits a capability no call has
//     (the false negative that let C274 fall silent on bundles with no write
//     path, iterion#2832);
//   - the lint says the tool is GONE: EVERY call must be denied, or the lint
//     bounds a tool that runs (the false positive a tool glob produced —
//     C274 fired on a bundle whose writer the gate allows).
//
// A rule form present in the parser and absent here is the next divergence,
// which is why the forms are named and their coverage asserted: a row's
// `form` is the shape it exercises, and the floor below refuses a table that
// stopped covering one.
func TestTheLintsGateQuestionAgreesWithTheGateOverEveryRuleForm(t *testing.T) {
	t.Parallel()

	wantForms := []string{
		"arg scope (every call)", "arg scope (subset)", "bare row", "catch-all",
		"class row", "class row (scoped)", "fallback route", "off mode", "prefix idiom",
		"prefix idiom (empty prefix)", "store builtin", "tool glob (catalogue spelling)",
		"tool glob (native spelling)", "unset mode",
	}
	seen := map[string]bool{}
	leftCount, goneCount := 0, 0
	for _, c := range gateCases() {
		seen[c.form] = true
		node := &ir.AgentNode{
			BaseNode:  ir.BaseNode{ID: "worker"},
			LLMFields: ir.LLMFields{Backend: c.backend},
			Tools:     []string{c.declared},
		}
		if c.fallback != "" {
			node.Fallbacks = []ir.Fallback{{Name: "second", Backend: c.fallback}}
		}
		w := wf("probe", nil, nil, nil, node)
		w.Permission, w.PermissionAllow, w.PermissionAsk, w.PermissionDeny = c.mode, c.allow, c.ask, c.deny

		lintLeaves := bundlelint.PermissionGateLeaves(node, w, c.declared)

		// The gate, built from the same declaration and asked about the call
		// the backend sends.
		var verdicts []string
		gateLeaves := false
		if c.mode == "" || c.mode == "off" {
			// An unset or `off` gate is not built at all by the runtime
			// (resolvePermissionPolicy returns a disabled policy), so there
			// is nothing to evaluate: every call runs.
			gateLeaves = true
			verdicts = append(verdicts, "gate disabled")
		} else {
			mode, err := permission.ParseMode(c.mode)
			if err != nil {
				t.Fatalf("%s: the case names a mode no gate parses (%q): %v", c.what, c.mode, err)
			}
			pol, err := permission.NewPolicy(mode, c.allow, c.ask, c.deny)
			if err != nil {
				t.Fatalf("%s: the case names a rule the gate refuses: %v", c.what, err)
			}
			for _, in := range c.inputs {
				dec, rule := pol.Evaluate(c.call, in)
				verdicts = append(verdicts, fmt.Sprintf("%s(%v) -> %s (rule %q)", c.call, in, dec, rule))
				if dec != permission.Deny {
					gateLeaves = true
				}
			}
		}
		if lintLeaves {
			leftCount++
		} else {
			goneCount++
		}
		if lintLeaves != gateLeaves {
			direction := "the lint credits a tool the gate denies on every call"
			if gateLeaves {
				direction = "the lint bounds a tool the gate lets run"
			}
			t.Errorf("[%s] %s: lint_leaves=%v gate_leaves=%v — %s\n  gate: %s",
				c.form, c.what, lintLeaves, gateLeaves, direction, strings.Join(verdicts, "\n        "))
		}
	}

	var missing []string
	for _, form := range wantForms {
		if !seen[form] {
			missing = append(missing, form)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("the table no longer covers %d rule form(s) of the grammar — a form nothing exercises is the next divergence: %s",
			len(missing), strings.Join(missing, ", "))
	}
	// A table that only ever agreed because it never saw a bound (or never
	// saw a capability) would pass over nothing.
	if leftCount == 0 || goneCount == 0 {
		t.Fatalf("the table reached one verdict only (%d left, %d gone) — it cannot witness an agreement in both directions", leftCount, goneCount)
	}
	t.Logf("%d rule forms, %d cases: %d left, %d gone", len(seen), leftCount+goneCount, leftCount, goneCount)
}

// TestTheUndeclaredRosterArmAsksTheGateInTheRoutesSpelling pins the half of
// the gate question that was asserted over nothing until iterion#2832 round 3:
// a node whose `tools:` list bounds nothing keeps its ROUTE's own roster, and
// the gate must be asked about the names that route SENDS. The arm used to
// hand it toolcatalog.DurableWriteCanonicals — the concept keys "write",
// "edit", "notebookedit", "bash", which no backend emits. A claude_code-only
// route hid that, because the projection renames the key through that route's
// roster; one route whose roster the registry leaves empty puts the raw key
// back in the offer, and `deny: ["Write*"]` then reads as leaving a writer it
// removes.
//
// Every row is compared with the GATE — permission.Policy.Evaluate over the
// calls claude_code actually sends for the writers of its roster — so the
// verdict is never this arm's reading of itself. The claw-fallback row is the
// one that was silent before: that route's surface is its `tools:` list, so
// an undeclared list holds nothing there and takes no writer with it.
func TestTheUndeclaredRosterArmAsksTheGateInTheRoutesSpelling(t *testing.T) {
	t.Parallel()

	// The names claude_code sends for a durable write, from the registry's own
	// roster — never a literal, so a roster change is read here the day it
	// lands instead of the day someone remembers.
	natives := toolcatalog.DurableWriteSpellingsOn("claude_code")
	if len(natives) == 0 {
		t.Fatal("the registry declares no durable writer on claude_code's roster — the arm this test is about credits nothing and every row below would pass over a no-op")
	}
	writePaths := []map[string]any{
		{"file_path": "/w/notes.md", "content": "x"},
		{"file_path": "/w/docs/notes.md", "content": "x"},
		{"file_path": "relative.md", "content": "x"},
	}
	shellCmds := []map[string]any{{"command": "echo hi >> /w/notes.md"}, {"command": "git diff"}}
	callsFor := func(native string) []map[string]any {
		if toolcatalog.CanonicalToolName(native) == "bash" {
			return shellCmds
		}
		return writePaths
	}

	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	cases := []struct {
		what             string
		fallback         string
		mode             string
		allow, ask, deny []string
	}{
		{what: "no gate at all"},
		{what: "`ask` + a deny of every writer of the route's roster", mode: "ask",
			deny: []string{"Write*", "Edit*", "MultiEdit", "NotebookEdit", "Bash"}},
		{what: "the same deny list, plus a claw `fallbacks:` route whose roster the registry leaves empty", fallback: "claw", mode: "ask",
			deny: []string{"Write*", "Edit*", "MultiEdit", "NotebookEdit", "Bash"}},
		{what: "`ask` + a deny list written in CATALOGUE spellings the route never sends", mode: "ask",
			deny: []string{"write_*", "edit_*", "notebook_*", "run_*"}},
		{what: "`deny` + an allow list naming no writer", mode: "deny", allow: []string{"Read(**)", "Glob"}},
		{what: "`deny` + an allow row naming the route's own writer", mode: "deny", allow: []string{"Write"}},
		{what: "`deny` + an ask row covering the route's own writer", mode: "deny", ask: []string{"Write"}},
	}
	quiet, named := 0, 0
	for _, c := range cases {
		node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "nexie"}, LLMFields: ir.LLMFields{Backend: "claude_code"}}
		if c.fallback != "" {
			node.Fallbacks = []ir.Fallback{{Name: "second", Backend: c.fallback}}
		}
		if node.Tools != nil {
			t.Fatalf("%s: the probe node declares a `tools:` list — the undeclared-list arm is not the one answering", c.what)
		}
		w := wf("probe", nil, nil, nil, node)
		w.Permission, w.PermissionAllow, w.PermissionAsk, w.PermissionDeny = c.mode, c.allow, c.ask, c.deny

		// The gate: does any call of any writer of claude_code's roster reach
		// a verdict other than Deny?
		gateLeaves := c.mode == "" || c.mode == "off"
		var verdicts []string
		if !gateLeaves {
			mode, err := permission.ParseMode(c.mode)
			if err != nil {
				t.Fatalf("%s: the case names a mode no gate parses (%q): %v", c.what, c.mode, err)
			}
			pol, err := permission.NewPolicy(mode, c.allow, c.ask, c.deny)
			if err != nil {
				t.Fatalf("%s: the case names a rule the gate refuses: %v", c.what, err)
			}
			for _, native := range natives {
				for _, in := range callsFor(native) {
					dec, rule := pol.Evaluate(native, in)
					verdicts = append(verdicts, fmt.Sprintf("%s(%v) -> %s (rule %q)", native, in, dec, rule))
					if dec != permission.Deny {
						gateLeaves = true
					}
				}
			}
		}

		got := durableMemoryDiags(w, page)
		lintQuiet := len(got) == 0
		if lintQuiet {
			quiet++
		} else {
			named++
		}
		if lintQuiet != gateLeaves {
			direction := "the warning credits a write path no call of the route's roster can take"
			if gateLeaves {
				direction = "the warning denies a write path the gate lets run"
			}
			t.Errorf("%s: lint_quiet=%v gate_leaves=%v — %s\n  diags: %+v\n  gate: %s",
				c.what, lintQuiet, gateLeaves, direction, got, strings.Join(verdicts, "\n        "))
		}
	}
	if quiet == 0 || named == 0 {
		t.Fatalf("the table reached one verdict only (%d quiet, %d named) — it cannot witness an agreement in both directions", quiet, named)
	}
}

// TestAnUnparseableRuleIsSkippedRowByRowAndNotAsAWholeGate is iterion#2832
// round 3: permission.NewPolicy fails on the FIRST malformed row of a list, so
// a gate question that built ONE policy and read the error as "no gate at all"
// turned five honoured deny rows into none, and credited every writer those
// five rows remove.
//
// A row the grammar refuses is C111's ERROR, and reading it as ABSENT is this
// warning's job — not reading the gate as absent. The rest of the list is what
// the runtime enforces, and that is what the row below is measured against.
func TestAnUnparseableRuleIsSkippedRowByRowAndNotAsAWholeGate(t *testing.T) {
	t.Parallel()

	honoured := []string{"Write*", "Edit*", "MultiEdit", "NotebookEdit", "Bash"}
	malformed := "Grep("
	if permission.ValidateRule(malformed) == nil {
		t.Fatalf("%q parses now — this case carries no rule the gate refuses and asserts nothing", malformed)
	}
	if _, err := permission.NewPolicy(permission.ModeAsk, nil, nil, append(slices.Clone(honoured), malformed)); err == nil {
		t.Fatal("permission.NewPolicy accepts a list with a malformed row now — the whole-gate discard this test is about cannot happen, and the comparison below is a no-op")
	}

	page := skillPage(pageTeaching(durableStoreTools(t)[0]))
	build := func(deny []string) (*ir.AgentNode, *ir.Workflow) {
		node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "nexie"}, LLMFields: ir.LLMFields{Backend: "claude_code"}}
		w := wf("probe", nil, nil, nil, node)
		w.Permission, w.PermissionDeny = "ask", deny
		return node, w
	}

	cleanNode, clean := build(honoured)
	if bundlelint.PermissionGateLeaves(cleanNode, clean, "Write") {
		t.Fatal("the five deny rows alone do not bound `Write` — the baseline of this case is wrong, and the row below would prove nothing")
	}
	if got := durableMemoryDiags(clean, page); len(got) != 1 {
		t.Fatalf("the five deny rows remove every writer of the route's roster: got %d durable-memory diagnostics, want 1: %+v", len(got), got)
	}

	badNode, bad := build(append(slices.Clone(honoured), malformed))
	if bundlelint.PermissionGateLeaves(badNode, bad, "Write") {
		t.Error("one row the grammar refuses read the five honoured deny rows as no gate at all: `Write` was credited as left")
	}
	if got := durableMemoryDiags(bad, page); len(got) != 1 {
		t.Errorf("one unparseable row beside five honoured ones credited a write path the gate denies on every call: got %d durable-memory diagnostics, want 1: %+v", len(got), got)
	}
}
