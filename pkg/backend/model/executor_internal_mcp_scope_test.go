package model

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/mcp"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/boardcaps"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/store"
)

// iterion's own in-process MCP families — board, runs, watch — are registered
// from a node's `capabilities:` grant and are not catalog entries any `mcp:`
// block can name. The node's resolved MCP set therefore has no say over them,
// and the ambient half of that set (the target repo's `.mcp.json`, the plugin
// catalog) used to decide the verdict: with an EMPTY set every capability tool
// resolved, and with this repo's own pair — engine + sentry — the same node
// died on `server "iterion_board" is not active`. Same binary, same bot,
// opposite answer, decided by a file the bot never mentions.
//
// The class is three bots / six nodes: bots/issue-triage/main.bot `triage`,
// bots/evolve/main.bot `survey` / `emit_backlog` / `load_nexie_handoff`,
// bots/copilot/main.bot `copi` / `reflect`.
func TestTheNodesMCPScopeDoesNotGateIterionsOwnServers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		dotted,
		fqn string
		caps []string
	}{
		{"board", "mcp.iterion_board.add_labels", "mcp__iterion_board__add_labels", []string{boardcaps.BoardLabel}},
		{"runs", "mcp.iterion_runs.runs_list", "mcp__iterion_runs__runs_list", []string{runops.CapRunsRead}},
		{"watch", "mcp.iterion_watch.subscribe", "mcp__iterion_watch__subscribe", []string{ir.CapWatchSubscribe}},
	} {
		for _, spelling := range []struct {
			label, name string
		}{{"dotted", tc.dotted}, {"fqn", tc.fqn}} {
			t.Run(tc.name+"/"+spelling.label, func(t *testing.T) {
				t.Parallel()
				e := internalMCPExecutor(t)
				// The ambient pair this repo's own .mcp.json contributes.
				node := &ir.AgentNode{
					BaseNode:         ir.BaseNode{ID: "triage"},
					Tools:            []string{"skill"},
					Capabilities:     tc.caps,
					ActiveMCPServers: []string{"engine", "sentry"},
				}
				defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{"skill", spelling.name})
				if err != nil {
					t.Fatalf("a capability-granted tool of iterion's own namespace must resolve whatever the ambient MCP set holds: %v", err)
				}
				if !slices.Contains(qualifiedToolNames(defs), tc.dotted) {
					t.Fatalf("%q resolved to %v, want it to carry %q", spelling.name, qualifiedToolNames(defs), tc.dotted)
				}
			})
		}
	}
}

// The exemption is a namespace, not a blanket lift: a server the node's `mcp:`
// scope excludes keeps its refusal, with the reason it always had.
func TestANodeScopeStillRefusesAnInactiveThirdPartyServer(t *testing.T) {
	t.Parallel()
	e := internalMCPExecutor(t)
	node := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "triage"},
		Tools:            []string{"skill"},
		Capabilities:     boardops.AllCapabilities(),
		ActiveMCPServers: []string{"engine", "sentry"},
	}
	_, _, err := e.resolveToolsForNode(context.Background(), node, []string{"mcp.slack.post_message"})
	if err == nil {
		t.Fatal("a third-party server outside the node's MCP scope must stay refused")
	}
	if !strings.Contains(err.Error(), `server "slack" is not active`) {
		t.Fatalf("the refusal must keep naming the inactive server: %v", err)
	}
}

