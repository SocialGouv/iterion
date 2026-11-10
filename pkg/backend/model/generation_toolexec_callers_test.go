package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// toolExecuteSites is the INVENTORY of every place in this package that calls
// an `Execute(` method, keyed by file:function, each with the gate it honours.
//
// It exists because toolDispatcher's doc comment makes a census claim — "two
// other sites run a tool's Execute closure today, each with a different gate"
// — and a census that lives in a comment drifts. This package already paid for
// that lesson once, in toolNodeCoreReaders' own words: "Before this inventory
// the rule lived in a doc comment alone, and the round that found that had
// already paid for it once." A reworded sentence is the artefact that lesson
// condemns, so the sentence points here and this holds the count.
//
// The class is chosen to be greppable rather than judged: `Execute` is a
// struct field of GenerationTool and tool.ToolDef, so reaching a tool's
// closure needs a selector. The walk does not resolve types, so it also
// catches an `Execute` on some other receiver — which is a false positive that
// fails LOUDLY and is answered by an entry saying so. The direction that
// matters is a call the walk does not see.
//
// Production files only. A test may touch any of these to exercise it; what
// the census is about is the paths that ship.
//
// Three ROLES, and the role is the point — a site that RUNS a closure is a
// dispatch path, a site that WRAPS one is a gate, and a site that merely
// CARRIES one is plumbing that must add nothing. A new site declares which.
//
// Writing this inventory is also what corrected its author: a
// `grep '\.Execute('` found 4 of these 9, because five reference Execute as a
// VALUE rather than calling it. That is the repo's own rule — inventory from
// the type or the verb, never by substring — paying immediately.
var toolExecuteSites = map[string]string{
	// RUNS a tool's Execute closure: every one of these is a dispatch path,
	// and every dispatch path is another gate to keep in agreement.
	"generation_toolexec.go:runToolExecution": "RUNS. THE door. Reached only through toolDispatcher.dispatch -> executeOneTool, so it carries the whole pipeline: " +
		"lifecycle hooks, the `allowed-tools:` command scope, permission.Policy, secret materialization, the events, and the node-set pin",
	"claw_backend.go:multiplexerHandler": "RUNS. The launcher's handler for a call a SANDBOXED node forwarded over IPC. It cannot reuse the door — the runner already ran " +
		"the in-process pipeline and what crosses is an envelope, not a tool_use block — so it re-derives the advertised-tool check, tool.SandboxPlacementOf, " +
		"permission.Policy on the tool's identity, and the same node-set pin",
	"executor_tool.go:executeToolNodeRecipe": "RUNS. A DSL tool node's `command:` running a registry tool. Honours the AUTHORING allowlist (checkToolNodePolicy over tool.ToolChecker), " +
		"not permission.Policy — a package executor_tool.go does not import. No node-set pin and no PreToolUse, because no model reads tool_search there: " +
		"it is not a gate on a model's call, and naming it here is what stops a reader assuming it is",

	// WRAPS a tool's Execute closure in a check: a gate, applied at the call.
	"executor_toolresolve.go:guardTool": "WRAPS. Replaces Execute with policy.CheckContext followed by the original, so a denied tool returns ErrToolDenied without the " +
		"implementation ever running. This is where a node's tool policy becomes an actual refusal",
	"executor_toolresolve.go:scopeMCPServerNamingTool": "WRAPS. Narrows the MCP-server-naming tools to the servers active for THIS node, decoding the input exactly as the " +
		"callee decodes it — into a map, read by the literal key — because a guard and the call it guards must read the same bytes the same way",

	// CARRIES a closure across a conversion or into a child's set: plumbing,
	// and it must add no judgement of its own.
	"claw_backend.go:toolDefsToGeneration": "CARRIES. Copies d.Execute across the delegate.ToolDef -> GenerationTool conversion. Pure plumbing: a check added here would be " +
		"a gate nobody is looking for",
	"claw_backend.go:applyAsyncAskExecs": "CARRIES (substitutes). Swaps the registry's loud \"not bound\" default execs of ask_user_async / await_answers for the task-bound " +
		"behaviour when the node is interaction: async (ADR-081). A no-op for every other node",
	"subagent.go:buildSubagentTools": "CARRIES. Copies td.Execute while building a child's tool set from the process registry. What bounds the child is the allowed set it is " +
		"filtered by, not anything added here",

	// NOT a tool's Execute at all.
	"model_boundary.go:executeDelegate": "NOT A TOOL: this is delegate.Backend.Execute, a whole model turn crossing to a backend. Listed because the walk matches the method " +
		"NAME and cannot resolve the receiver — an entry is how a false positive of that kind is answered, rather than by narrowing the walk until it stops seeing things",
}

