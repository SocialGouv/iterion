package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/toolcatalog"
)

// C328 — a node naming an iterion host tool without the capability that
// opens it is told at compile time, with the capability to declare (#1801).

func c328Diags(t *testing.T, src string) []Diagnostic {
	t.Helper()
	return compileFile(t, src).Diagnostics
}

func c328Count(diags []Diagnostic) int {
	n := 0
	for _, d := range diags {
		if d.Code == DiagHostToolWithoutCapability {
			n++
		}
	}
	return n
}

func agentSrc(toolsLine string, extra ...string) string {
	var b strings.Builder
	for _, e := range extra {
		b.WriteString(e + "\n")
	}
	b.WriteString(`agent a:
  backend: "claw"
  model: "m"
  tools: [` + toolsLine + `]

workflow w:
  entry: a
  a -> done
`)
	return b.String()
}

func TestC328_AgentNamingABoardToolWithoutTheCapability(t *testing.T) {
	diags := c328Diags(t, agentSrc("read_file, create_issue"))
	if c328Count(diags) != 1 {
		t.Fatalf("C328 x%d on create_issue without board.create, want 1: %v", c328Count(diags), diags)
	}
	for _, d := range diags {
		if d.Code == DiagHostToolWithoutCapability && !strings.Contains(d.Message, "board.create") {
			t.Errorf("the warning does not name the capability to declare: %q", d.Message)
		}
	}
}

func TestC328_QuietWhenTheCapabilityIsHeld(t *testing.T) {
	src := strings.Replace(agentSrc("create_issue"), `  model: "m"`, "  model: \"m\"\n  capabilities: [board.create]", 1)
	if diags := c328Diags(t, src); c328Count(diags) != 0 {
		t.Fatalf("C328 while board.create is held: %v", diags)
	}
}

func TestC328_InheritsTheWorkflowsGrant(t *testing.T) {
	src := strings.Replace(agentSrc("create_issue"), "workflow w:\n  entry: a", "workflow w:\n  capabilities: [board.create]\n  entry: a", 1)
	if diags := c328Diags(t, src); c328Count(diags) != 0 {
		t.Fatalf("C328 while the workflow grants board.create: %v", diags)
	}
}

func TestC328_ADeclaredEmptyListHoldsNothing(t *testing.T) {
	// capabilities: [] is a declared empty grant, not an inheritance.
	src := strings.Replace(agentSrc("create_issue"), "workflow w:\n  entry: a", "workflow w:\n  capabilities: [board.create]\n  entry: a", 1)
	src = strings.Replace(src, `  model: "m"`, "  model: \"m\"\n  capabilities: []", 1)
	if diags := c328Diags(t, src); c328Count(diags) != 1 {
		t.Fatalf("C328 x%d on a declared-empty grant, want 1: %v", c328Count(diags), diags)
	}
}

func TestC328_QualifiedSpellings(t *testing.T) {
	for _, spelling := range []string{
		`"mcp.iterion_board.close_issue"`,
		`"mcp__iterion_board__close_issue"`,
	} {
		diags := c328Diags(t, agentSrc(spelling))
		if c328Count(diags) != 1 {
			t.Errorf("C328 x%d on %s without board.close, want 1", c328Count(diags), spelling)
		}
	}
}

// The sanitized single-underscore form is the shape iterion advertises to the
// provider for the MODEL's calls — no resolver reads it back from a `tools:`
// list, so the entry is dead whatever the grant: C135's to say, once.
func TestC328_SingleUnderscoreSpellingIsC135s(t *testing.T) {
	diags := c328Diags(t, agentSrc(`"mcp_iterion_board_create_issue"`))
	if n := c328Count(diags); n != 0 {
		t.Errorf("C328 x%d on the single-underscore call spelling — C135 already names the dead entry, and declaring the capability would not fix it: %v", n, diags)
	}
	var c135 bool
	for _, d := range diags {
		if d.Code == DiagUnknownTool {
			c135 = true
		}
	}
	if !c135 {
		t.Errorf("no C135 on mcp_iterion_board_create_issue either — the dead entry would pass silently: %v", diags)
	}
}

