package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Roles of a reasoning-effort writer. A gateway route accepts only the
// efforts its accepted list names (compatgw.AcceptedEfforts: the run's feed
// table, then the runner's env table), so every effort that reaches a
// request either crosses gatewayReasoningEffort or was written by a site
// that did.
const (
	// effortMaps: the function maps the effort itself — its body calls
	// routeReasoningEffort or gatewayReasoningEffort, which the walk checks.
	effortMaps = "MAPS"
	// effortTask: the function writes delegate.Task.ReasoningEffort, which
	// executeDelegate maps before any backend reads it
	// (TestEveryBackendExecuteCrossesTheModelBoundary pins the crossing).
	effortTask = "TASK"
	// effortCarries: the function moves a value already mapped onto the
	// next carrier — the task's effort onto ProviderOptions, ProviderOptions
	// onto the request — and must add no effort of its own.
	effortCarries = "CARRIES"
	// effortHelper: providerOptsForNode itself, the map literal every
	// ProviderOptions effort is built from; its callers are entries here.
	effortHelper = "HELPER"
	// effortNotWire: an event or hook field that reports an effort and
	// reaches no request.
	effortNotWire = "NOT WIRE"
)

// reasoningEffortWriters is the INVENTORY of every function of this package
// that writes a reasoning effort onto a carrier a request is built from —
// GenerationOptions.ProviderOptions (its "reasoning_effort" key, or the
// field whole), a ReasoningEffort field (delegate.Task, the provider
// request), or the providerOptsForNode helper — keyed by file:function,
// each with its role. A new writer states its role, and a MAPS entry is
// checked: its body must call the gateway mapping. The walk does not
// resolve types, so a ReasoningEffort field of a type that never reaches a
// request (an event payload) is an entry too, NOT WIRE.
var reasoningEffortWriters = map[string]string{
	"model_boundary.go:executeDelegate":          effortMaps + ": the task path's one mapping — every backend.Execute crosses it, the task's effort mapped onto the route's accepted list",
	"executor_human.go:executeHumanLLMWithSpend": effortMaps + ": the human stand-in's direct generation maps the node's effort before building ProviderOptions",
	"structured_recovery.go:effortAfterCut": effortMaps + ": the turn after a cut at max_tokens lowers the effort to the model's lowest accepted level, " +
		"mapped again, on a copy of ProviderOptions",
	"executor_build_task.go:buildTask":           effortTask + ": an agent node's task, effort from the node, the launch override or an edge mapping",
	"executor_router.go:executeLLMRouterUnified": effortTask + ": an llm router's task",
	"claw_backend.go:Execute": effortCarries + ": the claw backend turns the task's effort, mapped at executeDelegate, into ProviderOptions " +
		"(coerceEffortForModel leaves a gateway id's effort as is: a gateway route has no vendor capability id)",
	"generation_request.go:buildRequest":     effortCarries + ": ProviderOptions' effort onto the provider request — the wire",
	"executor_router.go:providerOptsForNode": effortHelper,
	"generation_request.go:fireOnRequest":    effortNotWire + ": the OnRequest hook's RequestInfo",
	"events.go:toLLMRequestInfo":             effortNotWire + ": RequestInfo onto the llm_request event's info",
	"hooks.go:onLLMRequest":                  effortNotWire + ": the llm_request event's data map",
}

// effortMappers are the functions a MAPS entry's body calls.
var effortMappers = map[string]bool{"routeReasoningEffort": true, "gatewayReasoningEffort": true}

// effortWriterWalkFloor is what a collapsed walk would hide: 88 production
// files in this package when the inventory was written. The floor catches a
// walk that read almost nothing, never the file count.
const effortWriterWalkFloor = 40

