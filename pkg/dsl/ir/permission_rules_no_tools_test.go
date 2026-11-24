package ir

import (
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// C351: a node's own allow/ask/deny lists on a claw route that serves it no
// tool. Each witness compiles `.bot` TEXT and reads the diagnostics the
// compiler emits, so the parser's carry, the compiler's defaults (a
// `memory:` block's read/write, a workflow's codemode) and the validator are
// all on the path.

// noToolsBot scaffolds one LLM node of the given kind and one workflow. node
// is the node's body after its model line, workflow the workflow's body
// before its edge; both carry their own indentation and newlines.
func noToolsBot(kind, node, workflow string) string {
	return kind + " a:\n  model: \"m\"\n" + node +
		"workflow w:\n  entry: a\n" + workflow + "  a -> done\n"
}

// outSchema declares the schema a case's node names with `output: out`.
const outSchema = "schema out:\n  ok: bool\n"

// rulesNoTools returns the C351 findings of a compile.
func rulesNoTools(r *CompileResult) []Diagnostic {
	var out []Diagnostic
	for _, d := range r.Diagnostics {
		if d.Code == DiagPermissionRulesNoTools {
			out = append(out, d)
		}
	}
	return out
}

// TestRulesOnAToolLessClawRouteWarn is the fire set: the node declares a
// list of its own, a route the source sends to claw exists, and nothing the
// node declares gives that route a tool.
func TestRulesOnAToolLessClawRouteWarn(t *testing.T) {
	const gated = "  permission: deny\n"
	const clawFallback = "  fallbacks:\n    cl:\n      backend: \"claw\"\n      model: \"m2\"\n      on: [unavailable]\n"
	for _, tc := range []struct {
		name string
		src  string
		// want are fragments the message must carry: the node, the route,
		// the lists and the surface it names. absent are fragments it must
		// not: a route it has no business naming.
		want   []string
		absent []string
	}{
		{
			// The incident: allow:/deny: on a gated claw agent, no tools.
			"gated claw agent",
			noToolsBot("agent", "  backend: \"claw\"\n  permission: deny\n  allow: [\"Read(**)\", \"Grep(**)\"]\n  deny: [\"Bash\"]\n", ""),
			[]string{`agent "a"`, "allow:/deny:", "no `tools:` list", "primary route runs on claw", "nothing is callable", "the rules bound nothing"}, nil,
		},
		{
			// The defect is the surface, whatever arms the rules: with the
			// gate off C111 speaks too, and its fix alone leaves this one.
			"gate off",
			noToolsBot("agent", "  backend: \"claw\"\n  allow: [\"Read(**)\"]\n", ""),
			[]string{`agent "a"`, "allow:", "primary route"}, nil,
		},
		{
			"judge",
			noToolsBot("judge", "  backend: \"claw\"\n  deny: [\"Bash\"]\n", gated),
			[]string{`judge "a"`, "deny:", "primary route"}, nil,
		},
		{
			"ask list alone",
			noToolsBot("agent", "  backend: \"claw\"\n  ask: [\"Bash\"]\n", gated),
			[]string{"ask:", "primary route"}, nil,
		},
		{
			// The route is the workflow's to name when the node names none.
			"workflow default_backend",
			noToolsBot("agent", "  deny: [\"Bash\"]\n", gated+"  default_backend: \"claw\"\n"),
			[]string{"primary route"}, nil,
		},
		{
			// A dial is read by its default, as on every backend screen.
			"a dial defaulting to claw",
			noToolsBot("agent", "  backend: \"${C351_UNSET:-claw}\"\n  deny: [\"Bash\"]\n", gated),
			[]string{"primary route"}, nil,
		},
		{
			// claude_code keeps its roster, but the route that serves when it
			// fails is claw: nothing is callable at that moment. (With no
			// `tools:` line the crossing itself is C176's refusal.)
			"claude_code with a claw fallback",
			noToolsBot("agent", "  backend: \"claude_code\"\n  deny: [\"Bash\"]\n"+clawFallback, gated),
			[]string{"no `tools:` list", `fallback route "cl" runs on claw`}, nil,
		},
		{
			// The crossing C176 admits — a declared list says the same thing
			// on both sides — is the one where this warning stands alone.
			"claude_code with tools: [] and a claw fallback",
			noToolsBot("agent", "  backend: \"claude_code\"\n  tools: []\n  deny: [\"Bash\"]\n"+clawFallback, gated),
			[]string{"an empty `tools: []`", `fallback route "cl" runs on claw`}, nil,
		},
		{
			// A declared-empty list is the same surface, named as written.
			"tools: [] declared",
			noToolsBot("agent", "  backend: \"claw\"\n  tools: []\n  deny: [\"Bash\"]\n", gated),
			[]string{"an empty `tools: []`", "primary route"}, nil,
		},
		{
			// A capability grant rides a non-empty list and adds nothing to
			// an empty one.
			"capabilities create no surface",
			noToolsBot("agent", "  backend: \"claw\"\n  capabilities: [board.read]\n  deny: [\"Bash\"]\n", gated),
			[]string{"primary route"}, nil,
		},
		{
			// Nor do MCP servers: their wildcards are spliced into a list
			// that already holds a tool.
			"mcp servers create no surface",
			"mcp_server s:\n  transport: stdio\n  command: \"x\"\n" +
				noToolsBot("agent", "  backend: \"claw\"\n  mcp:\n    servers: [s]\n  deny: [\"Bash\"]\n", gated),
			[]string{"primary route"}, nil,
		},
		{
			// A block that installs no store tool is no surface.
			"memory block installing nothing",
			noToolsBot("agent", "  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    read: false\n    write: false\n  deny: [\"Bash\"]\n", gated),
			[]string{"primary route"}, nil,
		},
		{
			// The node's own `off` beats the workflow's `on`.
			"codemode off on the node",
			noToolsBot("agent", "  backend: \"claw\"\n  codemode: off\n  deny: [\"Bash\"]\n", gated+"  codemode: on\n"),
			[]string{"primary route"}, nil,
		},
		{
			// Reported against the FIRST claw route, in the order the chain
			// walks them — past a route that is not claw, before a second
			// one that is.
			"the first of two claw fallbacks",
			noToolsBot("agent", "  backend: \"claude_code\"\n  tools: []\n  deny: [\"Bash\"]\n  fallbacks:\n"+
				"    other:\n      backend: \"claude_code\"\n      model: \"m2\"\n      on: [unavailable]\n"+
				"    first:\n      backend: \"claw\"\n      model: \"m3\"\n      on: [unavailable]\n"+
				"    second:\n      backend: \"claw\"\n      model: \"m4\"\n      on: [unavailable]\n", gated),
			[]string{`fallback route "first" runs on claw`}, []string{`"second"`, `"other"`},
		},
		{
			// The write tool is withheld from a run on the instance-wide
			// space, so a write-only block there installs nothing.
			"memory write alone on the global space",
			noToolsBot("agent", "  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    read: false\n    visibility: \"global\"\n  deny: [\"Bash\"]\n", gated),
			[]string{"primary route"}, nil,
		},
		{
			// The store's tools reach the model on the text path alone: a
			// node that declares an output schema and no tool loop answers
			// through the structured path, which sends none of them.
			"memory block beside an output schema",
			outSchema + noToolsBot("agent", "  backend: \"claw\"\n  output: out\n  memory:\n    enabled: true\n    scope: \"s\"\n  deny: [\"Bash\"]\n", gated),
			[]string{"primary route", "nothing is callable"}, nil,
		},
		{
			"memory read tools beside an output schema",
			outSchema + noToolsBot("agent", "  backend: \"claw\"\n  output: out\n  memory:\n    enabled: true\n    scope: \"s\"\n    write: false\n  deny: [\"Bash\"]\n", gated),
			[]string{"primary route"}, nil,
		},
		{
			"memory write tool beside an output schema",
			outSchema + noToolsBot("agent", "  backend: \"claw\"\n  output: out\n  memory:\n    enabled: true\n    scope: \"s\"\n    read: false\n  deny: [\"Bash\"]\n", gated),
			[]string{"primary route"}, nil,
		},
		{
			// A group member is a node like another, under its instance id.
			"group member",
			"group g(p):\n  agent m:\n    model: \"m\"\n    backend: \"claw\"\n    deny: [\"Bash\"]\nuse g as x with { p: \"v\" }\n" +
				"workflow w:\n  entry: x.m\n  permission: deny\n  x.m -> done\n",
			[]string{`agent "x.m"`, "primary route"}, nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := rulesNoTools(compileText(t, tc.src))
			if len(got) != 1 {
				t.Fatalf("C351 fired %d time(s), want once: %+v", len(got), got)
			}
			if got[0].Severity != SeverityWarning {
				t.Errorf("severity %v, want a warning: the node still runs", got[0].Severity)
			}
			for _, want := range tc.want {
				if !strings.Contains(got[0].Message, want) {
					t.Errorf("message does not carry %q: %s", want, got[0].Message)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got[0].Message, absent) {
					t.Errorf("message names %s, which is not the route it reports: %s", absent, got[0].Message)
				}
			}
		})
	}
}