func TestC328_WatchAndRunsFamilies(t *testing.T) {
	diags := c328Diags(t, agentSrc("subscribe, runs_list"))
	if c328Count(diags) != 2 {
		t.Fatalf("C328 x%d on subscribe+runs_list without the grants, want 2: %v", c328Count(diags), diags)
	}
}

func TestC328_WildcardNamesEveryMissingGrant(t *testing.T) {
	src := strings.Replace(agentSrc(`"mcp.iterion_board.*"`), `  model: "m"`, "  model: \"m\"\n  capabilities: [board.read]", 1)
	diags := c328Diags(t, src)
	// board.read is held; the nine write-side tools must each be named
	// (label x3, assign x2, create/close/comment/move x1).
	if n := c328Count(diags); n != 9 {
		t.Errorf("a board wildcard holding only board.read drew %d C328s, want the 9 write-side tools", n)
	}
}

func TestC328_ToolNodeBareCommandReadsTheWorkflowsList(t *testing.T) {
	src := `tool closer:
  command: close_issue

workflow w:
  entry: closer
  closer -> done
`
	if diags := c328Diags(t, src); c328Count(diags) != 1 {
		t.Fatalf("C328 x%d on a tool node whose bare command is close_issue, want 1: %v", c328Count(diags), diags)
	}

	granted := strings.Replace(src, "workflow w:\n  entry: closer", "workflow w:\n  capabilities: [board.close]\n  entry: closer", 1)
	if diags := c328Diags(t, granted); c328Count(diags) != 0 {
		t.Fatalf("C328 on a tool node whose workflow holds board.close: %v", diags)
	}
}

func TestC328_BareNamesStepAsideWhenTheWorkflowWiresMCPServers(t *testing.T) {
	// A bare `subscribe` may name the third-party server's tool: only the
	// qualified spellings are judged then. The server must be WIRED — an
	// `mcp:` block naming it — not merely declared.
	src := `mcp_server bus:
  transport: stdio
  command: "./bus"

agent a:
  backend: "claw"
  model: "m"
  tools: [subscribe, "mcp.iterion_watch.subscribe"]
  mcp:
    servers: [bus]

workflow w:
  entry: a
  a -> done
`
	if n := c328Count(c328Diags(t, src)); n != 1 {
		t.Errorf("with MCP servers wired, C328 fired x%d, want 1 (the qualified spelling only)", n)
	}
}

// A top-level `mcp_server:` DECLARATION wires nothing by itself: no `mcp:`
// block names the server, so it never reaches a node's registry
// (PrepareWorkflow activates only what a block names, inherits or
// autoloads), and a bare `create_issue` remains iterion's own shorthand.
// Judged, not stepped aside.
func TestC328_ADeclaredButUnwiredServerKeepsBareNamesJudged(t *testing.T) {
	src := `mcp_server bus:
  transport: stdio
  command: "./bus"

agent a:
  backend: "claw"
  model: "m"
  tools: [create_issue]

workflow w:
  entry: a
  a -> done
`
	if n := c328Count(c328Diags(t, src)); n != 1 {
		t.Errorf("C328 x%d with a declared-but-unwired mcp_server, want 1", n)
	}
}

// The converse wiring: a workflow-level `mcp:` block (autoloading the
// project's servers here — no top-level declaration at all) connects servers
// whose bare tool names the compiler cannot enumerate, so bare names step
// aside exactly as when a declared server is wired.
func TestC328_WorkflowMCPBlockStepsBareNamesAside(t *testing.T) {
	src := `agent a:
  backend: "claw"
  model: "m"
  tools: [subscribe, "mcp.iterion_watch.subscribe"]

workflow w:
  entry: a
  mcp:
    autoload_project: true
  a -> done
`
	if n := c328Count(c328Diags(t, src)); n != 1 {
		t.Errorf("with a workflow mcp: block autoloading project servers, C328 fired x%d, want 1 (the qualified spelling only)", n)
	}
}