// What replaces the MCP scope on iterion's own servers is the GRANT. The
// registry is built with boardops.AllCapabilities() (pkg/runview/executor.go)
// and boardops re-reads those same all-caps when the tool executes, so no
// per-node check stood anywhere else: a node holding board.read resolved
// create_issue and the write landed on the operator's board. The capability
// that opens the tool is the boundary, and the refusal names it — which is
// what C328 promises at compile time (pkg/dsl/ir/validate_host_tools.go).
func TestTheReservedMCPNamespaceIsGatedByTheCapabilityGrant(t *testing.T) {
	t.Parallel()
	for _, ambient := range []struct {
		label   string
		servers []string
	}{
		{"an empty ambient set", nil},
		{"the repo's own ambient pair", []string{"engine", "sentry"}},
	} {
		t.Run(ambient.label, func(t *testing.T) {
			t.Parallel()
			e := internalMCPExecutor(t)
			node := &ir.AgentNode{
				BaseNode:         ir.BaseNode{ID: "triage"},
				Tools:            []string{"skill"},
				Capabilities:     []string{boardcaps.BoardRead},
				ActiveMCPServers: ambient.servers,
			}
			for _, granted := range []string{"mcp.iterion_board.get_issue", "mcp.iterion_board.list_issues"} {
				if _, _, err := e.resolveToolsForNode(context.Background(), node, []string{granted}); err != nil {
					t.Fatalf("%q is opened by board.read: %v", granted, err)
				}
			}
			for _, tc := range []struct{ name, capability string }{
				{"mcp.iterion_board.create_issue", boardcaps.BoardCreate},
				{"mcp.iterion_board.close_issue", boardcaps.BoardClose},
				{"mcp__iterion_board__transition_issue", boardcaps.BoardMove},
			} {
				_, _, err := e.resolveToolsForNode(context.Background(), node, []string{tc.name})
				if err == nil {
					t.Fatalf("%q writes the operator's board and the node holds no %q", tc.name, tc.capability)
				}
				if !strings.Contains(err.Error(), tc.capability) {
					t.Errorf("the refusal of %q must name the missing capability %q: %v", tc.name, tc.capability, err)
				}
			}
		})
	}
}

// A wildcard is a request for what the node MAY have, so it narrows to the
// grant instead of failing on the first name the grant does not open — the
// other half of C328's "dropped or its call refused". It also must not try to
// BOOT the server: iterion's families are in-process, the MCP catalog has no
// entry for them, and a server the node is taken to have declared fails the
// node fatally.
func TestAWildcardOverIterionsOwnServerNarrowsToTheGrant(t *testing.T) {
	t.Parallel()
	e := internalMCPExecutor(t)
	// An EMPTY catalog armed to start anything: the wildcard must skip the
	// ensure because the family is in-process, not because a start policy
	// refused it. The unarmed zero value (operator-installed servers only)
	// would let this pass for the wrong reason.
	e.mcpManager = mcp.NewManager(map[string]*mcp.ServerConfig{}, mcp.WithStartPolicy(mcp.StartAllServers))
	node := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "survey"},
		Tools:            []string{"skill"},
		Capabilities:     []string{boardcaps.BoardRead},
		ActiveMCPServers: []string{"engine"},
	}
	defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{"mcp.iterion_board.*"})
	if err != nil {
		t.Fatalf("a wildcard over an in-process family must neither boot it nor fail the node: %v", err)
	}
	got := qualifiedToolNames(defs)
	want := []string{"mcp.iterion_board.get_issue", "mcp.iterion_board.list_issues", "mcp.iterion_board.list_labels"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("mcp.iterion_board.* under board.read resolved %v, want %v", got, want)
	}
}

// The two sites that read the active set for an access decision are covered
// independently: checkNodeToolAccess answers a NAME, mcpServerActiveForNode
// answers a SERVER (and is what decides whether a wildcard is expanded at
// all). Two guards that cover each other would leave either free to regress.
func TestMCPServerActiveForNodeLeavesIterionsOwnServersToTheGrant(t *testing.T) {
	t.Parallel()
	node := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "n"},
		ActiveMCPServers: []string{"engine"},
	}
	for _, server := range []string{"iterion_board", "iterion_runs", "iterion_watch", "iterion_connectors"} {
		if !mcpServerActiveForNode(node, server) {
			t.Errorf("%q is iterion's own in-process family: the node's mcp: scope has no say over it", server)
		}
	}
	for _, server := range []string{"slack", "sentry"} {
		if mcpServerActiveForNode(node, server) {
			t.Errorf("%q is outside the node's MCP scope", server)
		}
	}
}