// TestRulesWithoutToolsStaySilentWhereTheShapeIsLegitimate is the other
// half: every arm that takes one of the three conditions away.
func TestRulesWithoutToolsStaySilentWhereTheShapeIsLegitimate(t *testing.T) {
	const gated = "  permission: deny\n"
	const rules = "  deny: [\"Bash\"]\n"
	for _, tc := range []struct{ name, src string }{
		// No list of the node's OWN.
		{"rules inherited from the workflow", noToolsBot("agent", "  backend: \"claw\"\n", gated+"  allow: [\"Read(**)\"]\n  deny: [\"Bash\"]\n")},
		{"no rules at all", noToolsBot("agent", "  backend: \"claw\"\n", gated)},

		// No route the source sends to claw.
		{"claude_code: no tools: line is the native roster", noToolsBot("agent", "  backend: \"claude_code\"\n"+rules, gated)},
		{"codex", noToolsBot("agent", "  backend: \"codex\"\n"+rules, gated)},
		{"claude_code with a claude_code fallback", noToolsBot("agent", "  backend: \"claude_code\"\n"+rules+"  fallbacks:\n    cc:\n      backend: \"claude_code\"\n      model: \"m2\"\n      on: [unavailable]\n", gated)},
		{"no backend named anywhere", noToolsBot("agent", rules, gated)},
		{"backend: auto", noToolsBot("agent", "  backend: \"auto\"\n"+rules, gated)},
		{"a dial nothing answers", noToolsBot("agent", "  backend: \"${C351_UNSET}\"\n"+rules, gated)},
		{"a vars-routed backend", "vars:\n  b: string = \"claw\"\n" + noToolsBot("agent", "  backend: \"{{vars.b}}\"\n"+rules, gated)},
		{"a vars-routed fallback", "vars:\n  b: string = \"claw\"\n" + noToolsBot("agent", "  backend: \"claude_code\"\n"+rules+"  fallbacks:\n    v:\n      backend: \"{{vars.b}}\"\n      model: \"m2\"\n      on: [unavailable]\n", gated)},

		// A surface on the claw route.
		{"tools declared", noToolsBot("agent", "  backend: \"claw\"\n  tools: [read_file, grep, glob]\n"+rules, gated)},
		{"interaction carries ask_user", noToolsBot("agent", "  backend: \"claw\"\n  interaction: human\n"+rules, gated)},
		{"interaction: async", noToolsBot("agent", "  backend: \"claw\"\n  interaction: async\n"+rules, gated)},
		{"memory block installs the store tools", noToolsBot("agent", "  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n"+rules, gated)},
		{"memory read tools alone", noToolsBot("agent", "  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    write: false\n"+rules, gated)},
		{"memory write tool alone", noToolsBot("agent", "  backend: \"claw\"\n  memory:\n    enabled: true\n    scope: \"s\"\n    read: false\n"+rules, gated)},
		{"codemode on the node", noToolsBot("agent", "  backend: \"claw\"\n  codemode: on\n"+rules, gated)},
		{"codemode on the workflow", noToolsBot("agent", "  backend: \"claw\"\n"+rules, gated+"  codemode: on\n")},
		// An output schema empties the memory arm alone: the three that start
		// a tool loop keep their tools beside it.
		{"tools beside an output schema", outSchema + noToolsBot("agent", "  backend: \"claw\"\n  output: out\n  tools: [read_file]\n"+rules, gated)},
		{"interaction beside an output schema", outSchema + noToolsBot("agent", "  backend: \"claw\"\n  output: out\n  interaction: human\n"+rules, gated)},
		{"codemode beside an output schema", outSchema + noToolsBot("agent", "  backend: \"claw\"\n  output: out\n  codemode: on\n"+rules, gated)},
		{"memory block beside codemode and an output schema", outSchema + noToolsBot("agent", "  backend: \"claw\"\n  output: out\n  codemode: on\n  memory:\n    enabled: true\n    scope: \"s\"\n"+rules, gated)},
		{"tools declared, claw fallback", noToolsBot("agent", "  backend: \"claude_code\"\n  tools: [read_file]\n"+rules+"  fallbacks:\n    cl:\n      backend: \"claw\"\n      model: \"m2\"\n      on: [unavailable]\n", gated)},

		// A tool node carries no rule list: its `permission:` is C112's, and
		// its recovery agent reads the workflow's lists.
		{"tool node", "tool a:\n  command: \"true\"\n  permission: deny\nworkflow w:\n  entry: a\n  default_backend: \"claw\"\n  deny: [\"Bash\"]\n  a -> done\n"},
		{"tool node reaching its agent rung", "tool a:\n  command: \"./deploy.sh\"\n  goal: \"deployed\"\n  postcondition: \"./check.sh\"\n  policy: recover\n  recovery:\n    max_agent_attempts: 1\nworkflow w:\n  entry: a\n  permission: deny\n  default_backend: \"claw\"\n  deny: [\"Bash\"]\n  a -> done\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rulesNoTools(compileText(t, tc.src)); len(got) != 0 {
				t.Fatalf("C351 on a legitimate shape: %s", got[0].Message)
			}
		})
	}
}