// toolExecuteWalkFloor is what a collapsed walk would hide. There are 88
// production files in this package at the time of writing, so a walk that
// comes back having read a handful read nothing and its clean answer means
// nothing. The floor sits well under that: its job is to catch a walk that
// COLLAPSED — an unreadable directory, a filter that matched almost nothing —
// never to pin the file count, so a package that gains or loses a few files
// does not fail here. The inventory's own assertions are what judge a drift.
const toolExecuteWalkFloor = 40

// TestEverySiteRunningAnExecuteNamesItsGate pins the call set so a fourth site
// running a tool's Execute closure has to state which gate it honours instead
// of inheriting an assumption from a doc comment.
//
// Mutations seen red: adding a fifth function that calls `gt.Execute(...)`
// (the inventory names it and demands its gate); removing an entry while its
// call stands (the walk reports it as uninventoried); and renaming an
// inventoried function (the entry goes stale and the stale branch says which
// of the two causes to check).
func TestEverySiteRunningAnExecuteNamesItsGate(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's own directory: %v", err)
	}
	visited := 0
	seen := map[string]bool{}
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(name) // #nosec G304 -- this package's own files
		if rerr != nil {
			// One unreadable file costs one file of coverage, never the whole
			// inventory: aborting would hide every offender behind an
			// unrelated read failure.
			t.Errorf("%s could not be read, so it was NOT inventoried: %v", name, rerr)
			continue
		}
		visited++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			t.Errorf("%s could not be parsed, so it was NOT inventoried: %v", name, perr)
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				if callsAnExecute(decl) {
					offenders = append(offenders, name+":<package scope>")
				}
				continue
			}
			if !callsAnExecute(fn.Body) {
				continue
			}
			key := name + ":" + fn.Name.Name
			seen[key] = true
			if _, listed := toolExecuteSites[key]; !listed {
				offenders = append(offenders, key)
			}
		}
	}

	// The offender list is this test's product: reported FIRST, and everything
	// below is an Errorf — an abort here would delete the answer.
	sort.Strings(offenders)
	for _, key := range offenders {
		t.Errorf("%s touches an Execute method and is not in toolExecuteSites: add an entry naming its ROLE — RUNS a tool's closure (then it is a dispatch path, and every "+
			"dispatch path is another gate to keep in agreement with toolDispatcher's: name the gate it honours, or route through toolDispatcher.dispatch, which holds the whole "+
			"pipeline), WRAPS one in a check (then say what it refuses), CARRIES one across a conversion (then it must add no judgement), or is some other Execute entirely", key)
	}
	var stale []string
	for key := range toolExecuteSites {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		// The discriminator, not a remedy: each cause has a different one, and
		// the remedy for the wrong cause silences this test.
		t.Errorf("toolExecuteSites names %s, which the walk did not see call an Execute method: check whether that function still exists and still calls one. "+
			"If it does, the walk lost the file — fix the walk. Only if the call is gone does the entry come out", key)
	}
	if visited < toolExecuteWalkFloor {
		t.Errorf("the walk read %d production files, under the floor of %d: it collapsed, so its answer about the census means nothing", visited, toolExecuteWalkFloor)
	}
}

