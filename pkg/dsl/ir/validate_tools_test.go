package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// toolsWorkflow builds a one-agent workflow whose backend line and tools line
// are the variables under test. Both are emitted verbatim so a test can pass
// an empty backend (the unresolved case) or a whole `fallbacks:` block.
func toolsWorkflow(nodeBody string) string {
	var b strings.Builder
	b.WriteString("schema out:\n  answer: string\n\nprompt p:\n  hi\n\nagent x:\n  system: p\n  output: out\n")
	b.WriteString(nodeBody)
	b.WriteString("\nworkflow w:\n  entry: x\n  x -> done\n")
	return b.String()
}

func compileToolsSrc(t *testing.T, src string) *CompileResult {
	t.Helper()
	pr := parser.Parse("t.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s\n--- source ---\n%s", d.Error(), src)
		}
	}
	return Compile(pr.File)
}

func diagsFor(cr *CompileResult, code DiagCode) []string {
	var out []string
	for _, d := range cr.Diagnostics {
		if d.Code == code {
			out = append(out, d.Message)
		}
	}
	return out
}

// TestUnknownToolOnClawIsAnError is the ticket's repro: the failure is
// deterministic from the source, so it must not cost a model call to find.
func TestUnknownToolOnClawIsAnError(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, list_files]\n"))
	got := diagsFor(cr, DiagUnknownTool)
	if len(got) != 1 {
		t.Fatalf("want exactly one C135 (for list_files), got %+v", cr.Diagnostics)
	}
	if !strings.Contains(got[0], `"list_files"`) {
		t.Errorf("the message must name the offending tool: %s", got[0])
	}
	if !strings.Contains(got[0], `"glob"`) {
		t.Errorf("list_files is the name authors actually type; the message should point at glob: %s", got[0])
	}
	if !cr.HasErrors() {
		t.Error("C135 must be an error — the run cannot succeed")
	}
}

// TestValidClawBuiltinsCompile guards the other direction: the catalog covers
// the families a real workflow uses, including the ones registered behind a
// host flag (web_search) and the interaction pair.
func TestValidClawBuiltinsCompile(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n"+
			"  tools: [read_file, write_file, file_edit, glob, grep, bash, web_fetch, web_search, skill, todo_write, agent, ask_user, read_image]\n"))
	if got := diagsFor(cr, DiagUnknownTool); len(got) > 0 {
		t.Fatalf("registered built-ins must compile clean, got %v", got)
	}
}

// TestUnknownToolOnClaudeCodeIsSilent: the lowercase list is inert under
// bypassPermissions, so the same names that break claw are merely dead config
// here. Flagging them would be wrong AND would fire on most of the catalog.
func TestUnknownToolOnClaudeCodeIsSilent(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  tools: [read_file, list_files, git_diff, search_codebase]\n"))
	if got := diagsFor(cr, DiagUnknownTool); len(got) > 0 {
		t.Fatalf("claude_code does not constrain its toolset; want no C135, got %v", got)
	}
}

// TestUnknownToolUnresolvedBackendIsSilent: with no backend anywhere the
// resolver falls through to env + host credential detection, so the compiler
// genuinely cannot know which backend serves — the same reasoning that keeps
// validateAutoMemory quiet there.
func TestUnknownToolUnresolvedBackendIsSilent(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow("  tools: [read_file, list_files]\n"))
	if got := diagsFor(cr, DiagUnknownTool); len(got) > 0 {
		t.Fatalf("an unresolved backend must not be guessed at, got %v", got)
	}
}

// TestUnknownToolInheritsWorkflowDefaultBackend: declaring the backend once at
// the workflow level is the likeliest shape by far, and reading only the
// node's own field would miss every one of them.
func TestUnknownToolInheritsWorkflowDefaultBackend(t *testing.T) {
	src := strings.Replace(
		toolsWorkflow("  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, list_files]\n"),
		"workflow w:\n", "workflow w:\n  default_backend: \"claw\"\n", 1)
	cr := compileToolsSrc(t, src)
	if got := diagsFor(cr, DiagUnknownTool); len(got) != 1 {
		t.Fatalf("want one C135 through default_backend, got %+v", cr.Diagnostics)
	}
}

// TestUnknownToolIgnoresMCPAndEnvRefs: everything the compiler cannot decide
// stays the run time's business — MCP names and wildcards are discovered when
// the server connects, `${VAR}` is read from the environment.
func TestUnknownToolIgnoresMCPAndEnvRefs(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n"+
			"  tools: [read_file, mcp.github.create_issue, mcp.github.*, mcp__iterion_board__create, \"${EXTRA_TOOL}\"]\n"))
	if got := diagsFor(cr, DiagUnknownTool); len(got) > 0 {
		t.Fatalf("dynamic tool references must not be rejected, got %v", got)
	}
}

// TestUnknownToolOnClawFallbackRoute: the route inherits the SAME list, and it
// exists to serve when the primary is already failing — an unresolvable name
// there turns the safety net into a second failure.
func TestUnknownToolOnClawFallbackRoute(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: [read_file, run_command]\n"+
			"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n"))
	got := diagsFor(cr, DiagUnknownTool)
	if len(got) != 1 {
		t.Fatalf("want one C135 for the claw route, got %+v", cr.Diagnostics)
	}
	if !strings.Contains(got[0], `"api"`) || !strings.Contains(got[0], `"bash"`) {
		t.Errorf("the message must name the route and the replacement: %s", got[0])
	}
}