// Backend parity (AGENTS.md: a capability wired for one backend is wired, or
// typed-refused, for the other). claude_code reads its board server from
// delegate's own wiring whatever the ambient set holds, so it was never
// refused; claw resolves the same names in-process through
// resolveToolsForNode. Both backends must now answer the same for the same
// node, which is what the scope filter broke.
func TestTheBoardCapabilitySurfaceIsTheSameOnClawAndClaudeCode(t *testing.T) {
	t.Parallel()
	for _, backend := range []string{delegate.BackendClaw, delegate.BackendClaudeCode} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			e := internalMCPExecutor(t)
			node := &ir.AgentNode{
				BaseNode:         ir.BaseNode{ID: "triage"},
				LLMFields:        ir.LLMFields{Backend: backend},
				Tools:            []string{"skill"},
				Capabilities:     []string{boardcaps.BoardLabel},
				ActiveMCPServers: []string{"engine", "sentry"},
			}
			advertised := e.EffectiveToolNamesOnRoutes(node, false, []string{backend})
			if !slices.Contains(advertised, "mcp__iterion_board__add_labels") {
				t.Fatalf("%s advertises %v, want the granted board tool", backend, advertised)
			}
			if backend != delegate.BackendClaw {
				// The CLI resolves its own MCP surface out of process; the
				// advertised list IS what it is held to.
				return
			}
			defs, _, err := e.resolveToolsForNode(context.Background(), node, advertised)
			if err != nil {
				t.Fatalf("claw must resolve the same surface claude_code advertises: %v", err)
			}
			if !slices.Contains(qualifiedToolNames(defs), "mcp.iterion_board.add_labels") {
				t.Fatalf("claw resolved %v, want the granted board tool", qualifiedToolNames(defs))
			}
		})
	}
}

// Every tool the in-process registrars put in iterion's namespace must map
// to a capability OF ITS OWN FAMILY. One that does not falls into
// internalMCPToolGrant's "no capability claims this name" arm and is handed
// out ungated — the drift the grant check exists to close, one new tool
// later. Read in the family that SERVES the tool, because the leaf-keyed
// reading answers for any family that shares a leaf name: asserting only
// that SOME capability maps would stay green on exactly that collision. The
// names are READ from the registrars rather than copied here, so a family
// that grows a tool without a capability pin reddens.
func TestEveryToolOfIterionsOwnNamespaceMapsToACapability(t *testing.T) {
	t.Parallel()
	e := internalMCPExecutor(t)
	seen := 0
	for _, td := range e.toolRegistry.ListByOrigin(tool.OriginMCP) {
		if !internalMCPServerName(td.Origin.Server) {
			continue
		}
		seen++
		if _, ok := ir.HostToolCapabilityInFamily(td.Origin.Server, td.QualifiedName); !ok {
			t.Errorf("%q is in iterion's reserved namespace and no capability of server %q gates it", td.QualifiedName, td.Origin.Server)
		}
	}
	if seen == 0 {
		t.Fatal("the registrars put no tool in iterion's namespace — this guard has nothing to judge")
	}
}

