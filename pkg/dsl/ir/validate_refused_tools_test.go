package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// refusedCode is the code under test.
const refusedCode = DiagToolRefusedOnClaw

// refusedProbe is the name the scenarios below declare. The production list
// is EMPTY — `agent` left it when iterion wired real subagents at both hosts
// (#1940) — so C159's three arms are driven through an injected entry over a
// name the catalog really holds. Without it the mechanism would be read by
// nothing, and a mechanism no test can fire is a mechanism nobody knows still
// works; with it, every reduction, scope and hedge below is exercised exactly
// as the next refused capability will find them.
const refusedProbe = "web_search"

// probeRefusal is shaped like the refusals this list carries: a reason in the
// present tense, and a remedy naming where the capability IS served.
var probeRefusal = toolcatalog.ClawRefusal{
	Reason: "iterion serves this name on no claw host, so claw registers it and answers nothing",
	Remedy: "Do the work in this node, or route it to claude_code, whose native Agent tool " +
		"(legacy name Task) serves it",
}

// withRefusedProbe installs probeRefusal for the duration of the test.
func withRefusedProbe(t *testing.T) {
	t.Helper()
	t.Cleanup(toolcatalog.SetClawRefusedBuiltinForTest(refusedProbe, probeRefusal))
}

// TestTheRefusedClawListIsEmptyInProduction is the flip of #1940: with the
// subagent runner wired at both hosts, no name claw registers is beyond
// iterion, and `agent` in particular must draw no warning — a warning that
// contradicts a working capability teaches authors to ignore diagnostics.
func TestTheRefusedClawListIsEmptyInProduction(t *testing.T) {
	if names := toolcatalog.ClawRefusedBuiltins(); len(names) != 0 {
		t.Fatalf("the refused list carries %v; every name claw registers is served, so C159 fires on nothing", names)
	}
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, "+refusedProbe+"]\n"))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("a claw node declaring `agent` still warns: %v — the capability is wired (#1940)", got)
	}
	if got := diagsFor(cr, DiagUnknownTool); len(got) > 0 {
		t.Fatalf("`agent` must stay a name the registry HOLDS, so C135 has nothing to say: %v", got)
	}
}

// TestRefusedToolOnClawIsAWarning: the probe resolves on claw — the registry
// holds it, so C135 is silent — and every call would earn a typed refusal
// because iterion serves it on no host. The author pays a run to learn that;
// the compiler knows it from the source.
//
// A warning, not an error: a host MAY serve the capability, and philosophy #1
// warns rather than rejects.
func TestRefusedToolOnClawIsAWarning(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, "+refusedProbe+"]\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want exactly one C159 for the probe on claw, got %+v", cr.Diagnostics)
	}
	if !strings.Contains(got[0], `"`+refusedProbe+`"`) {
		t.Errorf("the message must name the tool: %s", got[0])
	}
	if !strings.Contains(got[0], "claude_code") {
		t.Errorf("the message must name where the capability IS served: %s", got[0])
	}
	if cr.HasErrors() {
		t.Error("C159 must be a warning — the node still runs, and a host may wire a runner")
	}
	if len(diagsFor(cr, DiagUnknownTool)) != 0 {
		t.Error("C159 and C135 must stay disjoint: the probe is a name claw HAS")
	}
}

// TestRefusedToolOnClaudeCodeIsSilent: the list is claw's, so a node on
// another backend has nothing to read from it — the parity doctrine says
// nothing where the capability is served.
func TestRefusedToolOnClaudeCodeIsSilent(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  tools: [read_file, "+refusedProbe+"]\n"))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("claude_code serves subagents; want no C159, got %v", got)
	}
}

// TestRefusedToolUnresolvedBackendIsSilent: with no backend anywhere the
// resolver falls through to env and host credential detection, so the compiler
// cannot know which backend serves — the same silence C135 keeps there.
func TestRefusedToolUnresolvedBackendIsSilent(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow("  tools: [read_file, "+refusedProbe+"]\n"))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("want no C159 on an unresolved backend, got %v", got)
	}
}