// TestRulesWithoutToolsDiagnosticShape holds what a validate loop acts on:
// the code, the severity, the line of the first list the node declares, the
// property it names, the node it is attributed to and the catalogue's fix.
func TestRulesWithoutToolsDiagnosticShape(t *testing.T) {
	src := "agent a:\n  model: \"m\"\n  backend: \"claw\"\n  permission: deny\n  deny: [\"Bash\"]\n  allow: [\"Read(**)\"]\n" +
		"workflow w:\n  entry: a\n  a -> done\n"
	got := rulesNoTools(compileText(t, src))
	if len(got) != 1 {
		t.Fatalf("C351 fired %d time(s), want once: %+v", len(got), got)
	}
	d := got[0]
	if d.Code != "C351" || d.Severity != SeverityWarning {
		t.Errorf("code %s severity %v, want a C351 warning", d.Code, d.Severity)
	}
	if d.NodeID != "a" {
		t.Errorf("attributed to %q, want the node", d.NodeID)
	}
	// `allow:` is the first list in the order every message names them,
	// whichever the node writes first; its line is 6.
	if d.File != "x.bot" || d.Line != 6 || d.Column != 3 || d.Property != "allow" {
		t.Errorf("at %s:%d:%d property %q, want x.bot:6:3 on `allow`", d.File, d.Line, d.Column, d.Property)
	}
	if d.Hint == "" || d.Hint != HintFor(DiagPermissionRulesNoTools) {
		t.Errorf("hint %q is not the catalogue's %q", d.Hint, HintFor(DiagPermissionRulesNoTools))
	}
	for _, want := range []string{"`tools: [", "or drop the node's `allow:`/`ask:`/`deny:` lists"} {
		if !strings.Contains(d.Hint, want) {
			t.Errorf("the fix line does not carry %q: %s", want, d.Hint)
		}
	}
	if info := Catalog[DiagPermissionRulesNoTools]; info.Title != "Permission rules on a claw node that holds no tool" {
		t.Errorf("catalogue title %q", info.Title)
	}
}