// TestUnknownToolReportedOncePerNodeAcrossRoutes: several claw routes share
// one tools list, so repeating the message per route says nothing new.
func TestUnknownToolReportedOncePerNodeAcrossRoutes(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: [read_file, run_command]\n"+
			"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n"+
			"    gpt:\n      backend: \"claw\"\n      model: \"openai/gpt-5.5\"\n"))
	if got := diagsFor(cr, DiagUnknownTool); len(got) != 1 {
		t.Fatalf("want a single C135 for the node, got %v", got)
	}
}

// TestUnknownToolClawPrimaryNotDoubleReported: a claw node with a claw route
// must be reported for its own backend, once.
func TestUnknownToolClawPrimaryNotDoubleReported(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, list_files]\n"+
			"  fallbacks:\n    gpt:\n      backend: \"claw\"\n      model: \"openai/gpt-5.5\"\n"))
	got := diagsFor(cr, DiagUnknownTool)
	if len(got) != 1 {
		t.Fatalf("want a single C135, got %v", got)
	}
	if strings.Contains(got[0], "fallback") {
		t.Errorf("the node's own backend is the one that fails first: %s", got[0])
	}
}

// TestRunFallbackRefusedWhenToolsCannotResolve: the launch-time `--fallback`
// route is screened by the same predicate, so an operator cannot reach through
// a flag the route the compiler refuses in the .bot.
func TestRunFallbackRefusedWhenToolsCannotResolve(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: [read_file, run_command]\n"))
	if cr.HasErrors() {
		t.Fatalf("fixture must compile clean: %+v", cr.Diagnostics)
	}
	refusals := ApplyRunFallback(cr.Workflow, Fallback{Backend: "claw", Model: "anthropic/claude-opus-5"})
	if len(refusals) != 1 {
		t.Fatalf("want the route refused, got %v", refusals)
	}
	if !strings.Contains(refusals[0], `"run_command"`) {
		t.Errorf("the refusal must say which tool cannot resolve: %s", refusals[0])
	}
	agent, ok := cr.Workflow.Nodes["x"].(*AgentNode)
	if !ok {
		t.Fatal("node x is not an agent")
	}
	if len(agent.Fallbacks) != 0 {
		t.Errorf("a refused route must not be attached: %+v", agent.Fallbacks)
	}
}

// TestRunFallbackAcceptedWhenToolsResolve is the same launch with a list the
// route can serve — the screen must not become a blanket refusal of claw.
func TestRunFallbackAcceptedWhenToolsResolve(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: [read_file, bash]\n"))
	if refusals := ApplyRunFallback(cr.Workflow, Fallback{Backend: "claw", Model: "anthropic/claude-opus-5"}); len(refusals) != 0 {
		t.Fatalf("a resolvable list must be accepted, got %v", refusals)
	}
	agent := cr.Workflow.Nodes["x"].(*AgentNode)
	if len(agent.Fallbacks) != 1 {
		t.Fatalf("want the route attached, got %+v", agent.Fallbacks)
	}
}

// TestUnknownRecoveryAgentToolOnClaw: rung 4 of a Verified Action (ADR-044)
// hands `agent_tools:` to a synthetic agent node, so the same names hit the
// same registry — and there the failure lands after the deterministic rungs
// have already failed, which is the worst moment to discover it.
func TestUnknownRecoveryAgentToolOnClaw(t *testing.T) {
	src := `prompt p:
  hi

tool build:
  command: "make build"
  goal: "the build passes"
  postcondition: "make build"
  policy: recover
  recovery:
    max_agent_attempts: 1
    agent_tools: [read_file, run_command]

workflow w:
  default_backend: "claw"
  entry: build
  build -> done
`
	cr := compileToolsSrc(t, src)
	got := diagsFor(cr, DiagUnknownTool)
	if len(got) != 1 {
		t.Fatalf("want one C135 for agent_tools, got %+v", cr.Diagnostics)
	}
	if !strings.Contains(got[0], "agent_tools") || !strings.Contains(got[0], `"bash"`) {
		t.Errorf("the message must name the field and the replacement: %s", got[0])
	}
}

// TestRecoveryAgentToolsSilentWithoutClawDefault: the synthetic node declares
// no backend, so only the workflow default can make it claw — anything else is
// the unresolved case and must stay silent.
func TestRecoveryAgentToolsSilentWithoutClawDefault(t *testing.T) {
	src := `prompt p:
  hi

tool build:
  command: "make build"
  goal: "the build passes"
  postcondition: "make build"
  policy: recover
  recovery:
    max_agent_attempts: 1
    agent_tools: [read_file, run_command]

workflow w:
  entry: build
  build -> done
`
	cr := compileToolsSrc(t, src)
	if got := diagsFor(cr, DiagUnknownTool); len(got) > 0 {
		t.Fatalf("an unresolved backend must not be guessed at, got %v", got)
	}
}
