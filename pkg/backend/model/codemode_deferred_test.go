package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native/boardops"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runops"
	"github.com/SocialGouv/iterion/pkg/scripthost"
	"github.com/SocialGouv/iterion/pkg/scripthost/toolindex"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// deferredToolDefs is a node surface in the shape the executor resolves it:
// two AUTHOR-declared tools (the deferral's candidates), one author-declared
// tool the withheld class claims anyway (an author may write `ask_user` in
// `tools:` — the veto must keep it declared whatever the marker says), and
// the runtime's own append (the todo board), which never carries the marker.
func deferredToolDefs() []delegate.ToolDef {
	echo := func(context.Context, json.RawMessage) (string, error) { return "echoed", nil }
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	defs := []delegate.ToolDef{
		{Name: "read_file", Description: "Read a file from the workspace.", InputSchema: schema, Execute: echo, AuthorDeclared: true},
		{Name: "grep", Description: "Search the workspace.", InputSchema: schema, Execute: echo, AuthorDeclared: true},
		{Name: "todo_write", Description: "Maintain the task list.", Execute: echo},
		{Name: "ask_user", Description: "Ask the operator.", Execute: echo, AuthorDeclared: true},
	}
	return defs
}

// wireTools is a deferring node's tools as the claw backend leaves them:
// converted from the ToolDefs, then demoted by the real install — one bound,
// the marking, the codemode tool appended — with that trailing codemode tool
// stripped, which is what ParentLoop.Tools and the witnesses below read.
func wireTools(defs []delegate.ToolDef) []GenerationTool {
	opts := GenerationOptions{Tools: toolDefsToGeneration(defs)}
	if err := installCodemode(&opts, true, nil, nil, nil); err != nil {
		panic(err)
	}
	return opts.Tools[:len(opts.Tools)-1]
}

func requestedToolNames(cap *requestCapturingClient) []string {
	var names []string
	for _, req := range cap.requests {
		for _, tl := range req.Tools {
			names = append(names, tl.Name)
		}
	}
	return names
}

