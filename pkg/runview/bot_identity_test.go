package runview

import (
	"go/ast"
	"go/token"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A bot's memory is keyed on its identity, and four surfaces resolve that
// identity independently: a cloud launch stamps it on the run, a studio launch
// passes it to the executor without persisting it, `iterion run` passes
// nothing, and a resume rebuilds from the run document alone.
//
// They must agree. When they do not, the same bot silently keeps two memories
// — a bundle launched from the studio wrote to `whats-next` and, on resume,
// read an empty `whats_next` and left its notes there.
func TestBotIdentityAgreesAcrossSurfaces(t *testing.T) {
	const bundle = "bots/whats-next/main.bot"

	t.Run("a bundle resolves to its catalog id, not its workflow name", func(t *testing.T) {
		if got := BotIDForPath(bundle); got != "whats-next" {
			t.Errorf("BotIDForPath(%q) = %q, want %q", bundle, got, "whats-next")
		}
	})

	t.Run("launch and resume agree for a bundle the launch did not persist", func(t *testing.T) {
		launch := BotIDForPath(bundle) // what the CLI / detached subprocess uses
		resume := BotIDForRun(&store.Run{FilePath: bundle})
		if launch != resume {
			t.Fatalf("launch keyed %q, resume keyed %q — the resumed run reads an empty memory", launch, resume)
		}
	})

	t.Run("a persisted id always wins", func(t *testing.T) {
		// Cloud stamps it, and it is authoritative even when the path would
		// suggest something else (a catalog path rewritten inside a pod).
		got := BotIDForRun(&store.Run{BotID: "review-pr", FilePath: "bots/other/main.bot"})
		if got != "review-pr" {
			t.Errorf("persisted BotID should win, got %q", got)
		}
	})

	t.Run("a .botz keys on its declared name, never its content-hash cache slot", func(t *testing.T) {
		// bundle.Open extracts an archive into <cache>/<shard>/<sha256>/main.bot.
		// Deriving the identity from THAT path keys the bot's memory on a name
		// that changes with every edit to the bundle — each version bump would
		// orphan everything it had learned — and disagrees with the same bundle
		// opened in directory form.
		const slot = "/home/u/.cache/iterion/bundles/51/51c0e93a287cdbc10de3503523ddf988eae44168e41584fa2f2acb5f1282aad5/main.bot"
		if got := ResolveBotID("", "whats-next", slot); got != "whats-next" {
			t.Errorf("ResolveBotID with a manifest name = %q, want %q", got, "whats-next")
		}
		// Two builds of the same bot must agree.
		const slot2 = "/home/u/.cache/iterion/bundles/4e/4edf479ba9a6b55a76da6ad53bf873b7ed0f2d195641a7c71b5af8a2b3fde8bc/main.bot"
		if a, b := ResolveBotID("", "whats-next", slot), ResolveBotID("", "whats-next", slot2); a != b {
			t.Errorf("two builds of one bot keyed %q and %q", a, b)
		}
		// And must agree with the directory form of the same bundle.
		if a, b := ResolveBotID("", "whats-next", slot), ResolveBotID("", "whats-next", "bots/whats-next/main.bot"); a != b {
			t.Errorf("archive keyed %q, directory form keyed %q", a, b)
		}
	})

	t.Run("a launch by path and its resume agree without an explicit id", func(t *testing.T) {
		// The studio's file picker sends file_path with no bot_id. The launch
		// used to take the empty field raw (falling back to the workflow name)
		// while the resume derived one from the path — two spaces, one bot.
		const p = "/srv/projects/acme/main.bot"
		launch := ResolveBotID("", "", p)
		resume := BotIDForRun(&store.Run{FilePath: p})
		if launch != resume {
			t.Fatalf("launch keyed %q, resume keyed %q", launch, resume)
		}
	})

	t.Run("a standalone .bot claims no identity", func(t *testing.T) {
		// Deliberate: it has none beyond its workflow name, which the executor
		// already falls back to. Deriving one from the parent directory would
		// key the memory on wherever the file happens to sit — and would make
		// a CLI launch disagree with itself once the file moved.
		for _, p := range []string{"/tmp/scratch/probe.bot", "probe.bot", ""} {
			if got := BotIDForPath(p); got != "" {
				t.Errorf("BotIDForPath(%q) = %q, want \"\"", p, got)
			}
		}
	})
}

// Every place that builds an executor must decide, explicitly, which bot the
// run belongs to.
//
// This guards a CLASS, not a case. The identity was wired into the CLI launch,
// the CLI resume and the studio resume in one pass — and the studio's SUBBOT
// runner was missed, so the same subbot bundle kept one memory when its parent
// ran from the CLI and a different one when it ran from the studio. Nothing
// failed; the memory was just silently split. The first version of this guard
// then found an eighth site nobody had looked at, the dispatcher.
//
// It parses the SOURCE rather than scanning it. A text scan of `ExecutorSpec{`
// … `}` was the obvious version and it was wrong three ways at once: a `}`
// inside a string — `Bash(rm -rf }/)` is a legal permission rule — closed the
// literal early and failed legitimate code; the words "BotID:" inside a comment
// or a string satisfied it; and a spec built field by field carried no literal
// at all, so the one refactor most likely to reintroduce the defect was exactly
// the one that slipped through. A guard that can be evaded quietly is worse
// than none, because it reads as coverage.
//
// An entry in the allowlist is a decision on the record, not an exemption.
func TestEveryExecutorConstructionDecidesTheBotIdentity(t *testing.T) {
	exempt := map[string]string{
		// A golden replay executes ONE frozen node against a stub store and
		// never opens a memory space; a bot id there would be decoration.
		"pkg/botreplay/record.go": "replay harness: single node, no memory space",
	}

	var gaveUp []string
	offenders := executorSpecSitesMissing(t, "BotID", exempt, &gaveUp)
	assertGiveUpsAreOnTheRecord(t, gaveUp)
	if len(offenders) > 0 {
		t.Errorf("these build an executor without deciding its bot identity, so its bot-scoped memory "+
			"falls back to the WORKFLOW name and silently diverges from every surface that sets it: %v\n"+
			"Set BotID (see ResolveBotID), or add the file to this test's exempt map with a reason.",
			offenders)
	}
}

// executorSpecMissesField reports whether a file builds an ExecutorSpec
// without setting `field`, by either construction shape: a composite literal,
// or a variable whose fields are assigned one at a time.
//
// The walk is the field-independent part, and several fields of this spec are
// load-bearing in the same way — decided at every construction site or
// silently wrong at the one that forgot. Each such field gets its own test
// over this one traversal.
func executorSpecMissesField(file *ast.File, field string) (missing, unread bool) {
	missing = false
	// The identifier THIS file uses for the runview package. Hardcoding
	// "runview" made the sweep blind to `rv "…/pkg/runview"` — an import
	// alias is nobody's error and a guard that stops seeing a site because
	// of one reads as coverage while covering nothing. Measured once
	// already, on the manager-construction sweep of this same change.
	pkgNames := runviewPackageIdents(file)
	// Whether the BARE spelling `ExecutorSpec{…}` means ours: only inside
	// package runview itself, or in a file that dot-imports it. Matching it
	// unconditionally accused a file declaring its own package-local type of
	// the same name, and the remedy the failure names does not even
	// type-check there — the only way out would be an exempt entry about a
	// file that has nothing to do with the rule.
	bare := file.Name.Name == "runview" || pkgNames["."]

	// Shape 1 — `ExecutorSpec{…}` / `runview.ExecutorSpec{…}`, including the
	// ELIDED form Go allows inside a container literal: in
	// `map[string]ExecutorSpec{"a": {…}}` the inner literal carries no type of
	// its own, so matching on lit.Type alone misses every batch/broadcast
	// factory — the shape such code reaches for first.
	elided := map[ast.Node]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		var elem ast.Expr
		switch t := lit.Type.(type) {
		case *ast.MapType:
			elem = t.Value
		case *ast.ArrayType:
			elem = t.Elt
		}
		// One container level deeper, so `[][]ExecutorSpec{{{…}}}` is judged
		// too — and the DEPTH travels, because the recursion must stop at
		// the level that holds the specs. Descending blindly marked a
		// literal sitting in one of the spec's OWN fields as an elided
		// spec: ExecutorSpec has value-struct-slice fields, so
		// `[]ExecutorSpec{{BotID: "b", RunFallback: []ir.Fallback{{…}}}}`
		// was judged for BotID on the `ir.Fallback` literal and accused a
		// file that sets the field two lines up.
		depth := 0
		switch inner := elem.(type) {
		case *ast.ArrayType:
			elem, depth = inner.Elt, 1
		case *ast.MapType:
			elem, depth = inner.Value, 1
		}
		if elem != nil && isExecutorSpecType(elem, pkgNames, bare) {
			markElidedElements(lit, depth, elided)
		}
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || (!isExecutorSpecType(lit.Type, pkgNames, bare) && !elided[lit]) {
			return true
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == field {
					return true
				}
			}
		}
		missing = true
		return true
	})
	if missing {
		return true, unread
	}

	// Shape 2 — `var spec ExecutorSpec` (or `new(ExecutorSpec)`) followed by
	// `spec.Field = …`. The literal search above sees nothing here, and this is
	// the shape any ordinary refactor of an existing site produces.
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		declared := map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			switch v := m.(type) {
			case *ast.ValueSpec:
				if isExecutorSpecType(v.Type, pkgNames, bare) {
					for _, name := range v.Names {
						declared[name.Name] = true
					}
				}
			case *ast.AssignStmt:
				for i, rhs := range v.Rhs {
					call, ok := rhs.(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						continue
					}
					if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "new" {
						continue
					}
					if isExecutorSpecType(call.Args[0], pkgNames, bare) && i < len(v.Lhs) {
						if id, ok := v.Lhs[i].(*ast.Ident); ok {
							declared[id.Name] = true
						}
					}
				}
			}
			return true
		})
		if len(declared) == 0 {
			return true
		}
		// Narrowed on purpose: only a spec this function itself hands to
		// BuildExecutor is judged here. A spec filled by a helper through a
		// pointer — `var s ExecutorSpec; fill(&s)` — sets the field somewhere
		// this walk cannot see, and flagging it would fail correct code. In a
		// gate, a false accusation costs as much as a miss and is harder to
		// spot, so the guard gives that case up rather than guess.
		handed := map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok || !isBuildExecutorCall(call.Fun) {
				return true
			}
			for _, arg := range call.Args {
				switch a := arg.(type) {
				case *ast.Ident:
					handed[a.Name] = true
				case *ast.StarExpr:
					// `BuildExecutor(*spec)` after `spec := new(ExecutorSpec)`
					// — the shape a refactor to a pointer produces.
					if id, ok := a.X.(*ast.Ident); ok {
						handed[id.Name] = true
					}
				}
			}
			return true
		})
		// A factory that fills a spec and RETURNS it is handing it to
		// whoever calls BuildExecutor — the shape pkg/runner/loop.go's
		// executorSpec has today, covered only because it happens to use a
		// composite literal.
		//
		// Returned-and-never-assigned is a different animal: `var zero
		// ExecutorSpec` returned beside an error is a sentinel, and the same
		// file has one. Requiring at least one assigned field separates the
		// two without guessing — a factory that fills nothing builds
		// nothing.
		returned := map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			ret, ok := m.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			for _, res := range ret.Results {
				if id, ok := res.(*ast.Ident); ok {
					returned[id.Name] = true
				}
			}
			return true
		})
		assigned := map[string]bool{}
		anyField := map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			as, ok := m.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				anyField[id.Name] = true
				if sel.Sel.Name == field {
					assigned[id.Name] = true
				}
			}
			return true
		})
		for name := range returned {
			if anyField[name] {
				handed[name] = true
			}
		}
		// The give-up the comment above promises, implemented: a spec
		// whose ADDRESS is taken leaves this walk's sight — `fill(&s)` may
		// be exactly where the field is set — so it is not judged. Without
		// this, `fill(&s); BuildExecutor(s)` was flagged: correct code
		// accused by a gate, which costs more than a miss because nobody
		// looks for it.
		escaped := map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			u, ok := m.(*ast.UnaryExpr)
			if !ok || u.Op != token.AND {
				return true
			}
			if id, ok := u.X.(*ast.Ident); ok {
				escaped[id.Name] = true
			}
			return true
		})
		for name := range declared {
			switch {
			case assigned[name]:
				// Decided HERE, in plain sight. This arm comes first on
				// purpose: with `escaped` ahead of it, a site that set both
				// guarded fields three lines apart and also passed `&s` to a
				// helper for an unrelated field was declared unjudgeable —
				// the gate failed, demanding an allowlist entry, with a
				// message that was factually false about the file.
			case returned[name] && !anyField[name]:
				// A zero-value sentinel returned beside an error. It builds
				// nothing, so it owes nothing — and reporting it as
				// unjudgeable would be noise about a variable nobody uses.
			case returned[name]:
				// A factory that FILLS a spec and returns it: the caller is
				// the judgeable site, not this one. Reporting it as missing
				// accused a legitimate shared-base helper whose caller
				// decides the fields.
				unread = true
			case !handed[name], escaped[name]:
				// Declared and then out of sight: handed nowhere this walk
				// can see, or its address passed to a callee that may be
				// exactly where the field is set. Not judged — and said so,
				// because a site drifting into this shape otherwise leaves
				// the guard covering nothing and reporting nothing.
				unread = true
			default:
				missing = true
			}
		}
		return true
	})
	return missing, unread
}