// internalMCPExecutor wires the three in-process families through their REAL
// registrars — the same calls pkg/runview/executor.go makes, with the same
// all-capabilities registration — so the witness exercises the surface a run
// gets and not a hand-built stand-in.
func internalMCPExecutor(t *testing.T) *ClawExecutor {
	t.Helper()
	boardStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("open native board store: %v", err)
	}
	t.Cleanup(func() { _ = boardStore.Close() })
	runStore, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("open run store: %v", err)
	}

	tr := tool.NewRegistry()
	if err := tool.RegisterClawBoardTools(tr, &tool.BoardConfig{
		Store:        boardStore,
		Capabilities: boardops.AllCapabilities(),
	}); err != nil {
		t.Fatalf("RegisterClawBoardTools: %v", err)
	}
	if err := tool.RegisterClawRunTools(tr, &tool.RunConfig{
		Store:        runStore,
		Capabilities: []string{runops.CapRunsRead},
	}); err != nil {
		t.Fatalf("RegisterClawRunTools: %v", err)
	}
	if err := tool.RegisterClawWatchTools(tr, &tool.WatchConfig{
		Store:        runStore,
		RunID:        "run-1",
		Capabilities: []string{ir.CapWatchSubscribe, ir.CapWatchUnsubscribe},
	}); err != nil {
		t.Fatalf("RegisterClawWatchTools: %v", err)
	}
	if err := tr.RegisterMCP("slack", "post_message", "Slack post", nil, jsonExec(`{"ok":true}`)); err != nil {
		t.Fatalf("register slack MCP tool: %v", err)
	}
	// `skill` is the node's own declared tool (bots/issue-triage/main.bot);
	// `todo_write` is claw's own append to a restricted list.
	for _, builtin := range []string{"skill", "todo_write"} {
		if err := tr.RegisterBuiltin(builtin, builtin, nil, jsonExec(`{"ok":true}`)); err != nil {
			t.Fatalf("register %q: %v", builtin, err)
		}
	}

	wf := &ir.Workflow{Prompts: map[string]*ir.Prompt{}, Schemas: map[string]*ir.Schema{}}
	return newTestClawExecutor(NewRegistry(), wf, WithToolRegistry(tr))
}

// qualifiedToolNames reads the QUALIFIED name: ToDelegateDef sanitizes Name
// for the provider (`mcp_iterion_board_add_labels`), and a witness asserting
// on the sanitized spelling would stop naming what the node asked for.
func qualifiedToolNames(defs []delegate.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.QualifiedName)
	}
	return out
}

// The exemption lifts the `mcp:` scope off the namespace, so the GRANT is the
// only gate left: a registered tool no capability maps must be refused, not
// handed out. Allowing it was fail-open on the whole `iterion*` prefix —
// permission.IsReservedMCPServerName answers true for far more than the three
// families — and the wildcard narrowing served it too.
func TestAnUnmappedToolOfTheReservedNamespaceIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ label, server, tool string }{
		{"a tool added to an existing family", "iterion_board", "delete_board"},
		{"a family a fourth registrar adds", "iterion_memory", "wipe"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			e := internalMCPExecutor(t)
			if err := e.toolRegistry.RegisterMCP(tc.server, tc.tool, "unpinned", nil, jsonExec(`{"ok":true}`)); err != nil {
				t.Fatalf("register %q: %v", tc.tool, err)
			}
			qualified := "mcp." + tc.server + "." + tc.tool
			if _, ok := ir.HostToolCapabilityInFamily(tc.server, qualified); ok {
				t.Fatalf("%q must be UNMAPPED for this witness to judge the fail-open arm", qualified)
			}
			node := &ir.AgentNode{
				BaseNode: ir.BaseNode{ID: "n"},
				Tools:    []string{"skill"},
				// Everything the three families map, so the refusal cannot be
				// read as a missing grant: no capability claims this name.
				Capabilities:     append(boardops.AllCapabilities(), runops.CapRunsRead, ir.CapWatchSubscribe),
				ActiveMCPServers: []string{"engine", "sentry"},
			}
			_, _, err := e.resolveToolsForNode(context.Background(), node, []string{qualified})
			if err == nil {
				t.Fatalf("%q is in iterion's reserved namespace and no capability gates it: it must be refused, not served", qualified)
			}
			if !strings.Contains(err.Error(), "no capability gates it") {
				t.Errorf("the refusal must say why: %v", err)
			}
		})
	}
}