// toolFunnelCallers is the second half of the census, and the half that
// actually closes it: for each function in the tool-running funnel, the set of
// functions allowed to call it.
//
// The inventory above keys on a selector named `Execute`, which is the right
// class for finding who WRAPS or CARRIES a closure — but it cannot see a new
// dispatch path at all. Every real run in this package is funnelled through
// runToolExecution, which already owns the only Execute selector, so a second
// door calling executeOneTool directly contains no selector, and the inventory
// stays green while its own entry ("Reached only through
// toolDispatcher.dispatch") goes false. Measured: a probe file holding
//
//	func ScriptHostRunTool(...) { return executeOneTool(ctx, ..., nil, nil, nil, nil, nil) }
//
// runs a tool with no node-set pin and no command scope, and the inventory
// passes over a tree containing it.
//
// So this pins the funnel's CALLERS instead of a spelling. It is the greppable
// form of what the doc comment promises: anything dispatching on a node's
// behalf takes the door.
var toolFunnelCallers = map[string]map[string]bool{
	// The door. dispatch is the only way in, which is what makes the
	// dispatcher's pipeline unavoidable rather than merely available.
	"executeOneTool": {"dispatch": true},
	// The last step before a tool's own closure runs.
	"runToolExecution": {"executeOneTool": true},
}

// TestTheToolFunnelIsReachedOnlyThroughTheDoor refuses a second dispatch path.
//
// Mutations seen red: a new production function calling executeOneTool (named
// as an offender); and allowing it by widening toolFunnelCallers (the entry is
// then an explicit, reviewable decision rather than silence). In the same run
// the Execute inventory above stayed GREEN, which is the whole reason this
// second check exists.
//
// Mutating it takes a file on DISK: both walks read the filesystem rather than
// the build's file set, so `go test -overlay` adds a door to the build that the
// walk cannot see. The trade is deliberate — reading the real directory is what
// makes a build-tag-excluded file fail loudly instead of silently — but it means
// a probe for this guard is written, run and removed, never overlaid.
func TestTheToolFunnelIsReachedOnlyThroughTheDoor(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's own directory: %v", err)
	}
	visited := 0
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(name) // #nosec G304 -- this package's own files
		if rerr != nil {
			t.Errorf("%s could not be read, so it was NOT inventoried: %v", name, rerr)
			continue
		}
		visited++
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, name, src, 0)
		if perr != nil {
			t.Errorf("%s could not be parsed, so it was NOT inventoried: %v", name, perr)
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, isCall := node.(*ast.CallExpr)
				if !isCall {
					return true
				}
				ident, isIdent := call.Fun.(*ast.Ident)
				if !isIdent {
					return true
				}
				allowed, funnel := toolFunnelCallers[ident.Name]
				// A function calling itself is recursion, not a new door;
				// and the funnel's own declaration is not its caller.
				if !funnel || ident.Name == fn.Name.Name || allowed[fn.Name.Name] {
					return true
				}
				offenders = append(offenders, name+":"+fn.Name.Name+" -> "+ident.Name)
				return true
			})
		}
	}

	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("%s: a dispatch path reaches the tool-running funnel without going through toolDispatcher.dispatch. "+
			"Every dispatch path is another gate to keep in agreement with the door's — the lifecycle hooks, the `allowed-tools:` command scope, "+
			"permission.Policy, secret materialization, the events and the node-set pin. Route the call through dispatch, or widen toolFunnelCallers "+
			"and say in its entry which of those this path holds instead", o)
	}
	if visited < toolExecuteWalkFloor {
		t.Errorf("the walk read %d production files, under the floor of %d: it collapsed, so its answer about the funnel means nothing", visited, toolExecuteWalkFloor)
	}
}

// callsAnExecute reports whether the node contains a call to a method named
// Execute, called or taken as a value. Found through the syntax tree, never by
// the spelling of a line.
func callsAnExecute(n ast.Node) bool {
	if n == nil {
		return false
	}
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		sel, ok := node.(*ast.SelectorExpr)
		if ok && sel.Sel != nil && sel.Sel.Name == "Execute" {
			found = true
			return false
		}
		return true
	})
	return found
}