// markElidedElements records the type-less inner literals of a container
// whose element type is an ExecutorSpec, so `map[string]ExecutorSpec{"a":
// {…}}` is judged like a spelled-out one.
//
// depth is how many container levels still separate lit from the specs. At
// depth 0 its direct elements ARE the specs; descending further would reach
// a spec's own field values, which are not specs.
func markElidedElements(lit *ast.CompositeLit, depth int, elided map[ast.Node]bool) {
	for _, el := range lit.Elts {
		inner := el
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			inner = kv.Value
		}
		c, ok := inner.(*ast.CompositeLit)
		if !ok {
			continue
		}
		if depth == 0 {
			if c.Type == nil {
				elided[c] = true
			}
			continue
		}
		markElidedElements(c, depth-1, elided)
	}
}

// isBuildExecutorCall matches `BuildExecutor(…)` / `runview.BuildExecutor(…)`.
func isBuildExecutorCall(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "BuildExecutor"
	case *ast.SelectorExpr:
		return f.Sel.Name == "BuildExecutor"
	}
	return false
}

// runviewPackageIdents returns every identifier this file can spell the
// runview package with: its alias when it has one, its default name
// otherwise, and "." for a dot-import (which entitles the bare spelling).
func runviewPackageIdents(file *ast.File) map[string]bool {
	const path = `"github.com/SocialGouv/iterion/pkg/runview"`
	out := map[string]bool{}
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != path {
			continue
		}
		if imp.Name == nil {
			out["runview"] = true
			continue
		}
		if imp.Name.Name != "_" {
			out[imp.Name.Name] = true
		}
	}
	return out
}

// isExecutorSpecType matches the type EXACTLY, so a neighbour named
// MyExecutorSpec is not mistaken for it. pkgNames are the identifiers the
// file under examination may prefix it with; bare says whether the
// unqualified spelling means ours in that file.
func isExecutorSpecType(expr ast.Expr, pkgNames map[string]bool, bare bool) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return bare && t.Name == "ExecutorSpec"
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && pkgNames[pkg.Name] && t.Sel.Name == "ExecutorSpec"
	case *ast.StarExpr:
		return isExecutorSpecType(t.X, pkgNames, bare)
	}
	return false
}