// The wildcard half of the same rule: expandInternalMCPWildcard drops what
// the grant does not open, and an unmapped tool is not opened by any grant.
func TestAWildcardDoesNotServeAnUnmappedToolOfTheFamily(t *testing.T) {
	t.Parallel()
	e := internalMCPExecutor(t)
	e.mcpManager = mcp.NewManager(map[string]*mcp.ServerConfig{}, mcp.WithStartPolicy(mcp.StartAllServers))
	if err := e.toolRegistry.RegisterMCP("iterion_board", "delete_board", "unpinned", nil, jsonExec(`{"ok":true}`)); err != nil {
		t.Fatalf("register delete_board: %v", err)
	}
	node := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "survey"},
		Tools:            []string{"skill"},
		Capabilities:     boardops.AllCapabilities(),
		ActiveMCPServers: []string{"engine"},
	}
	defs, _, err := e.resolveToolsForNode(context.Background(), node, []string{"mcp.iterion_board.*"})
	if err != nil {
		t.Fatalf("a wildcard narrows, it does not fail the node: %v", err)
	}
	if got := qualifiedToolNames(defs); slices.Contains(got, "mcp.iterion_board.delete_board") {
		t.Fatalf("the wildcard served an unmapped tool to a node holding every mapped capability: %v", got)
	}
}

// The grant the check reads is the production seam's: the node's declared
// list, and the workflow's only where the node declares NONE. A declared
// empty `capabilities: []` (#1801) is an explicit no-grant — the one
// distinction a `len(caps) == 0` reading loses.
func TestTheGrantSeamReadsTheDeclaredListThenTheWorkflows(t *testing.T) {
	t.Parallel()
	t.Run("a declared empty list holds nothing", func(t *testing.T) {
		t.Parallel()
		e := internalMCPExecutor(t)
		e.wfCapabilities = []string{boardcaps.BoardRead}
		node := &ir.AgentNode{
			BaseNode:     ir.BaseNode{ID: "n"},
			Tools:        []string{"skill"},
			Capabilities: []string{},
		}
		_, _, err := e.resolveToolsForNode(context.Background(), node, []string{"mcp.iterion_board.get_issue"})
		if err == nil {
			t.Fatal("`capabilities: []` is an explicit no-grant: it must not inherit the workflow's board.read")
		}
		if !strings.Contains(err.Error(), boardcaps.BoardRead) {
			t.Errorf("the refusal must name the missing capability: %v", err)
		}
	})
	t.Run("no declared list inherits the workflow's", func(t *testing.T) {
		t.Parallel()
		e := internalMCPExecutor(t)
		e.wfCapabilities = []string{boardcaps.BoardRead}
		node := &ir.AgentNode{
			BaseNode: ir.BaseNode{ID: "n"},
			Tools:    []string{"skill"},
		}
		if _, _, err := e.resolveToolsForNode(context.Background(), node, []string{"mcp.iterion_board.get_issue"}); err != nil {
			t.Fatalf("the workflow's board.read opens get_issue: %v", err)
		}
		_, _, err := e.resolveToolsForNode(context.Background(), node, []string{"mcp.iterion_board.create_issue"})
		if err == nil {
			t.Fatal("the workflow's board.read does not open create_issue")
		}
	})
}