// TestRulesWithoutToolsReadsBesideItsNeighbours holds the order a file's
// findings are read in where two of the family share a node.
//
// With the gate off, C111 and C351 name the same line — two defects, two
// fixes — and the code breaks the tie, so the gate's verdict reads first.
// With the gate on, C167 has nothing to say: a node that holds no tool holds
// no shell a deny could leave uncovered.
func TestRulesWithoutToolsReadsBesideItsNeighbours(t *testing.T) {
	codesOn := func(src string) []DiagCode {
		var out []DiagCode
		for _, d := range compileText(t, src).Diagnostics {
			if d.NodeID == "a" {
				out = append(out, d.Code)
			}
		}
		return out
	}
	off := codesOn(noToolsBot("agent", "  backend: \"claw\"\n  deny: [\"Bash\"]\n", ""))
	if len(off) != 2 || off[0] != DiagPermissionRulesNoGate || off[1] != DiagPermissionRulesNoTools {
		t.Errorf("gate off: findings %v, want C111 then C351", off)
	}
	on := codesOn(noToolsBot("agent", "  backend: \"claw\"\n  deny: [\"Bash\"]\n", "  permission: deny\n"))
	if len(on) != 1 || on[0] != DiagPermissionRulesNoTools {
		t.Errorf("gate on: findings %v, want C351 alone — no C167 over a roster claw does not serve", on)
	}
	// The fallback arm of the same pair: the claw route holds nothing, so
	// C351 names it and C167 stays silent.
	fb := codesOn(noToolsBot("agent", "  backend: \"claude_code\"\n  deny: [\"Bash\"]\n  fallbacks:\n    cl:\n      backend: \"claw\"\n      model: \"m2\"\n      on: [unavailable]\n", "  permission: deny\n"))
	for _, code := range fb {
		if code == DiagPermissionClassRisk {
			t.Errorf("C167 on a claw fallback that serves no tool: %v", fb)
		}
	}
}