// TestEveryReasoningEffortWriterIsInventoried pins the writers of a
// reasoning effort: a site outside the inventory is named with the roles it
// may claim, an entry whose writer is gone is named stale, and a MAPS entry
// whose body no longer calls the gateway mapping is refused.
func TestEveryReasoningEffortWriterIsInventoried(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's own directory: %v", err)
	}
	visited := 0
	seen := map[string]bool{}
	var offenders []string
	var files []*ast.File
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(name) // #nosec G304 -- this package's own files
		if rerr != nil {
			t.Errorf("%s could not be read, so its writers were NOT inventoried: %v", name, rerr)
			continue
		}
		visited++
		f, perr := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if perr != nil {
			t.Errorf("%s could not be parsed, so its writers were NOT inventoried: %v", name, perr)
			continue
		}
		files = append(files, f)
		names = append(names, name)
	}
	// A key spelled through a name — a constant or a variable bound to
	// "reasoning_effort" anywhere in the package — is the effort key too.
	keyNames := effortKeyNames(files)
	for i, f := range files {
		name := names[i]
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				if writesAnEffort(decl, keyNames) {
					offenders = append(offenders, name+":<package scope>")
				}
				continue
			}
			if fn.Body == nil || !writesAnEffort(fn.Body, keyNames) {
				continue
			}
			key := name + ":" + fn.Name.Name
			seen[key] = true
			role, listed := reasoningEffortWriters[key]
			if !listed {
				offenders = append(offenders, key)
				continue
			}
			if strings.HasPrefix(role, effortMaps) && !callsAny(fn.Body, effortMappers) {
				t.Errorf("%s is inventoried as %s, and its body calls neither routeReasoningEffort nor gatewayReasoningEffort: "+
					"the effort it writes reaches a gateway unmapped — map it, or state the role it really has", key, effortMaps)
			}
		}
	}
	sort.Strings(offenders)
	for _, key := range offenders {
		t.Errorf("%s writes a reasoning effort onto a request's carrier and is not in reasoningEffortWriters: add an entry with its role — "+
			"%s (it maps through routeReasoningEffort/gatewayReasoningEffort itself), %s (delegate.Task's effort, mapped at executeDelegate), "+
			"%s (it moves a mapped value and adds none), or %s (an event field) — a gateway answers 400 to an effort outside its accepted list",
			key, effortMaps, effortTask, effortCarries, effortNotWire)
	}
	var stale []string
	for key := range reasoningEffortWriters {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("reasoningEffortWriters names %s, which the walk did not see write an effort: check that the function still exists and still writes one. "+
			"If it does, the walk lost it — fix the walk. Only if the write is gone does the entry come out", key)
	}
	if visited < effortWriterWalkFloor {
		t.Errorf("the walk read %d production files, under the floor of %d: it collapsed, so its answer means nothing", visited, effortWriterWalkFloor)
	}
}

// effortKeyNames lists the identifiers the files bind to the
// "reasoning_effort" string — a constant or a variable, declared at
// package scope or in a function, or assigned. The walk resolves no
// types, so a name is matched by its spelling in the whole package: a
// writer indexing a map by one is a writer of the effort key.
func effortKeyNames(files []*ast.File) map[string]bool {
	names := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.ValueSpec:
				for i, name := range x.Names {
					if i < len(x.Values) && isEffortLiteral(x.Values[i]) {
						names[name.Name] = true
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && i < len(x.Rhs) && isEffortLiteral(x.Rhs[i]) {
						names[id.Name] = true
					}
				}
			}
			return true
		})
	}
	return names
}

// isEffortLiteral reports whether e is the "reasoning_effort" string.
func isEffortLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && s == "reasoning_effort"
}

// writesAnEffort reports whether n writes a reasoning effort: a call of
// providerOptsForNode, an assignment to a "reasoning_effort" key (the
// literal or a name keyNames binds to it) or a ProviderOptions /
// ReasoningEffort field, a composite literal setting one of them, the
// address of either field taken (a setter's or a decoder's pointer), or
// the ProviderOptions map handed to a call — maps.Copy, a decoder, a
// setter whose key is a parameter — which may write it out of this walk's
// sight; len and cap only read it.
func writesAnEffort(n ast.Node, keyNames map[string]bool) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		switch x := node.(type) {
		case *ast.CallExpr:
			found = calleeName(x) == "providerOptsForNode" || handsTheCarrier(x)
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if isEffortTarget(lhs, keyNames) {
					found = true
				}
			}
		case *ast.KeyValueExpr:
			found = isEffortKey(x.Key, keyNames)
		case *ast.UnaryExpr:
			found = x.Op == token.AND && isEffortField(x.X)
		}
		return !found
	})
	return found
}

// carrierReaders are the builtins handed the ProviderOptions map that
// cannot write it.
var carrierReaders = map[string]bool{"len": true, "cap": true}

// handsTheCarrier reports whether a call is handed the ProviderOptions map
// itself — a map is a reference, so the callee may write any key of it.
func handsTheCarrier(call *ast.CallExpr) bool {
	if fn, ok := call.Fun.(*ast.Ident); ok && carrierReaders[fn.Name] {
		return false
	}
	for _, arg := range call.Args {
		if sel, ok := arg.(*ast.SelectorExpr); ok && sel.Sel.Name == "ProviderOptions" {
			return true
		}
	}
	return false
}

