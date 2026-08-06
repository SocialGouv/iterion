package runview

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

	repoRoot := filepath.Join("..", "..")
	var offenders []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", ".iterion", "studio", "testdata":
				return filepath.SkipDir
			}
			// A directory carrying its own .git is a NESTED CHECKOUT — a git
			// worktree or a sibling clone an operator keeps on disk. Its files
			// belong to another tree (none are tracked here), and its older
			// copies would report as offenders of a rule they predate. Detect
			// them by that marker rather than by directory name: where someone
			// parks their checkouts is their business, not this test's.
			if path != repoRoot {
				if _, statErr := os.Stat(filepath.Join(path, ".git")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, repoRoot+string(filepath.Separator)))
		if _, ok := exempt[rel]; ok {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", rel, perr)
			return nil
		}
		if executorSpecMissesBotID(file) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("these build an executor without deciding its bot identity, so its bot-scoped memory "+
			"falls back to the WORKFLOW name and silently diverges from every surface that sets it: %v\n"+
			"Set BotID (see ResolveBotID), or add the file to this test's exempt map with a reason.",
			offenders)
	}
}

// executorSpecMissesBotID reports whether a file builds an ExecutorSpec without
// setting BotID, by either construction shape: a composite literal, or a
// variable whose fields are assigned one at a time.
func executorSpecMissesBotID(file *ast.File) bool {
	missing := false

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
		if elem != nil && isExecutorSpecType(elem) {
			for _, el := range lit.Elts {
				inner := el
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					inner = kv.Value
				}
				if c, ok := inner.(*ast.CompositeLit); ok && c.Type == nil {
					elided[c] = true
				}
			}
		}
		return true
	})
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || (!isExecutorSpecType(lit.Type) && !elided[lit]) {
			return true
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "BotID" {
					return true
				}
			}
		}
		missing = true
		return true
	})
	if missing {
		return true
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
				if isExecutorSpecType(v.Type) {
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
					if isExecutorSpecType(call.Args[0]) && i < len(v.Lhs) {
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
				if id, ok := arg.(*ast.Ident); ok {
					handed[id.Name] = true
				}
			}
			return true
		})
		assigned := map[string]bool{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			as, ok := m.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range as.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "BotID" {
					continue
				}
				if id, ok := sel.X.(*ast.Ident); ok {
					assigned[id.Name] = true
				}
			}
			return true
		})
		for name := range declared {
			if handed[name] && !assigned[name] {
				missing = true
			}
		}
		return true
	})
	return missing
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

// isExecutorSpecType matches the type EXACTLY, so a neighbour named
// MyExecutorSpec is not mistaken for it.
func isExecutorSpecType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "ExecutorSpec"
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		return ok && pkg.Name == "runview" && t.Sel.Name == "ExecutorSpec"
	case *ast.StarExpr:
		return isExecutorSpecType(t.X)
	}
	return false
}