func runDeferredNode(t *testing.T, backend *ClawBackend, deferOn bool) {
	t.Helper()
	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:        "n",
		Model:         "test/test-model",
		UserPrompt:    "go",
		HasTools:      true,
		ToolDefs:      deferredToolDefs(),
		Codemode:      true,
		CodemodeDefer: deferOn,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Witness 1 — the capture witness: what the wire carries, what the catalogue
// holds.
// ---------------------------------------------------------------------------

func TestADeferredNodeDeclaresOnlyTheWithheldSet(t *testing.T) {
	t.Parallel()
	backend, cap := newCapturingBackend()
	runDeferredNode(t, backend, true)

	got := requestedToolNames(cap)
	// The author's tools are off the wire.
	for _, name := range []string{"read_file", "grep"} {
		if slices.Contains(got, name) {
			t.Errorf("defer on: %q is declared to the model — the deferral did not reach the request", name)
		}
	}
	// The withheld class and the runtime's own appends stay declared, beside
	// the codemode tool itself.
	for _, name := range []string{"ask_user", "todo_write", ScriptHostToolName} {
		if !slices.Contains(got, name) {
			t.Errorf("defer on: %q is missing from the request — only the author's tools leave the wire (got %v)", name, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("defer on: the request declares %v, want exactly ask_user, todo_write and codemode", got)
	}

	// The cache breakpoint rides the FILTERED tail: the marker lands on the
	// last tool that is actually declared, so the deferred ones leaving the
	// array does not strand the prefix beyond the last real entry.
	last := cap.requests[len(cap.requests)-1].Tools
	if n := len(last); n == 0 || last[n-1].CacheControl == nil {
		t.Errorf("the tools-array cache breakpoint is missing from the last declared tool (%d tool(s))", len(last))
	}

	// The catalogue the model reads — the codemode tool's description index —
	// still lists what the wire does not: the union is the catalogue, the
	// declared set is not.
	for _, req := range cap.requests {
		for _, tl := range req.Tools {
			if tl.Name != ScriptHostToolName {
				continue
			}
			for _, name := range []string{"read_file", "grep"} {
				if !strings.Contains(tl.Description, "\n- "+name+" ") {
					t.Errorf("the deferred index does not list %q — a tool off the wire must stay discoverable", name)
				}
			}
			if !strings.Contains(tl.Description, codemodeDeferredComplement) {
				t.Error("the deferred node's description does not carry the catalogue-only complement")
			}
		}
	}
}

// Defer off is today's behaviour: every tool declared, no complement, and the
// request carries nothing the undeferred install would not have carried.
func TestADeferOffNodeIsByteIdenticalToToday(t *testing.T) {
	t.Parallel()
	backend, cap := newCapturingBackend()
	runDeferredNode(t, backend, false)

	got := requestedToolNames(cap)
	for _, name := range []string{"read_file", "grep", "todo_write", "ask_user", ScriptHostToolName} {
		if !slices.Contains(got, name) {
			t.Errorf("defer off: %q is missing from the request — the deferral must not reach an undeferred node (got %v)", name, got)
		}
	}
	if len(got) != 5 {
		t.Errorf("defer off: the request declares %v, want the full set", got)
	}
	for _, req := range cap.requests {
		for _, tl := range req.Tools {
			if tl.Name == ScriptHostToolName && strings.Contains(tl.Description, codemodeDeferredComplement) {
				t.Error("defer off: the description gained the catalogue-only complement")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Witness 2 — real-child discovery: a deferred tool is found, described and
// run through the door; the ask channel stays refused with its direct-call
// wording.
// ---------------------------------------------------------------------------

func TestAProgramDiscoversADeferredToolThroughTheDoor(t *testing.T) {
	t.Parallel()

	// The author's tools of the discovery fixture, demoted by hand the way
	// markCatalogueOnly demotes a resolved author tool; the withheld class
	// (ask_user, agent, the board tool) keeps its declared place.
	tools := discoveryTools()
	for i := range tools {
		switch tools[i].Name {
		case "read_file", "list_dir":
			tools[i].catalogueOnly = true
		}
	}
	opts := GenerationOptions{Tools: tools}
	if err := installCodemodeOn(&opts, childHost(), false, &scripthost.Limits{Memory: 1 << 30}); err != nil {
		t.Fatalf("installCodemodeOn: %v", err)
	}
	cm := opts.Tools[len(opts.Tools)-1]
	_, ctx := newToolDispatcher(context.Background(), buildToolMap(opts.Tools), nil, nil, nil, nil, nil)
	in, err := json.Marshal(map[string]string{"source": `
		const hit = call.search("read a file")[0];
		let declared = "";
		try { declared = call.describe(hit.name); } catch (e) { declared = "UNDESCRIBED"; }
		let ran = "";
		try { ran = call(hit.name, { path: "/w/a" }).text; } catch (e) { ran = "UNRUNNABLE"; }
		let askable = "found";
		try { call("ask_user", { question: "?" }); askable = "ANSWERED"; } catch (e) { askable = String(e.text || e); }
		[hit.name, declared.includes("path"), ran, askable.includes("directly")]
	`})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cm.Execute(ctx, in)
	if err != nil {
		t.Fatalf("the program failed: %v", err)
	}
	if out != `["read_file",true,"echoed",true]` {
		t.Fatalf("the program returned %s", out)
	}
}

// ---------------------------------------------------------------------------
// Witness 3 — the direct path still asks: ask_user stays declared on a
// deferred node, and its tool_use raises the pause the declared set promises.
// ---------------------------------------------------------------------------

func TestADeferredNodeStillAsksOnTheDirectPath(t *testing.T) {
	t.Parallel()

	// ask_user is the fixture the L1/L3 loop tests drive the pause with
	// (claw_backend_test.go): the handler raises the suspension the real
	// native tool raises. Declared, never deferred — the withheld class.
	defs := deferredToolDefs()
	for i := range defs {
		if defs[i].Name == "ask_user" {
			defs[i] = askUserToolDef()
			// Written by the author: the withheld class must hold whatever
			// the marker says.
			defs[i].AuthorDeclared = true
		}
	}
	tools := wireTools(defs)

	toolMap := buildToolMap(tools)
	askTool, declared := toolMap["ask_user"]
	if !declared {
		t.Fatal("ask_user is not in the deferred node's tool map — the withheld class must stay declared")
	}
	if askTool.catalogueOnly {
		t.Fatal("ask_user was demoted off the wire — the withheld class must stay declared")
	}
	if !anyCatalogueOnly(tools) {
		t.Fatal("fixture: the node defers nothing — this witness would pass over a dead deferral")
	}
	// The funnel builds its own dispatcher from the context it is given, so
	// this drives the direct path exactly as the loop does.
	_, abort := executeToolsDirect(context.Background(), []toolUseBlock{
		{ID: "tu_1", Name: "ask_user", PartialJSON: `{"question":"ship it?"}`},
	}, toolMap, nil, nil, nil, nil, nil)
	if abort == nil {
		t.Fatal("a direct ask_user on a deferred node returned no abort — the declared ask channel must still pause the run")
	}
	var ask *delegate.ErrAskUser
	if !errors.As(abort, &ask) {
		t.Fatalf("the abort is %T, want *delegate.ErrAskUser", abort)
	}
	if ask.Question != "ship it?" {
		t.Errorf("the pause lost the question: %q", ask.Question)
	}
}

// ---------------------------------------------------------------------------
// Witness 4 — the posture appends stay declared, and the posture text still
// keys on the declared todo_write.
// ---------------------------------------------------------------------------

func TestThePostureAppendsStayDeclaredUnderDefer(t *testing.T) {
	t.Parallel()

	defs := []delegate.ToolDef{
		{Name: "grep", Description: "Search the workspace.", InputSchema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { return "echoed", nil }, AuthorDeclared: true},
		{Name: "todo_write", Description: "Maintain the task list.", Execute: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }},
		{Name: "write_file", Description: "Write a file.", InputSchema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }},
		{Name: "glob", Description: "Find files.", InputSchema: json.RawMessage(`{"type":"object"}`), Execute: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }},
	}
	backend, cap := newCapturingBackend()
	_, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:        "n",
		Model:         "test/test-model",
		UserPrompt:    "go",
		HasTools:      true,
		ToolDefs:      defs,
		Codemode:      true,
		CodemodeDefer: true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := requestedToolNames(cap)
	for _, name := range []string{"todo_write", "write_file", "glob", ScriptHostToolName} {
		if !slices.Contains(got, name) {
			t.Errorf("defer on: the posture append %q left the wire (got %v) — the runtime's own appends stay declared", name, got)
		}
	}
	if slices.Contains(got, "grep") {
		t.Errorf("defer on: the author tool grep is still declared (got %v)", got)
	}
	// The posture text keys on the DECLARED tool and stays armed: todo_write
	// is in the request, so the reseed nudge still sees its tool.
	if !hasTodoTool(wireTools(defs)) {
		t.Error("hasTodoTool lost the declared todo_write on a deferred node — the posture reseed would go blind")
	}
}

// ---------------------------------------------------------------------------
// Witness 5 — a child conversation does NOT inherit the deferral: it runs
// GenerateTextDirect with no codemode program, so a tool its parent deferred
// would be reachable neither directly nor from a script. filterSubagentTools
// clears the flag on everything it grants — one place, both call sites.
// ---------------------------------------------------------------------------

func TestAChildOfADeferringNodeDeclaresTheToolsItsFilterGrants(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		subagentType string
		want         []string
	}{
		// explore's allowlist names read_file and grep: both must reach the
		// child's wire although the parent deferred them.
		{"explore", []string{"read_file", "grep"}},
		// general-purpose grants the node's set minus the child's own
		// exclusions (agent, the ask channel): every remaining tool declared.
		{"", []string{"read_file", "grep", "todo_write"}},
	} {
		t.Run("subagent_type="+c.subagentType, func(t *testing.T) {
			t.Parallel()
			reg := NewRegistry()
			childCap := &requestCapturingClient{}
			reg.Register("test", func(modelID string) (api.APIClient, error) { return childCap, nil })

			// ParentLoop.Tools is the parent's slice as the loop arms it:
			// marked catalogue-only, before codemode joins it.
			parent := ParentLoop{Model: "test/test-model", Tools: wireTools(deferredToolDefs()), MaxSteps: 4}
			for _, pt := range parent.Tools {
				if (pt.Name == "read_file" || pt.Name == "grep") && !pt.catalogueOnly {
					t.Fatalf("fixture: the parent's %s is not catalogue-only — this witness proves nothing", pt.Name)
				}
			}
			ctx := WithParentLoop(context.Background(), parent)

			run := NewSubagentRunner(reg, tool.NewRegistry(), EventHooks{}, nil, "")
			input := map[string]any{"prompt": "go", "description": "witness", "model": "test/test-model"}
			if c.subagentType != "" {
				input["subagent_type"] = c.subagentType
			}
			if _, err := run(ctx, input); err != nil {
				t.Fatalf("the child conversation failed: %v", err)
			}
			got := requestedToolNames(childCap)
			for _, name := range c.want {
				if !slices.Contains(got, name) {
					t.Errorf("the child does not declare %q (got %v) — a child has no codemode program, so a deferred tool it is granted must stay on its wire", name, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Witness 7 — byte stability of the description per demoted set, and the
// complement only where something was demoted (F4).
// ---------------------------------------------------------------------------

// demotedDiscoveryTools is the discovery fixture with its author tools
// demoted the way markCatalogueOnly demotes them.
func demotedDiscoveryTools() []GenerationTool {
	tools := discoveryTools()
	for i := range tools {
		switch tools[i].Name {
		case "read_file", "list_dir":
			tools[i].catalogueOnly = true
		}
	}
	return tools
}

func installedDescription(t *testing.T, tools []GenerationTool) string {
	t.Helper()
	opts := GenerationOptions{Tools: append([]GenerationTool(nil), tools...)}
	if err := installCodemode(&opts, false, nil, nil, nil); err != nil {
		t.Fatalf("installCodemode: %v", err)
	}
	return opts.Tools[len(opts.Tools)-1].Description
}

func TestTheDeferredDescriptionIsByteStable(t *testing.T) {
	t.Parallel()

	forward := installedDescription(t, demotedDiscoveryTools())
	demoted := demotedDiscoveryTools()
	reversed := make([]GenerationTool, len(demoted))
	for i, tl := range demoted {
		reversed[len(demoted)-1-i] = tl
	}
	if backward := installedDescription(t, reversed); forward != backward {
		t.Fatalf("the deferred description depends on registration order:\n%s\n---\n%s", forward, backward)
	}
	if !strings.HasPrefix(forward, codemodeDescription+"\n\n"+codemodeDeferredComplement+"\n\nTools a program can call:\n") {
		t.Errorf("the deferred description does not read preamble, complement, index:\n%s", forward[:min(len(forward), 400)])
	}

	plain := installedDescription(t, discoveryTools())
	if strings.Contains(plain, codemodeDeferredComplement) {
		t.Error("the undeferred description gained the catalogue-only complement")
	}
	if plain == forward {
		t.Error("the demoted set does not change the description — the model cannot tell a deferring node from a declared one")
	}
}

// A deferring node whose author tools are all withheld or runtime-appended
// demotes nothing, and must not pay for a complement that would be false.
// Driven through the backend, where the task says defer and the set is empty.
func TestADeferringNodeThatDemotesNothingCarriesNoComplement(t *testing.T) {
	t.Parallel()
	echo := func(context.Context, json.RawMessage) (string, error) { return "ok", nil }
	backend, cap := newCapturingBackend()
	if _, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:     "n",
		Model:      "test/test-model",
		UserPrompt: "go",
		HasTools:   true,
		// Author-declared, but each one withheld: nothing is demotable.
		ToolDefs: []delegate.ToolDef{
			{Name: "ask_user", Description: "Ask the operator.", Execute: echo, AuthorDeclared: true},
			{Name: "agent", Description: "Start a sub-agent.", Execute: echo, AuthorDeclared: true},
		},
		Codemode:      true,
		CodemodeDefer: true,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	found := false
	for _, req := range cap.requests {
		for _, tl := range req.Tools {
			if tl.Name != ScriptHostToolName {
				continue
			}
			found = true
			if strings.Contains(tl.Description, codemodeDeferredComplement) {
				t.Error("a deferring node that demoted nothing carries the catalogue-only complement — bytes for a false sentence")
			}
		}
	}
	if !found {
		t.Fatal("no codemode tool on the wire — the fixture proves nothing")
	}
}

// ---------------------------------------------------------------------------
// Witness 10 — the measurement: the real families through the real
// registry, and a synthetic stress arm, counted and logged both sides.
// ---------------------------------------------------------------------------

// measuredRun is what one Execute put on the wire.
type measuredRun struct {
	count int
	bytes int
	names []string
}

func runMeasured(t *testing.T, defs []delegate.ToolDef, deferOn bool) measuredRun {
	t.Helper()
	reg := NewRegistry()
	cap := &requestCapturingClient{}
	reg.Register("test", func(modelID string) (api.APIClient, error) { return cap, nil })
	backend := NewClawBackend(reg, EventHooks{}, RetryPolicy{})
	if _, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:        "n",
		Model:         "test/test-model",
		UserPrompt:    "go",
		HasTools:      true,
		ToolDefs:      defs,
		Codemode:      true,
		CodemodeDefer: deferOn,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(cap.requests) == 0 {
		t.Fatal("no request captured")
	}
	raw, err := json.Marshal(cap.requests[len(cap.requests)-1].Tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	run := measuredRun{bytes: len(raw)}
	for _, tl := range cap.requests[len(cap.requests)-1].Tools {
		run.names = append(run.names, tl.Name)
	}
	run.count = len(run.names)
	return run
}

func TestTheDeferralMeasuredOnTheRealFamilies(t *testing.T) {
	t.Parallel()

	// The real in-process families through the real registry, resolved the
	// way buildTask resolves them: the author's own entry (`skill`), the
	// posture append (`todo_write`), and three capability-granted family
	// tools. Author: skill. Posture/infrastructure: the rest.
	e := internalMCPExecutor(t)
	node := &ir.AgentNode{
		BaseNode:     ir.BaseNode{ID: "triage"},
		Tools:        []string{"skill"},
		Capabilities: slices.Concat(boardops.AllCapabilities(), []string{runops.CapRunsRead, ir.CapWatchSubscribe}),
	}
	names := []string{"skill", "todo_write", "mcp.iterion_board.add_labels", "mcp.iterion_runs.runs_list", "mcp__iterion_watch__subscribe"}
	defs, _, err := e.resolveToolsForNode(context.Background(), node, names, toolAuthorship{author: node.Tools, runtime: runtimeAppends{"todo_write": true}})
	if err != nil {
		t.Fatalf("resolveToolsForNode: %v", err)
	}
	if !slices.ContainsFunc(defs, func(d delegate.ToolDef) bool { return d.Name == "skill" && d.AuthorDeclared }) {
		t.Fatalf("the author's entry did not come back marked: %v", qualifiedToolNames(defs))
	}

	off := runMeasured(t, defs, false)
	on := runMeasured(t, defs, true)
	t.Logf("real families: defer off = %d tools, %d bytes; defer on = %d tools, %d bytes; saved %d bytes (%d%%)",
		off.count, off.bytes, on.count, on.bytes, off.bytes-on.bytes, 100*(off.bytes-on.bytes)/max(off.bytes, 1))
	if on.count != 5 { // todo_write + the three withheld family tools + codemode
		t.Errorf("defer on declares %v, want the withheld set plus codemode (5)", on.names)
	}
	if slices.Contains(on.names, "skill") {
		t.Errorf("defer on still declares the author's skill tool: %v", on.names)
	}

	// The stress arm: one hundred synthetic author tools, each with a real
	// schema — under toolindex.MaxTools, so the catalogue carries every one
	// and only the wire shrinks.
	stress := []delegate.ToolDef{{
		Name: "todo_write", Description: "Maintain the task list.",
		Execute: func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
	}}
	schema := strings.Repeat("x", 1024)
	for i := range 100 {
		stress = append(stress, delegate.ToolDef{
			Name:           fmt.Sprintf("synth_%03d", i),
			Description:    "Synthetic author tool " + fmt.Sprint(i),
			InputSchema:    json.RawMessage(`{"type":"object","properties":{"payload":{"type":"string","description":"` + schema + `"}},"required":["payload"]}`),
			Execute:        func(context.Context, json.RawMessage) (string, error) { return "ok", nil },
			AuthorDeclared: true,
		})
	}
	off = runMeasured(t, stress, false)
	on = runMeasured(t, stress, true)
	t.Logf("stress (100 synthetic author tools): defer off = %d tools, %d bytes; defer on = %d tools, %d bytes; saved %d bytes (%d%%)",
		off.count, off.bytes, on.count, on.bytes, off.bytes-on.bytes, 100*(off.bytes-on.bytes)/max(off.bytes, 1))
	if on.count != 2 { // todo_write + codemode
		t.Errorf("defer on declares %v, want todo_write plus codemode (2)", on.names)
	}
	if off.count != len(stress)+1 { // the full set plus the codemode tool
		t.Errorf("defer off declares %d tools, want %d", off.count, len(stress)+1)
	}
}

// ---------------------------------------------------------------------------
// The author-marker seam: what resolveToolsForNode marks, and what it
// refuses to.
// ---------------------------------------------------------------------------

func TestTheAuthorMarkerFollowsTheAuthorsOwnEntries(t *testing.T) {
	t.Parallel()

	e := internalMCPExecutor(t)
	node := &ir.AgentNode{
		BaseNode:     ir.BaseNode{ID: "triage"},
		Tools:        []string{"skill", "mcp.iterion_board.*"},
		Capabilities: boardops.AllCapabilities(),
	}
	// The author's entries plus the runtime's own append. `todo_write` and
	// the board expansion resolve from this call too, so the marker's two
	// sides are visible in one result.
	defs, _, err := e.resolveToolsForNode(context.Background(), node,
		[]string{"skill", "mcp.iterion_board.*", "todo_write"}, toolAuthorship{author: node.Tools, runtime: runtimeAppends{"todo_write": true}})
	if err != nil {
		t.Fatalf("resolveToolsForNode: %v", err)
	}
	byName := make(map[string]delegate.ToolDef, len(defs))
	for _, d := range defs {
		byName[d.Name] = d
	}
	if d, ok := byName["skill"]; !ok || !d.AuthorDeclared {
		t.Errorf("skill: marked=%v present=%v — the author's own entry must come back marked", markedOrFalse(byName, "skill"), ok)
	}
	board := 0
	for name, d := range byName {
		if strings.HasPrefix(name, "mcp_iterion_board_") {
			board++
			if !d.AuthorDeclared {
				t.Errorf("%s: the expansion of the author's OWN wildcard must come back marked", name)
			}
		}
	}
	if board == 0 {
		t.Fatal("the fixture resolved no board tool — the wildcard arm proves nothing")
	}
	if d, ok := byName["todo_write"]; !ok {
		t.Fatal("the fixture resolved no todo_write — the append arm proves nothing")
	} else if d.AuthorDeclared {
		t.Error("todo_write is marked author-declared — the runtime's append must stay declared")
	}
}

func markedOrFalse(byName map[string]delegate.ToolDef, name string) bool {
	return byName[name].AuthorDeclared
}

// ---------------------------------------------------------------------------
// The sandbox crossing does not carry the deferral (ToIOTask), so a
// deferring node that crosses one runs undeclared-to-declared. The build
// says so once, on the codemode family's logger — never a silent
// replacement of the author's `defer: on`.
// ---------------------------------------------------------------------------

func TestASandboxedDeferringNodeIsToldItRunsUndeferred(t *testing.T) {
	t.Parallel()
	const marker = "codemode: `defer: on` is not applied on this route"
	type fixture struct {
		e   *ClawExecutor
		buf *bytes.Buffer
	}
	newFixture := func(t *testing.T, sandboxed bool) fixture {
		t.Helper()
		var buf bytes.Buffer
		e := &ClawExecutor{
			logger:       iterlog.New(iterlog.LevelInfo, &buf),
			toolRegistry: placementRegistry(t, "read_file", "todo_write"),
		}
		if sandboxed {
			e.sandbox = fakeSandboxRun{}
		}
		return fixture{e: e, buf: &buf}
	}
	fields := func(deferMode string, tools ...string) backendFields {
		return backendFields{
			id: "surveyor", model: "anthropic/claude-opus-5", tools: tools,
			codemode: "on", codemodeDefer: deferMode,
		}
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "surveyor"}}
	build := func(t *testing.T, fx fixture, f backendFields, sess *nodeBuildSession) error {
		t.Helper()
		task, err := fx.e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, sess)
		if err == nil {
			if got, want := task.CodemodeDefer, f.codemodeDefer == "on"; got != want {
				t.Fatalf("fixture: task.CodemodeDefer = %v, want %v", got, want)
			}
		}
		return err
	}

	t.Run("a sandboxed deferring node is told", func(t *testing.T) {
		fx := newFixture(t, true)
		if err := build(t, fx, fields("on", "read_file"), nil); err != nil {
			t.Fatalf("buildTask: %v", err)
		}
		log := fx.buf.String()
		if !strings.Contains(log, marker) || !strings.Contains(log, "surveyor") ||
			!strings.Contains(log, "sandboxed runner declares every tool") || !strings.Contains(log, "only to unsandboxed execution") {
			t.Errorf("a sandboxed deferring node was not told it runs undeferred; log:\n%s", log)
		}
	})
	t.Run("an unsandboxed deferring node is not", func(t *testing.T) {
		fx := newFixture(t, false)
		if err := build(t, fx, fields("on", "read_file"), nil); err != nil {
			t.Fatalf("buildTask: %v", err)
		}
		if strings.Contains(fx.buf.String(), marker) {
			t.Errorf("an unsandboxed deferring node was warned — the deferral applies there:\n%s", fx.buf.String())
		}
	})
	t.Run("a sandboxed node that does not defer is not", func(t *testing.T) {
		fx := newFixture(t, true)
		if err := build(t, fx, fields("", "read_file"), nil); err != nil {
			t.Fatalf("buildTask: %v", err)
		}
		if strings.Contains(fx.buf.String(), marker) {
			t.Errorf("a sandboxed node that does not defer was warned:\n%s", fx.buf.String())
		}
	})
	// A build that fails runs nothing, so it must not describe how a route
	// runs: tool resolution is the last thing that can fail the build, and
	// the warning is held until after it.
	t.Run("a failing build says nothing", func(t *testing.T) {
		fx := newFixture(t, true)
		if err := build(t, fx, fields("on", "no_such_tool"), nil); err == nil {
			t.Fatal("fixture: the build with an unregistered tool succeeded — the failing arm proves nothing")
		}
		if strings.Contains(fx.buf.String(), marker) {
			t.Errorf("a FAILED build logged how its route runs — a route that never ran:\n%s", fx.buf.String())
		}
	})
	// The real session: one node execution, three chain elements. The
	// failed first element must not spend the claim; the two that build
	// are told once between them.
	t.Run("once per node execution, a failed element spending nothing", func(t *testing.T) {
		fx := newFixture(t, true)
		sess := &nodeBuildSession{}
		if err := build(t, fx, fields("on", "no_such_tool"), sess); err == nil {
			t.Fatal("fixture: the failing element built")
		}
		for i := range 2 {
			if err := build(t, fx, fields("on", "read_file"), sess); err != nil {
				t.Fatalf("element %d: buildTask: %v", i+2, err)
			}
		}
		if n := strings.Count(fx.buf.String(), marker); n != 1 {
			t.Errorf("the warning was said %d time(s) over one node execution, want exactly 1 (a failed element spends nothing, a rebuilt one repeats nothing):\n%s", n, fx.buf.String())
		}
	})
}

// ---------------------------------------------------------------------------
// An author who lists a tool the runtime appends anyway does not get to
// defer it: the single entry serves the runtime's posture (todo_write's
// reseed, the auto-memory instructions). Driven end to end — buildTask
// records the appends, resolveToolsForNode reads the record, the backend
// builds the wire.
// ---------------------------------------------------------------------------

func TestAnAuthorListedRuntimeToolStaysDeclaredUnderDefer(t *testing.T) {
	t.Parallel()
	e := &ClawExecutor{
		logger:       iterlog.Nop(),
		toolRegistry: placementRegistry(t, "todo_write", "skill"),
	}
	node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "planner"}}
	f := backendFields{
		id: "planner", model: "test/test-model", tools: []string{"todo_write", "skill"},
		codemode: "on", codemodeDefer: "on",
	}
	task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if !task.CodemodeDefer {
		t.Fatal("fixture: the task does not defer — this witness proves nothing")
	}
	// The build resolved no prompt (the fixture declares none); the wire is
	// what is under test, so the turn gets one.
	task.UserPrompt = "go"
	backend, cap := newCapturingBackend()
	if _, err := backend.Execute(context.Background(), task); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := requestedToolNames(cap)
	if !slices.Contains(got, "todo_write") {
		t.Errorf("the author-listed todo_write left the wire (got %v) — the runtime appends it anyway, and the posture keeps telling the model to call it", got)
	}
	if slices.Contains(got, "skill") {
		t.Errorf("the author's own skill stayed declared (got %v) — the fixture defers nothing, so this witness would pass over a dead deferral", got)
	}
}

// The PROPERTY behind the census below: every name the union adds beyond
// the author's `tools:` list is in the runtimeAppends record. The census
// reads the source for the spellings it knows (ensureToolPresent, append, …)
// and is blind to any other way of growing a slice; this reads the
// behaviour, over the product of every axis an append is conditional on —
// backend, the author's list (empty, declared-empty, restricted, one that
// already names a runtime tool), interaction mode, capability grant,
// ultracode, auto-memory, and the image attachment the CLI backends append
// read_image for.
func TestEveryNameTheUnionAddsIsInTheRecord(t *testing.T) {
	t.Parallel()
	backends := []string{delegate.BackendClaw, delegate.BackendClaudeCode, delegate.BackendCodex, "pi"}
	toolLists := [][]string{nil, {}, {"bash"}, {"read_file", "grep"}, {"todo_write", "skill"}, {"ask_user", "agent"}}
	interactions := []ir.InteractionMode{ir.InteractionNone, ir.InteractionHuman, ir.InteractionAsync}
	capSets := [][]string{
		nil,
		boardops.AllCapabilities(),
		{runops.CapRunsRead},
		{connection.CapConnectors},
		slices.Concat(boardops.AllCapabilities(), []string{runops.CapRunsRead, connection.CapConnectors}),
	}
	prompts := map[string]*ir.Prompt{"with_image": {Name: "with_image", Body: "look at {{attachments.shot}}"}}
	checked := 0
	added := map[string]bool{}
	for _, backend := range backends {
		for _, tools := range toolLists {
			for _, interaction := range interactions {
				for _, caps := range capSets {
					for _, ultracode := range []bool{false, true} {
						for _, autoMemory := range []string{"", "on"} {
							for _, userPrompt := range []string{"", "with_image"} {
								e := &ClawExecutor{
									botID:        "tester",
									wfAutoMemory: autoMemory,
									prompts:      prompts,
									imageAttachs: map[string]bool{"shot": true},
								}
								f := backendFields{id: "n", tools: tools, interaction: interaction, userPrompt: userPrompt}
								union, appended := e.assembleEffectiveToolsAndAppends(f, backend, caps, ultracode)
								for _, name := range union {
									if slices.Contains(tools, name) {
										continue
									}
									added[name] = true
									if !appended[name] {
										t.Errorf("backend=%s tools=%v interaction=%v caps=%d ultracode=%v auto_memory=%q prompt=%q: the union adds %q, which the record does not hold — a runtime tool the deferral is free to demote",
											backend, tools, interaction, len(caps), ultracode, autoMemory, userPrompt, name)
									}
								}
								checked++
							}
						}
					}
				}
			}
		}
	}
	if want := len(backends) * len(toolLists) * len(interactions) * len(capSets) * 2 * 2 * 2; checked != want {
		t.Fatalf("checked %d fixtures, want %d", checked, want)
	}
	// The product reaches every append kind — or the property would hold
	// over fixtures that never append.
	for _, kind := range []string{askUserToolName, delegate.AskUserAsyncToolName, delegate.AwaitAnswersToolName,
		"todo_write", clawOrchestrationTool, "write_file", "read_image"} {
		if !added[kind] {
			t.Errorf("no fixture of the product adds %q — that append is unexercised", kind)
		}
	}
	family := 0
	for name := range added {
		if strings.Contains(name, "iterion_") {
			family++
		}
	}
	if family == 0 {
		t.Error("no fixture adds an iterion family tool (board, runs, connectors) — the capability appends are unexercised")
	}
}

// Every runtime append in assembleEffectiveToolsAndAppends goes through the
// recorder: an append that bypassed it would be a runtime tool the deferral
// is free to demote — the defect class the record closes. Read from the
// source, so a new append cannot appear without this test seeing it.
func TestEveryRuntimeAppendIsRecorded(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "executor_build_task.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "assembleEffectiveToolsAndAppends" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("assembleEffectiveToolsAndAppends not found — the census is blind")
	}
	recorded := 0
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			switch fun.Name {
			case "ensureToolPresent", "append":
				t.Errorf("%s: %s(...) appends to the node's tools without the record — route it through runtimeAppends.ensure/add", fset.Position(call.Pos()), fun.Name)
			}
		case *ast.SelectorExpr:
			if id, ok := fun.X.(*ast.Ident); ok && id.Name == "appended" && (fun.Sel.Name == "ensure" || fun.Sel.Name == "add") {
				recorded++
			}
		}
		return true
	})
	if recorded == 0 {
		t.Fatal("no recorded append found — the census matches nothing and proves nothing")
	}
}

// The fact the complement's wording respects: the dispatcher's map is the
// union, so a direct tool_use naming a catalogue-only tool still executes.
// The complement says such tools are not declared and are reached from a
// program — never that a direct call is impossible, which this would make
// a lie.
func TestADirectCallToACatalogueOnlyToolStillExecutes(t *testing.T) {
	t.Parallel()
	var ran bool
	tools := []GenerationTool{{
		Name: "grep", InputSchema: json.RawMessage(`{"type":"object"}`),
		Execute: func(context.Context, json.RawMessage) (string, error) {
			ran = true
			return "found", nil
		},
		authorDeclared: true, catalogueOnly: true,
	}}
	toolMap := buildToolMap(tools)
	blocks, abort := executeToolsDirect(context.Background(), []toolUseBlock{
		{ID: "tu_1", Name: "grep", PartialJSON: `{}`},
	}, toolMap, nil, nil, nil, nil, nil)
	if abort != nil {
		t.Fatalf("abort: %v", abort)
	}
	if !ran || len(blocks) != 1 || blocks[0].IsError {
		t.Fatalf("the direct call did not execute (ran=%v, blocks=%+v)", ran, blocks)
	}
	for _, claim := range []string{"no direct call", "cannot be called", "never"} {
		if strings.Contains(codemodeDeferredComplement, claim) {
			t.Errorf("the complement claims %q, which the dispatcher contradicts: %q", claim, codemodeDeferredComplement)
		}
	}
}

// ---------------------------------------------------------------------------
// An Ask-able tool is never deferred (Rcf8ffb). Inside a program an Ask
// degrades to a refusal, so a demoted tool the node's policy can ask about
// would have no usable call at all. The witnesses drive the REAL backend
// with the policy on the task — the object the dispatcher gates with — and
// then drive a direct tool_use through the funnel to prove the declared
// call reaches the Ask, i.e. it is usable, not merely listed.
// ---------------------------------------------------------------------------

type wireOutcome struct {
	names       []string
	description string
}

func runPolicyNode(t *testing.T, defs []delegate.ToolDef, pol *permission.Policy) wireOutcome {
	t.Helper()
	backend, cap := newCapturingBackend()
	if _, err := backend.Execute(context.Background(), delegate.Task{
		NodeID:        "n",
		Model:         "test/test-model",
		UserPrompt:    "go",
		HasTools:      true,
		ToolDefs:      defs,
		Codemode:      true,
		CodemodeDefer: true,
		Permission:    pol,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(cap.requests) == 0 {
		t.Fatal("no request reached the wire")
	}
	var out wireOutcome
	for _, tl := range cap.requests[0].Tools {
		out.names = append(out.names, tl.Name)
		if tl.Name == ScriptHostToolName {
			out.description = tl.Description
		}
	}
	return out
}

func authorTool(name string) delegate.ToolDef {
	return delegate.ToolDef{
		Name: name, Description: "Author tool " + name + ".",
		InputSchema:    json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
		Execute:        func(context.Context, json.RawMessage) (string, error) { return "ran", nil },
		AuthorDeclared: true,
	}
}

// directCallAsks drives one tool_use through the direct funnel under pol and
// reports whether it raised the Ask pause.
func directCallAsks(t *testing.T, name, input string, pol *permission.Policy) bool {
	t.Helper()
	tools := toolDefsToGeneration([]delegate.ToolDef{authorTool(name)})
	_, abort := executeToolsDirect(context.Background(), []toolUseBlock{{ID: "tu_1", Name: name, PartialJSON: input}},
		buildToolMap(tools), nil, nil, nil, nil, pol)
	var ask *delegate.ErrAskUser
	return errors.As(abort, &ask)
}

func TestAnAskableToolIsNeverDeferred(t *testing.T) {
	t.Parallel()
	mustPolicy := func(t *testing.T, mode permission.Mode, allow, ask []string) *permission.Policy {
		t.Helper()
		pol, err := permission.NewPolicy(mode, allow, ask, nil)
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		return pol
	}

	t.Run("an ask rule on a bare builtin keeps it declared, the rest deferred", func(t *testing.T) {
		t.Parallel()
		pol := mustPolicy(t, permission.ModeDeny, []string{"Read(**)", "Grep"}, []string{"Bash"})
		got := runPolicyNode(t, []delegate.ToolDef{authorTool("read_file"), authorTool("grep"), authorTool("bash")}, pol)
		if !slices.Contains(got.names, "bash") {
			t.Errorf("bash, which the policy asks about, left the wire (got %v) — inside a program its Ask is a refusal, so it has no usable call", got.names)
		}
		for _, name := range []string{"read_file", "grep"} {
			if slices.Contains(got.names, name) {
				t.Errorf("%s stayed declared (got %v) — the policy never asks about it, so the deferral applies", name, got.names)
			}
		}
		if !directCallAsks(t, "bash", `{"command":"ls"}`, pol) {
			t.Error("the declared bash does not reach the Ask on a direct call — the fixture's policy is not the one the gate applies")
		}
	})

	t.Run("an argument-scoped ask keeps the tool declared", func(t *testing.T) {
		t.Parallel()
		pol := mustPolicy(t, permission.ModeDeny, []string{"Bash", "Read(**)"}, []string{"Bash(git push:*)"})
		got := runPolicyNode(t, []delegate.ToolDef{authorTool("read_file"), authorTool("bash")}, pol)
		if !slices.Contains(got.names, "bash") {
			t.Errorf("bash left the wire although `Bash(git push:*)` can ask about it (got %v)", got.names)
		}
		if slices.Contains(got.names, "read_file") {
			t.Errorf("read_file stayed declared (got %v)", got.names)
		}
		if !directCallAsks(t, "bash", `{"command":"git push origin main"}`, pol) {
			t.Error("a `git push` through the declared bash does not reach the Ask")
		}
	})

	t.Run("an MCP tool the policy asks about keeps its declared call", func(t *testing.T) {
		t.Parallel()
		// The name the dispatcher evaluates a direct call with is the
		// tool_use name — GenerationTool.Name, claw's sanitized spelling —
		// and CanAskAbout is asked with exactly that name.
		pol := mustPolicy(t, permission.ModeDeny, []string{"Read(**)"}, []string{"mcp_repo_*"})
		got := runPolicyNode(t, []delegate.ToolDef{authorTool("read_file"), authorTool("mcp_repo_search")}, pol)
		if !slices.Contains(got.names, "mcp_repo_search") {
			t.Errorf("the Ask-able MCP tool left the wire (got %v)", got.names)
		}
		if slices.Contains(got.names, "read_file") {
			t.Errorf("read_file stayed declared (got %v)", got.names)
		}
		if !directCallAsks(t, "mcp_repo_search", `{}`, pol) {
			t.Error("the declared MCP tool does not reach the Ask on a direct call — the veto and the gate read different names")
		}
	})

	t.Run("permission mode ask demotes nothing, and so carries no complement", func(t *testing.T) {
		t.Parallel()
		pol := mustPolicy(t, permission.ModeAsk, nil, nil)
		got := runPolicyNode(t, []delegate.ToolDef{authorTool("read_file"), authorTool("grep"), authorTool("bash")}, pol)
		for _, name := range []string{"read_file", "grep", "bash", ScriptHostToolName} {
			if !slices.Contains(got.names, name) {
				t.Errorf("%q left the wire under permission mode ask (got %v) — every call can ask, so nothing is deferred", name, got.names)
			}
		}
		if strings.Contains(got.description, codemodeDeferredComplement) {
			t.Error("a node that demoted nothing carries the catalogue-only complement")
		}
	})
}

// ---------------------------------------------------------------------------
// Deferral never removes reach (R450fc1): only tools the catalogue bound
// keeps WHOLE are demoted. A tool past MaxTools, an over-long name, or an
// oversized schema stays declared — the bound would otherwise take it off
// the wire AND out of the index, or leave a program to call it blind.
// ---------------------------------------------------------------------------

func TestDeferralDemotesOnlyWhatTheBoundKeepsWhole(t *testing.T) {
	t.Parallel()

	t.Run("past MaxTools the rest stay declared and callable", func(t *testing.T) {
		t.Parallel()
		const n = 300
		defs := make([]delegate.ToolDef, 0, n)
		for i := range n {
			defs = append(defs, authorTool(fmt.Sprintf("synth_%03d", i)))
		}
		got := runPolicyNode(t, defs, nil)
		// Bound keeps the first MaxTools names in sorted order: synth_000 …
		// synth_255. Exactly those leave the wire.
		for i := range n {
			name := fmt.Sprintf("synth_%03d", i)
			kept := i < toolindex.MaxTools
			if declared := slices.Contains(got.names, name); declared == kept {
				t.Errorf("%s: declared=%v, want %v — only a tool the bound keeps whole may be demoted", name, declared, !kept)
			}
		}
		if want := n - toolindex.MaxTools + 1; len(got.names) != want {
			t.Errorf("the request declares %d tools, want %d (the %d the bound leaves out, plus codemode)", len(got.names), want, n-toolindex.MaxTools)
		}
		// Still callable, both sides: a declared one directly, and the map
		// still holds the demoted ones for the door.
		tools := wireTools(defs)
		toolMap := buildToolMap(tools)
		if _, ok := toolMap["synth_000"]; !ok {
			t.Error("a demoted tool left the tool map — the door can no longer serve it")
		}
		blocks, abort := executeToolsDirect(context.Background(), []toolUseBlock{{ID: "tu_1", Name: "synth_299", PartialJSON: `{}`}},
			toolMap, nil, nil, nil, nil, nil)
		if abort != nil || len(blocks) != 1 || blocks[0].IsError {
			t.Errorf("the declared synth_299 is not directly callable (abort=%v blocks=%+v)", abort, blocks)
		}
	})

	t.Run("an oversized schema stays declared", func(t *testing.T) {
		t.Parallel()
		big := authorTool("big_schema")
		big.InputSchema = json.RawMessage(`{"type":"object","properties":{"payload":{"type":"string","description":"` + strings.Repeat("x", 40<<10) + `"}}}`)
		got := runPolicyNode(t, []delegate.ToolDef{authorTool("read_file"), big}, nil)
		if !slices.Contains(got.names, "big_schema") {
			t.Errorf("the 40 KiB-schema tool left the wire (got %v) — the bound omits its schema, so a program would call it blind", got.names)
		}
		if slices.Contains(got.names, "read_file") {
			t.Errorf("read_file stayed declared (got %v) — the fixture defers nothing", got.names)
		}
	})

	t.Run("an over-long name stays declared", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("n", toolindex.MaxNameBytes+44)
		got := runPolicyNode(t, []delegate.ToolDef{authorTool("read_file"), authorTool(long)}, nil)
		if !slices.Contains(got.names, long) {
			t.Error("the over-long tool name left the wire — the bound leaves it out of the catalogue, so it would be reachable nowhere")
		}
		if slices.Contains(got.names, "read_file") {
			t.Errorf("read_file stayed declared (got %v)", got.names)
		}
	})
}