// isEffortField reports whether e is a ProviderOptions or ReasoningEffort
// field.
func isEffortField(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && (sel.Sel.Name == "ProviderOptions" || sel.Sel.Name == "ReasoningEffort")
}

// isEffortTarget reports whether an assignment's left side is an effort
// carrier: m["reasoning_effort"] (or m[k], k a name keyNames binds to it),
// x.ProviderOptions, x.ReasoningEffort.
func isEffortTarget(lhs ast.Expr, keyNames map[string]bool) bool {
	switch x := lhs.(type) {
	case *ast.IndexExpr:
		return isEffortKey(x.Index, keyNames)
	case *ast.SelectorExpr:
		return x.Sel.Name == "ProviderOptions" || x.Sel.Name == "ReasoningEffort"
	}
	return false
}

// isEffortKey reports whether a composite-literal key or an index names an
// effort carrier: the "reasoning_effort" string, a name keyNames binds to
// it, or a ProviderOptions / ReasoningEffort field.
func isEffortKey(key ast.Expr, keyNames map[string]bool) bool {
	switch k := key.(type) {
	case *ast.BasicLit:
		return isEffortLiteral(k)
	case *ast.Ident:
		return k.Name == "ProviderOptions" || k.Name == "ReasoningEffort" || keyNames[k.Name]
	}
	return false
}

// callsAny reports whether n calls a function named in names.
func callsAny(n ast.Node, names map[string]bool) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && names[calleeName(call)] {
			found = true
		}
		return !found
	})
	return found
}

// The walk sees an effort however its key is spelled: the literal, a
// constant or a variable bound to "reasoning_effort" (in another file of
// the package, or local), indexed in place, through an alias of
// ProviderOptions, or as a composite-literal key. A key bound to another
// string is no effort.
func TestTheEffortWriterWalkSeesEveryKeySpelling(t *testing.T) {
	t.Parallel()
	const keys = `package model
const effortKeyProbe = "reasoning_effort"
const otherKeyProbe = "temperature"
var effortVarProbe = "reasoning_effort"
`
	for _, tc := range []struct {
		name, body string
		writes     bool
	}{
		{"literal key", `opts.ProviderOptions["reasoning_effort"] = v`, true},
		{"package constant, in place", `opts.ProviderOptions[effortKeyProbe] = v`, true},
		{"package constant, through an alias", `po := opts.ProviderOptions; po[effortKeyProbe] = v`, true},
		{"package variable", `po := map[string]any{}; po[effortVarProbe] = v; _ = po`, true},
		{"local constant", `const k = "reasoning_effort"; po := map[string]any{}; po[k] = v; _ = po`, true},
		{"local variable", `k := "reasoning_effort"; po := map[string]any{}; po[k] = v; _ = po`, true},
		{"composite-literal key", `_ = map[string]any{effortKeyProbe: v}`, true},
		{"another key", `po := map[string]any{}; po[otherKeyProbe] = v; _ = po`, false},
		{"a field's address handed to a setter", `setString(&task.ReasoningEffort, v)`, true},
		{"the carrier's address handed to a decoder", `_ = json.Unmarshal(raw, &opts.ProviderOptions)`, true},
		{"the carrier handed to maps.Copy", `maps.Copy(opts.ProviderOptions, src)`, true},
		{"the carrier handed to a setter, the key a parameter", `setKey(opts.ProviderOptions, k, v)`, true},
		{"the carrier's length", `_ = len(opts.ProviderOptions)`, false},
		{"the effort read as a value", `_ = fmt.Sprint(task.ReasoningEffort)`, false},
	} {
		src := "package model\nfunc probe(opts GenerationOptions, v string) { " + tc.body + " }\n"
		var files []*ast.File
		for i, text := range []string{keys, src} {
			f, err := parser.ParseFile(token.NewFileSet(), "probe"+strconv.Itoa(i)+".go", text, 0)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			files = append(files, f)
		}
		keyNames := effortKeyNames(files)
		fn := files[1].Decls[0].(*ast.FuncDecl)
		if got := writesAnEffort(fn.Body, keyNames); got != tc.writes {
			t.Errorf("%s: the walk saw writesAnEffort=%v, want %v", tc.name, got, tc.writes)
		}
	}
}