// hintToolList is the `tools: [...]` example a catalogue fix line carries.
var hintToolList = regexp.MustCompile(`tools: \[([A-Za-z_, ]+)\]`)

// hintToolNames returns the names of the tool list a fix line gives as its
// example, failing the test when the line gives none.
func hintToolNames(t *testing.T, code DiagCode) []string {
	t.Helper()
	m := hintToolList.FindStringSubmatch(HintFor(code))
	if m == nil {
		t.Fatalf("%s's fix line gives no `tools: [...]` example: %s", code, HintFor(code))
	}
	var names []string
	for _, n := range strings.Split(m[1], ",") {
		names = append(names, strings.TrimSpace(n))
	}
	return names
}

// TestToolSurfaceHintsNameToolsClawServes: the fix line is what an author —
// often a model — pastes. Its example list is written for claw, so every
// name in it is one claw's registry resolves under that exact spelling; a
// CLI's spelling (`Read`, `Grep`) pasted from it would be C135's refusal on
// the very route the warning is about.
func TestToolSurfaceHintsNameToolsClawServes(t *testing.T) {
	for _, code := range []DiagCode{DiagPermissionRulesNoTools, DiagRecoveryAgentNoTools} {
		names := hintToolNames(t, code)
		if len(names) == 0 {
			t.Fatalf("%s's example list is empty", code)
		}
		for _, name := range names {
			if !toolcatalog.IsBuiltin(name) {
				t.Errorf("%s's fix line offers %q, which is not a claw built-in: %s", code, name, HintFor(code))
			}
			if _, refused := toolcatalog.ClawRefusesBuiltin(name); refused {
				t.Errorf("%s's fix line offers %q, which claw holds and cannot serve", code, name)
			}
		}
	}
}

// TestClawToolSurfaceReadsTheMemoryBlockAsDispatchDoes holds the memory arm
// on the one field no `.bot` text reaches: a block the compiler keeps is
// always enabled (`enabled: false` compiles to no block), while dispatch
// reads the field itself before installing anything.
func TestClawToolSurfaceReadsTheMemoryBlockAsDispatchDoes(t *testing.T) {
	w := &Workflow{}
	node := func(m *Memory) *AgentNode { return &AgentNode{BaseNode: BaseNode{ID: "a"}, Memory: m} }
	if !ClawToolSurfaceEmpty(node(&Memory{Enabled: false, Read: true, Write: true}), w) {
		t.Error("a disabled memory block was read as a tool surface: dispatch installs nothing for it")
	}
	if ClawToolSurfaceEmpty(node(&Memory{Enabled: true, Read: true}), w) {
		t.Error("an enabled memory block with read: true was read as no surface on a node with no output schema")
	}
}