// TestRefusedToolOnClawFallbackRoute: the route inherits the SAME list and
// exists to serve when the primary is already failing — a tool refused there
// turns the safety net into a second disappointment.
func TestRefusedToolOnClawFallbackRoute(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: [read_file, "+refusedProbe+"]\n"+
			"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want one C159 for the claw route, got %+v", cr.Diagnostics)
	}
	if !strings.Contains(got[0], `"api"`) {
		t.Errorf("the message must name the route: %s", got[0])
	}
}

// TestRefusedToolReportedOncePerNodeAcrossRoutes: several claw routes share
// one tools list, so repeating the message per route says nothing new.
func TestRefusedToolReportedOncePerNodeAcrossRoutes(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: [read_file, "+refusedProbe+"]\n"+
			"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n"+
			"    gpt:\n      backend: \"claw\"\n      model: \"openai/gpt-5.5\"\n"))
	if got := diagsFor(cr, refusedCode); len(got) != 1 {
		t.Fatalf("want a single C159 for the node, got %v", got)
	}
}

// TestRefusedToolOnClawPrimaryNotDoubleReported: a claw node with a claw route
// is reported for its own backend, once — the route adds no new fact.
func TestRefusedToolOnClawPrimaryNotDoubleReported(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: ["+refusedProbe+"]\n"+
			"  fallbacks:\n    gpt:\n      backend: \"claw\"\n      model: \"openai/gpt-5.5\"\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want a single C159, got %v", got)
	}
	if strings.Contains(got[0], "fallback") {
		t.Errorf("the node's own backend is the one that refuses first: %s", got[0])
	}
}

// TestRefusedToolSilentWhenNotDeclared: the diagnostic reads the author's own
// list. Nothing else on a claw node may raise it.
func TestRefusedToolSilentWhenNotDeclared(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, bash]\n"))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("want no C159 for a list that does not name a refused tool, got %v", got)
	}
}

// recoveryAgentToolsWorkflow is a legitimate Verified Action (command and
// postcondition differ) whose rung-4 recovery agent declares `tools` — the
// third author-written list that reaches claw's registry.
func recoveryAgentToolsWorkflow(tools, workflowBody string) string {
	return `prompt p:
  hi

tool build:
  command: "make build"
  goal: "the build passes"
  postcondition: "test -f ./out/bin"
  policy: recover
  recovery:
    max_agent_attempts: 1
    agent_tools: [` + tools + `]

workflow w:
` + workflowBody + `  entry: build
  build -> done
`
}

// TestRefusedRecoveryAgentToolOnClaw: executor_verified_action hand-builds an
// ir.AgentNode carrying `agent_tools:` verbatim and dispatches it through the
// same claw registry, so a refused name is refused there too — at the moment
// C135's own message calls the worst one, after the deterministic rungs have
// already failed. The identical entry on an agent node's own `tools:` is
// warned about, so silence here is the hole.
func TestRefusedRecoveryAgentToolOnClaw(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, recoveryAgentToolsWorkflow("read_file, "+refusedProbe, "  default_backend: \"claw\"\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want exactly one C159 for the probe on a rung-4 recovery agent, got %+v", cr.Diagnostics)
	}
	for _, want := range []string{"agent_tools", `"` + refusedProbe + `"`, "claude_code"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("the message must name %s: %s", want, got[0])
		}
	}
	if cr.HasErrors() {
		t.Error("C159 must stay a warning on the recovery list too")
	}
	if len(diagsFor(cr, DiagUnknownTool)) != 0 {
		t.Error("C159 and C135 must stay disjoint: the probe is a name claw HAS")
	}
}

// TestRefusedRecoveryAgentToolSilentWithoutClawDefault: the synthetic node
// declares no backend of its own, so only the workflow default can make it
// claw — the same silence C135 keeps on an unresolved backend.
func TestRefusedRecoveryAgentToolSilentWithoutClawDefault(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, recoveryAgentToolsWorkflow("read_file, "+refusedProbe, ""))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("an unresolved backend must not be guessed at, got %v", got)
	}
}

// TestRefusedRecoveryAgentToolSilentWhenNotDeclared keeps the new arm narrow:
// a recovery list naming only served tools says nothing.
func TestRefusedRecoveryAgentToolSilentWhenNotDeclared(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, recoveryAgentToolsWorkflow("read_file, bash", "  default_backend: \"claw\"\n"))
	if got := diagsFor(cr, refusedCode); len(got) > 0 {
		t.Fatalf("want no C159 for a recovery list of served tools, got %v", got)
	}
}

