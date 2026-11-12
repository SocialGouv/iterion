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
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				if writesAnEffort(decl) {
					offenders = append(offenders, name+":<package scope>")
				}
				continue
			}
			if fn.Body == nil || !writesAnEffort(fn.Body) {
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

// writesAnEffort reports whether n writes a reasoning effort: a call of
// providerOptsForNode, an assignment to a "reasoning_effort" key or a
// ProviderOptions / ReasoningEffort field, or a composite literal setting
// one of them.
func writesAnEffort(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		switch x := node.(type) {
		case *ast.CallExpr:
			found = calleeName(x) == "providerOptsForNode"
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if isEffortTarget(lhs) {
					found = true
				}
			}
		case *ast.KeyValueExpr:
			found = isEffortKey(x.Key)
		}
		return !found
	})
	return found
}

// isEffortTarget reports whether an assignment's left side is an effort
// carrier: m["reasoning_effort"], x.ProviderOptions, x.ReasoningEffort.
func isEffortTarget(lhs ast.Expr) bool {
	switch x := lhs.(type) {
	case *ast.IndexExpr:
		return isEffortKey(x.Index)
	case *ast.SelectorExpr:
		return x.Sel.Name == "ProviderOptions" || x.Sel.Name == "ReasoningEffort"
	}
	return false
}

// isEffortKey reports whether a composite-literal key or an index names an
// effort carrier: the "reasoning_effort" string, or a ProviderOptions /
// ReasoningEffort field.
func isEffortKey(key ast.Expr) bool {
	switch k := key.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(k.Value)
		return k.Kind == token.STRING && err == nil && s == "reasoning_effort"
	case *ast.Ident:
		return k.Name == "ProviderOptions" || k.Name == "ReasoningEffort"
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