// Parity of the REFUSAL side. A node that NAMES an ungranted board tool in
// its `tools:` is refused on both routes, and the advertised surface carries
// the author's name on both (it reports what the DECLARATION cannot bound,
// which pre-run admission reads as an upper bound). Where the refusal lands
// differs and is not symmetric: claw raises at build, so the node dies before
// its `fallbacks:` are walked, while claude_code's board server — the same
// boardops surface, served from ITERION_BOARD_CAPS (cmd/iterion/mcp_board.go)
// — never lists the tool and denies the call. Both are C328's "dropped or its
// call refused"; the claw half is the harsher reading of it.
func TestAnUngrantedBoardToolIsRefusedOnBothBackends(t *testing.T) {
	t.Parallel()
	const named = "mcp__iterion_board__create_issue"
	for _, backend := range []string{delegate.BackendClaw, delegate.BackendClaudeCode} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			e := internalMCPExecutor(t)
			node := &ir.AgentNode{
				BaseNode:         ir.BaseNode{ID: "n"},
				LLMFields:        ir.LLMFields{Backend: backend},
				Tools:            []string{"skill", named},
				Capabilities:     []string{boardcaps.BoardRead},
				ActiveMCPServers: []string{"engine", "sentry"},
			}
			advertised := e.EffectiveToolNamesOnRoutes(node, false, []string{backend})
			if !slices.Contains(advertised, named) {
				t.Fatalf("%s must keep reporting the author's declared name: %v", backend, advertised)
			}
			if backend == delegate.BackendClaw {
				_, _, err := e.resolveToolsForNode(context.Background(), node, advertised)
				if err == nil {
					t.Fatal("claw must refuse a named board tool the node holds no capability for")
				}
				if !strings.Contains(err.Error(), boardcaps.BoardCreate) {
					t.Errorf("claw's refusal must name the missing capability: %v", err)
				}
				return
			}
			// The CLI route's refusal is its board server's, and the grant
			// that server receives is the TASK's: both CLI backends spawn it
			// with delegate.BoardServerEnv(task) — claude_code's wireBoardMCP
			// and pi's spec builder — whose ITERION_BOARD_CAPS is what
			// cmd/iterion's board subcommand reads back into boardops, and
			// delegate's own witness pins the stdio spawn configuration to
			// that derivation. So the task is built by the real buildTask
			// and the caps are READ off the env the wiring sends. Re-deriving
			// them from node.Capabilities here asserted a boardops property
			// and stayed green on a wiring that sent the whole board surface.
			f, err := extractBackendFields(node)
			if err != nil {
				t.Fatalf("extractBackendFields: %v", err)
			}
			task, err := e.buildTask(context.Background(), node, f, map[string]any{}, backend, nil)
			if err != nil {
				t.Fatalf("buildTask: %v", err)
			}
			if got := delegate.BoardToolsFor(task.Capabilities); slices.Contains(got, named) {
				t.Fatalf("%s's board extras carry %q under %v: %v", backend, named, task.Capabilities, got)
			}
			env := delegate.BoardServerEnv(task)
			caps := boardops.NewCapabilities(env["ITERION_BOARD_CAPS"])
			for _, listed := range boardops.ToolsFor(caps) {
				if listed.Name == "create_issue" {
					t.Fatalf("%s's board server must not LIST create_issue under %v", backend, task.Capabilities)
				}
			}
			boardStore, err := native.NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("open native board store: %v", err)
			}
			t.Cleanup(func() { _ = boardStore.Close() })
			_, callErr := boardops.CallWithEnv(boardStore, caps, "create_issue",
				json.RawMessage(`{"title":"x"}`), boardops.CallEnv{})
			if !errors.Is(callErr, boardops.ErrCapabilityDenied) {
				t.Fatalf("%s's board server must deny the call: %v", backend, callErr)
			}
		})
	}
}