// TestEveryCompileTimeMessageHedgesWhatTheSeverityDeclined: the refusal is a
// WARNING at compile time because a host MAY wire a subagent runner, so no
// such message may state it as certain — the rule this file already applies
// to C135 (toolDiag's consequence travels with the reporter). An operator who
// wired the runner must not read a warning whose text is false for their
// deployment.
//
// Three messages make the claim, and the guard takes all three: C159's arms
// (the node's own backend, its first claw `fallbacks:` route, a Verified
// Action's rung-4 `recovery: agent_tools:`). C270's claw arm left the table
// when the `agent` spawner stopped being refused (#1940) — it now describes a
// capability that works, and has nothing to hedge.
func TestEveryCompileTimeMessageHedgesWhatTheSeverityDeclined(t *testing.T) {
	withRefusedProbe(t)
	const hedge = "unless"
	for _, tc := range []struct {
		name string
		code DiagCode
		cr   *CompileResult
	}{
		{"C159 node backend", refusedCode, compileToolsSrc(t, toolsWorkflow(
			"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: ["+refusedProbe+"]\n"))},
		{"C159 fallback route", refusedCode, compileToolsSrc(t, toolsWorkflow(
			"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: ["+refusedProbe+"]\n"+
				"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n"))},
		{"C159 rung-4 recovery", refusedCode, compileToolsSrc(t,
			recoveryAgentToolsWorkflow(refusedProbe, "  default_backend: \"claw\"\n"))},
	} {
		got := diagsFor(tc.cr, tc.code)
		if len(got) != 1 {
			t.Fatalf("%s: want one %s, got %+v", tc.name, tc.code, tc.cr.Diagnostics)
		}
		if !strings.Contains(got[0], hedge) {
			t.Errorf("%s: the warning asserts the refusal as certain, with no %q clause for a host that wires the runner: %s",
				tc.name, hedge, got[0])
		}
		if !strings.Contains(got[0], "host serves it") && !strings.Contains(got[0], "subagent runner") {
			t.Errorf("%s: the message does not name the override that changes the outcome: %s", tc.name, got[0])
		}
	}
}

// TestRefusedToolReportedOncePerRepeatedName: a `tools:` list is a list, and
// an author can name the same tool twice. C135's own reduction over the same
// field (unresolvableToolNames) deduplicates, so the two codes report the
// same source shape the same way — one row per refused NAME, not one per
// occurrence of it on the line.
func TestRefusedToolReportedOncePerRepeatedName(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: ["+refusedProbe+", read_file, "+refusedProbe+"]\n"))
	if got := diagsFor(cr, refusedCode); len(got) != 1 {
		t.Fatalf("want one C159 for a name listed twice, got %d: %v", len(got), got)
	}
}

// TestRefusedRecoveryAgentToolReportedOncePerRepeatedName: the rung-4 recovery
// list is reduced by the same helper, so it inherits the same guarantee.
func TestRefusedRecoveryAgentToolReportedOncePerRepeatedName(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, recoveryAgentToolsWorkflow(refusedProbe+", read_file, "+refusedProbe, "  default_backend: \"claw\"\n"))
	if got := diagsFor(cr, refusedCode); len(got) != 1 {
		t.Fatalf("want one C159 for a recovery list naming the probe twice, got %d: %v", len(got), got)
	}
}

// TestRefusedToolNameIsReportedTrimmed: the catalog predicates trim, so a
// padded entry still draws the warning about the tool it names — and the name
// the message quotes is the TOOL's, not the author's whitespace, which is what
// a reader greps for (C135's reduction trims the same way).
//
// The padding's own cost is not this warning's subject. The registry resolves
// an entry as written, so a padded one fails the node at dispatch before any
// call — pinned, for the production registry, by pkg/backend/model's
// TestAPaddedToolEntryNeverReachesTheRefusal.
func TestRefusedToolNameIsReportedTrimmed(t *testing.T) {
	withRefusedProbe(t)
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [\" "+refusedProbe+" \", read_file]\n"))
	got := diagsFor(cr, refusedCode)
	if len(got) != 1 {
		t.Fatalf("want one C159 for a padded refused name, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"`+refusedProbe+`"`) {
		t.Errorf("the message quotes the author's padding instead of the tool name: %s", got[0])
	}
}
