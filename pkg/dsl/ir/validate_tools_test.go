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
	// The fatal branch is where the way out matters most: its predicate
	// includes near-miss typos, which can occasionally be an ambient MCP
	// tool sitting that close to a built-in.
	if !strings.Contains(got[0], "mcp.<server>.<tool>") {
		t.Errorf("a blocking finding must carry its escape hatch: %s", got[0])
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

// TestUnknownToolIgnoresQualifiedMCPRefs: a qualified MCP reference names a
// server's tools, which exist only once it connects — the compiler has no
// opinion on those, in any of the three spellings.
func TestUnknownToolIgnoresQualifiedMCPRefs(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n"+
			"  tools: [read_file, mcp.github.create_issue, mcp.github.*, mcp__iterion_board__create]\n"))
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
	refusals := ApplyRunFallback(cr.Workflow, []Fallback{{Backend: "claw", Model: "anthropic/claude-opus-5"}}, false)
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
	if refusals := ApplyRunFallback(cr.Workflow, []Fallback{{Backend: "claw", Model: "anthropic/claude-opus-5"}}, false); len(refusals) != 0 {
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

// TestBoardShorthandIsAccepted (R4290e1): Registry.Resolve matches a bare name
// as a unique suffix over the registered MCP tools, and the runtime registers
// the board family for every run — so `tools: [create_issue]` on a claw node
// resolves and runs today. Rejecting it would be the expensive direction of
// drift: a hard error on a workflow that works.
func TestBoardShorthandIsAccepted(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n"+
			"  capabilities: [board.create, board.read]\n"+
			"  tools: [read_file, create_issue, list_issues, subscribe]\n"))
	if got := diagsFor(cr, DiagUnknownTool); len(got) > 0 {
		t.Fatalf("iterion's own board/watch tools resolve by their bare name, got %v", got)
	}
}

// TestUnknownToolWarnsWhenWorkflowWiresMCP: a third-party server's tool list
// exists only once it connects, so the compiler cannot rule out that a bare
// name resolves through the shorthand path. The finding survives — the author
// keeps the signal — but as a warning, since blocking would reject a workflow
// that runs.
func TestUnknownToolWarnsWhenWorkflowWiresMCP(t *testing.T) {
	src := toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, browser_click]\n") +
		"\nmcp_server playwright:\n  transport: http\n  url: \"https://example.com/mcp\"\n"
	cr := compileToolsSrc(t, src)
	if got := diagsFor(cr, DiagUnknownTool); len(got) != 1 {
		t.Fatalf("want one C135, got %+v", cr.Diagnostics)
	}
	if cr.HasErrors() {
		t.Errorf("with MCP servers wired the finding must not BLOCK: %+v", cr.Diagnostics)
	}
}

// TestUnexpandedToolRefIsReported (Rd78359): `tools:` is the one field iterion
// never expands — unlike model:/backend:/command:/timeout:, which all go
// through ir.ExpandEnvWithDefault. A `${VAR}` entry therefore reaches the
// registry verbatim and can only fail, so it is reported with a message saying
// why, instead of being exempted on a promise nothing keeps.
//
// Asserted below the parser on purpose: the .bot grammar accepts only bare and
// dotted identifiers in a tools list (parseToolRef), so a quoted `${VAR}` is
// dropped at parse. The shape is reachable through the AST/JSON path the
// studio editor and `iterion import` write, where Tools is a []string carried
// verbatim.
func TestUnexpandedToolRefIsReported(t *testing.T) {
	got := unresolvableToolNames([]string{"read_file", "${EXTRA_TOOL}", "{{vars.extra}}"})
	want := []string{"${EXTRA_TOOL}", "{{vars.extra}}"}
	if len(got) != len(want) {
		t.Fatalf("unresolvableToolNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unresolvableToolNames = %v, want %v", got, want)
		}
	}
	if hint := toolHint("${EXTRA_TOOL}"); !strings.Contains(hint, "does not expand") {
		t.Errorf("the hint must explain why the ref cannot work: %s", hint)
	}
}

// TestUnrecognisedNameWarnsWithoutMCPBlock (R931e3a): the ambient MCP catalog
// — a project .mcp.json, an enabled plugin's mcp_servers — is merged AFTER
// ir.Compile, and a claw node gets those servers spliced in as `mcp.<srv>.*`.
// So a name the compiler merely does not recognise may resolve and run, and
// blocking it would refuse a working workflow to guard a guess.
func TestUnrecognisedNameWarnsWithoutMCPBlock(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, firecrawl_search]\n"))
	got := diagsFor(cr, DiagUnknownTool)
	if len(got) != 1 {
		t.Fatalf("want one C135 for the unrecognised name, got %+v", cr.Diagnostics)
	}
	if cr.HasErrors() {
		t.Errorf("a name the compiler cannot identify must NOT block: %+v", cr.Diagnostics)
	}
	if !strings.Contains(got[0], "warning") && !strings.Contains(got[0], "mcp.<server>.<tool>") {
		t.Errorf("the message should tell the reader why it stopped short of an error: %s", got[0])
	}
}

// TestIdentifiableMistakeStillBlocks: the ticket's case is exactly the one the
// compiler CAN name the fix for, so softening the unknown names must not cost
// the check its teeth.
func TestIdentifiableMistakeStillBlocks(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, list_files, read_fil]\n"))
	if got := diagsFor(cr, DiagUnknownTool); len(got) != 2 {
		t.Fatalf("want C135 for both the legacy name and the typo, got %+v", cr.Diagnostics)
	}
	if !cr.HasErrors() {
		t.Error("a phantom name and a near-miss typo are identifiable mistakes — they must block")
	}
}

// TestMCPActivationBlockSoftensEvenLegacyNames: an `mcp:` block is the author
// wiring servers on purpose (it sets w.MCP, NOT w.MCPServers — the shape the
// first fix missed), and a server they wire can expose any name at all.
func TestMCPActivationBlockSoftensEvenLegacyNames(t *testing.T) {
	src := strings.Replace(
		toolsWorkflow("  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  tools: [read_file, list_files]\n"),
		"workflow w:\n", "workflow w:\n  mcp:\n    servers: [firecrawl]\n", 1)
	cr := compileToolsSrc(t, src)
	if got := diagsFor(cr, DiagUnknownTool); len(got) != 1 {
		t.Fatalf("want one C135, got %+v", cr.Diagnostics)
	}
	if cr.HasErrors() {
		t.Errorf("with MCP deliberately wired the finding must not block: %+v", cr.Diagnostics)
	}
}

// TestRunFallbackNotRefusedOnUnrecognisedName: the launch flag is screened on
// exactly what C135 blocks, so an operator's explicit route is never dropped
// on a name the compiler merely does not recognise.
func TestRunFallbackNotRefusedOnUnrecognisedName(t *testing.T) {
	cr := compileToolsSrc(t, toolsWorkflow(
		"  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  tools: [read_file, firecrawl_search]\n"))
	if refusals := ApplyRunFallback(cr.Workflow, []Fallback{{Backend: "claw", Model: "anthropic/claude-opus-5"}}, false); len(refusals) != 0 {
		t.Fatalf("an unrecognised name must not drop the operator's route, got %v", refusals)
	}
}