func TestC328_AThirdPartyNameIsNotAHostTool(t *testing.T) {
	if diags := c328Diags(t, agentSrc(`"mcp.github.create_issue", my_custom_tool`)); c328Count(diags) != 0 {
		t.Fatalf("C328 on a third-party or unknown tool name: %v", diags)
	}
}

// The rung-4 recovery agent's `agent_tools:` is gated the same way at run
// time — the synthetic node inherits the workflow's capabilities — so the
// check covers the third list, as C135 and C159 already do.
func TestC328_RecoveryAgentTools(t *testing.T) {
	src := `tool s:
  command: "git status"
  postcondition: "true"
  policy: recover
  recovery:
    max_agent_attempts: 1
    agent_tools: [create_issue]

workflow w:
  entry: s
  s -> done
`
	diags := c328Diags(t, src)
	if n := c328Count(diags); n != 1 {
		t.Fatalf("C328 x%d on recovery agent_tools naming create_issue without the grant, want 1: %v", n, diags)
	}
	for _, d := range diags {
		if d.Code == DiagHostToolWithoutCapability && !strings.Contains(d.Message, "agent_tools") {
			t.Errorf("the warning does not name the recovery agent_tools: list: %q", d.Message)
		}
	}

	granted := strings.Replace(src, "workflow w:\n  entry: s", "workflow w:\n  capabilities: [board.create]\n  entry: s", 1)
	if diags := c328Diags(t, granted); c328Count(diags) != 0 {
		t.Fatalf("C328 on recovery agent_tools while the workflow holds board.create: %v", diags)
	}
}

// One warning per host tool per list: two spellings of the same tool are one
// finding, and a wildcard does not repeat what an explicit entry said.
func TestC328_DeduplicatesSpellingsOfOneTool(t *testing.T) {
	diags := c328Diags(t, agentSrc(`subscribe, "mcp.iterion_watch.subscribe"`))
	if n := c328Count(diags); n != 1 {
		t.Errorf("C328 x%d on subscribe named bare and qualified, want 1", n)
	}
	diags = c328Diags(t, agentSrc(`create_issue, "mcp.iterion_board.*"`))
	// The wildcard covers the 12 board tools; create_issue was already said.
	if n := c328Count(diags); n != 12 {
		t.Errorf("C328 x%d on create_issue + board wildcard, want 12 (each tool once)", n)
	}
}

// C328 keeps its own copies of the host-tool names (HostToolCapability's
// switch, expandHostToolEntry's family lists) because the registry lives
// above the IR layer. toolcatalog.internalMCPShorthands is the leaf-level
// copy, itself pinned to the live registry by pkg/backend/tool's
// TestInternalMCPShorthandsMatchRegistry — so pinning C328's set to it closes
// the chain: a tool the runtime adds or renames breaks a test here before it
// silently escapes the warning, and a name C328 invents is caught the same
// way.
func TestC328_HostToolNamesMatchTheShorthandCatalog(t *testing.T) {
	// C328's known set, read through the wildcard expansion it ships.
	known := map[string]bool{}
	for _, family := range []string{"board", "watch", "runs"} {
		for _, name := range expandHostToolEntry("mcp.iterion_" + family + ".*") {
			known[name] = true
			if _, ok := HostToolCapability(name); !ok {
				t.Errorf("family %q expands to %q, which HostToolCapability does not map to a capability", family, name)
			}
		}
	}
	catalog := map[string]bool{}
	for _, name := range toolcatalog.InternalMCPShorthands() {
		catalog[name] = true
		if _, ok := HostToolCapability(name); !ok {
			t.Errorf("internal MCP tool %q resolves via the registry's shorthand path but HostToolCapability has no row — C328 would never warn on it", name)
		}
		if !known[name] {
			t.Errorf("internal MCP tool %q is in no family expandHostToolEntry expands — a %q wildcard would miss it", name, "mcp.iterion_<family>.*")
		}
	}
	for name := range known {
		if !catalog[name] {
			t.Errorf("C328 names %q as a host tool, but toolcatalog's shorthand set (pinned to the live registry) does not — a ghost name", name)
		}
	}
}