// The exemption is keyed on the reserved NAMESPACE — the whole `iterion*`
// prefix (permission.IsReservedMCPServerName), far more than the three
// shipped families — while the capability that replaces the `mcp:` scope is a
// property of a TOOL, and a leaf name alone does not say who serves it. So a
// fourth registrar's `get_issue` was opened by board.read: the three shipped
// families happen to hold disjoint leaves, which made it latent rather than
// live. The grant is read in the family that SERVES the tool.
func TestAReservedFamilyIsNotOpenedByAnotherFamilysCapability(t *testing.T) {
	t.Parallel()
	const qualified = "mcp.iterion_memory.get_issue"
	// The trap, stated: the family-BLIND reading C328 needs for a bare
	// `tools:` entry maps this very name to board.read.
	if capability, ok := ir.HostToolCapability(qualified); !ok || capability != boardcaps.BoardRead {
		t.Fatalf("premise: ir.HostToolCapability(%q) = %q, %v — this witness judges the leaf collision", qualified, capability, ok)
	}
	e := internalMCPExecutor(t)
	e.mcpManager = mcp.NewManager(map[string]*mcp.ServerConfig{}, mcp.WithStartPolicy(mcp.StartAllServers))
	if err := e.toolRegistry.RegisterMCP("iterion_memory", "get_issue", "a fourth family's own read", nil, jsonExec(`{"ok":true}`)); err != nil {
		t.Fatalf("register iterion_memory.get_issue: %v", err)
	}
	node := &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "n"},
		Tools:    []string{"skill"},
		// Every capability the three shipped families map, so the refusal
		// cannot be read as a missing grant: it is about the FAMILY.
		Capabilities:     append(boardops.AllCapabilities(), runops.CapRunsRead, ir.CapWatchSubscribe, ir.CapWatchUnsubscribe),
		ActiveMCPServers: []string{"engine", "sentry"},
	}
	_, _, err := e.resolveToolsForNode(context.Background(), node, []string{qualified})
	if err == nil {
		t.Fatalf("%q belongs to no family a capability table describes: %q must not open it", qualified, boardcaps.BoardRead)
	}
	if !strings.Contains(err.Error(), "iterion_memory") {
		t.Errorf("the refusal must name the server whose capabilities were asked for: %v", err)
	}
	defs, _, werr := e.resolveToolsForNode(context.Background(), node, []string{"mcp.iterion_memory.*"})
	if werr != nil {
		t.Fatalf("the family IS registered, so the wildcard narrows rather than failing the node: %v", werr)
	}
	if got := qualifiedToolNames(defs); slices.Contains(got, qualified) {
		t.Fatalf("the wildcard served %q under another family's capability: %v", qualified, got)
	}
}

// A reserved family with no tool in this route's registry is a declared
// dependency the route cannot serve, not a grant that opens nothing. The
// registrars are conditional — pkg/runview/executor.go needs a dispatcher
// store for board and a run store for watch/runs, and iterion_connectors is
// delegate/HTTP-only, so it is never in a claw registry — and before the
// namespace exemption the ensure answered such a wildcard with `unknown
// server`. It must not come back silently with zero tools.
func TestAWildcardOverAnUnregisteredReservedFamilyFailsTheNode(t *testing.T) {
	t.Parallel()
	e := internalMCPExecutor(t)
	e.mcpManager = mcp.NewManager(map[string]*mcp.ServerConfig{}, mcp.WithStartPolicy(mcp.StartAllServers))
	if got := len(e.toolRegistry.ListByServer("iterion_connectors")); got != 0 {
		t.Fatalf("premise: the connector facade is delegate/HTTP-only, so the claw registry holds none of it, got %d tools", got)
	}
	node := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "n"},
		Tools:            []string{"skill"},
		Capabilities:     []string{connection.CapConnectors, boardcaps.BoardRead},
		ActiveMCPServers: []string{"engine"},
	}
	_, _, err := e.resolveToolsForNode(context.Background(), node, []string{"mcp.iterion_connectors.*"})
	if err == nil {
		t.Fatal("a wildcard over a family this route does not register must fail the node, not resolve to nothing")
	}
	if !strings.Contains(err.Error(), "iterion_connectors") || !strings.Contains(err.Error(), "no tool registered on this route") {
		t.Errorf("the refusal must name the family and say it is not served here: %v", err)
	}
	// The distinction, both ways: a family that IS registered and whose
	// tools the grant does not open is dropped with a warning.
	bare := &ir.AgentNode{
		BaseNode:         ir.BaseNode{ID: "n"},
		Tools:            []string{"skill"},
		Capabilities:     []string{},
		ActiveMCPServers: []string{"engine"},
	}
	defs, _, err := e.resolveToolsForNode(context.Background(), bare, []string{"mcp.iterion_board.*"})
	if err != nil {
		t.Fatalf("a registered family the grant does not open is a drop, not a failure: %v", err)
	}
	if got := qualifiedToolNames(defs); len(got) != 0 {
		t.Fatalf("`capabilities: []` opens no board tool, got %v", got)
	}
}
