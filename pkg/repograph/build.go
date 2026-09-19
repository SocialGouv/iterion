package repograph

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// skipDir names the trees the graph never describes. Vendored code would
// dominate every ranking; sibling worktrees and run scratch are not this
// repository.
var skipDir = map[string]bool{
	"vendor": true, "node_modules": true, ".git": true, ".works": true,
	".repos": true, ".iterion": true, ".devbox": true, "graphify-out": true,
	".claude": true, ".task": true, "dist": true, ".pnpm-store": true,
	"testdata": true,
}

// Build walks the tree once and returns the finalised graph.
func Build(root string) (*Graph, error) {
	g := NewGraph()
	modulePath, err := readModulePath(root)
	if err != nil {
		return nil, err
	}
	if err := buildGo(g, root, modulePath); err != nil {
		return nil, err
	}
	if err := buildDocs(g, root); err != nil {
		return nil, err
	}
	if err := buildBots(g, root); err != nil {
		return nil, err
	}
	g.BuiltAt = time.Now().UTC().Format(time.RFC3339)
	g.Finalise()
	return g, nil
}

// readModulePath returns the module path from go.mod. Without it an
// import cannot be told from a third-party one, so a missing go.mod is
// an error rather than a degraded build.
func readModulePath(root string) (string, error) {
	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("repograph: read go.mod: %w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("repograph: go.mod carries no module line")
}

// buildGo adds packages, files, symbols, imports and calls.
//
// Calls are resolved by NAME, from the file's own import table: a
// selector `foo.Bar()` where `foo` is an import alias resolves to that
// package's `Bar`, and a bare `Bar()` resolves inside the package that
// declares it. That is short of what go/types would prove and well
// beyond what a text search can tell — and it needs no dependency
// outside the standard library.
func buildGo(g *Graph, root, modulePath string) error {
	type pkgInfo struct {
		dir     string
		symbols map[string]bool
	}
	packages := map[string]*pkgInfo{}
	type fileUnit struct {
		rel  string
		pkg  string
		file *ast.File
	}
	var units []fileUnit
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, abs)
			if rel != "." && skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		relPath, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		relPath = filepath.ToSlash(relPath)
		dir := path.Dir(relPath)
		src, readErr := os.ReadFile(abs)
		if readErr != nil {
			return fmt.Errorf("repograph: read %s: %w", relPath, readErr)
		}
		f, parseErr := parser.ParseFile(fset, relPath, src, parser.ParseComments|parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("repograph: parse %s: %w", relPath, parseErr)
		}
		p, ok := packages[dir]
		if !ok {
			p = &pkgInfo{dir: dir, symbols: map[string]bool{}}
			packages[dir] = p
			g.AddNode(Node{ID: packageID(dir), Kind: KindPackage, Label: dir, Path: dir})
		}
		g.AddNode(Node{ID: fileID(relPath), Kind: KindFile, Label: path.Base(relPath), Path: relPath})
		g.AddEdge(packageID(dir), fileID(relPath), RelContains)

		for _, decl := range f.Decls {
			for _, name := range declaredNames(decl) {
				p.symbols[name.name] = true
				g.AddNode(Node{
					ID:    symbolID(dir, name.name),
					Kind:  KindSymbol,
					Label: name.name,
					Path:  relPath,
					Line:  fset.Position(name.pos).Line,
					Doc:   name.doc,
				})
				g.AddEdge(fileID(relPath), symbolID(dir, name.name), RelDeclares)
			}
		}
		units = append(units, fileUnit{rel: relPath, pkg: dir, file: f})
		return nil
	})
	if err != nil {
		return err
	}

	// Second pass: imports and calls, now that every package's symbol
	// set is known. Resolving in one pass would miss every call to a
	// package parsed later.
	for _, u := range units {
		aliases := map[string]string{} // local name → repo-relative dir
		for _, imp := range u.file.Imports {
			target := strings.Trim(imp.Path.Value, `"`)
			dir, inModule := moduleDir(modulePath, target)
			if !inModule {
				continue
			}
			g.AddEdge(packageID(u.pkg), packageID(dir), RelImports)
			name := path.Base(dir)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			aliases[name] = dir
		}
		resolveUses(g, u.file, u.pkg, aliases, func(dir, name string) bool {
			p, ok := packages[dir]
			return ok && p.symbols[name]
		})
	}
	return nil
}

type declaredName struct {
	name string
	pos  token.Pos
	doc  string
}

// declaredNames returns a declaration's exported top-level names. Only
// exported ones: an index of every unexported helper would be four times
// the size and answer a question nobody asks of a map.
func declaredNames(decl ast.Decl) []declaredName {
	var out []declaredName
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil || !d.Name.IsExported() {
			return nil
		}
		out = append(out, declaredName{d.Name.Name, d.Name.Pos(), docLine(d.Doc)})
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				if s.Name.IsExported() {
					doc := docLine(s.Doc)
					if doc == "" {
						doc = docLine(d.Doc)
					}
					out = append(out, declaredName{s.Name.Name, s.Name.Pos(), doc})
				}
			case *ast.ValueSpec:
				for _, id := range s.Names {
					if id.IsExported() {
						doc := docLine(s.Doc)
						if doc == "" {
							doc = docLine(d.Doc)
						}
						out = append(out, declaredName{id.Name, id.Pos(), doc})
					}
				}
			}
		}
	}
	return out
}

func docLine(c *ast.CommentGroup) string {
	if c == nil {
		return ""
	}
	return firstSentence(c.Text(), 120)
}

// resolveUses walks a file and records an edge from the enclosing symbol
// to every declared symbol it names — RelCalls in call position, and
// RelReferences everywhere else (a parameter type, a field, a variable).
//
// The second half is what makes the graph answer anything about an
// interface: a seam is referenced, never called, so recording calls alone
// would report "nothing depends on this" about the declarations the whole
// architecture hangs from.
func resolveUses(g *Graph, file *ast.File, pkgDir string, aliases map[string]string, declared func(dir, name string) bool) {
	// Attribution is decided per top-level declaration, not by a cursor
	// that moves as the walk descends: a type's references belong to the
	// type, and a local `type` inside a function body must not capture
	// the rest of that function.
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			owner := packageID(pkgDir)
			if d.Recv == nil && d.Name.IsExported() {
				owner = symbolID(pkgDir, d.Name.Name)
			}
			inspectFor(g, d, pkgDir, owner, aliases, declared)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				inspectFor(g, spec, pkgDir, ownerOfSpec(pkgDir, spec), aliases, declared)
			}
		}
	}
}

// ownerOfSpec attributes a declaration's references to the symbol it
// declares when that symbol is exported — a method or an unexported
// helper belongs to its package, not to a symbol this graph never made.
func ownerOfSpec(pkgDir string, spec ast.Spec) string {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		if s.Name.IsExported() {
			return symbolID(pkgDir, s.Name.Name)
		}
	case *ast.ValueSpec:
		for _, id := range s.Names {
			if id.IsExported() {
				return symbolID(pkgDir, id.Name)
			}
		}
	}
	return packageID(pkgDir)
}

// inspectFor walks one declaration and emits an edge per named symbol.
func inspectFor(g *Graph, root ast.Node, pkgDir, owner string, aliases map[string]string, declared func(dir, name string) bool) {
	// ast.Inspect is pre-order, so a CallExpr is seen before its Fun:
	// marking it here means the Fun is classified as a call when its own
	// visit comes round, and as a reference otherwise.
	callFuns := map[ast.Node]bool{}
	emit := func(dir, name string, rel Rel) {
		if declared(dir, name) {
			g.AddEdge(owner, symbolID(dir, name), rel)
		}
	}
	ast.Inspect(root, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			callFuns[node.Fun] = true
		case *ast.SelectorExpr:
			ident, ok := node.X.(*ast.Ident)
			if !ok {
				return true
			}
			if dir, isImport := aliases[ident.Name]; isImport {
				emit(dir, node.Sel.Name, relFor(callFuns[node]))
			}
			return true
		case *ast.Ident:
			// Object resolution is off, so a bare name is matched against
			// the package's own declared set. A local variable with an
			// exported name would be mis-attributed; Go style makes that
			// rare, and the cost is one extra edge in a ranking rather
			// than a wrong answer about a path.
			if node.IsExported() {
				emit(pkgDir, node.Name, relFor(callFuns[node]))
			}
		}
		return true
	})
}

func relFor(isCall bool) Rel {
	if isCall {
		return RelCalls
	}
	return RelReferences
}

// moduleDir maps an import path to its repo-relative directory, and
// reports whether it belongs to this module at all.
func moduleDir(modulePath, importPath string) (string, bool) {
	if importPath == modulePath {
		return ".", true
	}
	rest, ok := strings.CutPrefix(importPath, modulePath+"/")
	if !ok {
		return "", false
	}
	return rest, true
}

// mdLink matches an inline markdown link's target.
var mdLink = regexp.MustCompile(`\]\(([^)\s]+)`)

// buildDocs adds one node per markdown page and one edge per link
// between two pages in the tree. A link to a heading, a URL or a missing
// file produces no edge — this graph records what exists, and #1233 owns
// what does not.
func buildDocs(g *Graph, root string) error {
	docsRoot := filepath.Join(root, "docs")
	if _, err := os.Stat(docsRoot); err != nil {
		return nil
	}
	type page struct{ rel, body string }
	var pages []page

	err := filepath.WalkDir(docsRoot, func(abs string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		body, readErr := os.ReadFile(abs)
		if readErr != nil {
			return fmt.Errorf("repograph: read %s: %w", rel, readErr)
		}
		g.AddNode(Node{
			ID: docID(rel), Kind: KindDoc, Label: path.Base(rel), Path: rel,
			Doc: firstHeading(string(body)),
		})
		pages = append(pages, page{rel, string(body)})
		return nil
	})
	if err != nil {
		return err
	}

	known := map[string]bool{}
	for _, p := range pages {
		known[p.rel] = true
	}
	for _, p := range pages {
		for _, m := range mdLink.FindAllStringSubmatch(p.body, -1) {
			target := m[1]
			if strings.HasPrefix(target, "#") || strings.Contains(target, "://") ||
				strings.HasPrefix(target, "mailto:") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			if target == "" || !strings.HasSuffix(target, ".md") {
				continue
			}
			resolved := path.Clean(path.Join(path.Dir(p.rel), target))
			if known[resolved] {
				g.AddEdge(docID(p.rel), docID(resolved), RelLinks)
			}
		}
	}
	return nil
}

// buildBots adds one node per bundle, one per skill, and one per node of
// every `.bot` the bundle ships — with the workflow's own edges between
// them. This is the half no external indexer has: the DAG is not
// inferred from the text, it is the compiler's own output.
func buildBots(g *Graph, root string) error {
	botsDir := filepath.Join(root, "bots")
	entries, err := os.ReadDir(botsDir)
	if err != nil {
		return nil // a tree without bots is not an error
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bot := e.Name()
		dir := filepath.Join(botsDir, bot)
		g.AddNode(Node{
			ID: botID(bot), Kind: KindBot, Label: bot,
			Path: filepath.ToSlash(filepath.Join("bots", bot)),
		})
		addBotSkills(g, root, bot, dir)
		if err := addBotWorkflows(g, root, bot, dir); err != nil {
			return err
		}
	}
	return nil
}

func addBotSkills(g *Graph, root, bot, dir string) {
	skills, err := os.ReadDir(filepath.Join(dir, "skills"))
	if err != nil {
		return
	}
	for _, s := range skills {
		if s.IsDir() || !strings.HasSuffix(s.Name(), ".md") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join("bots", bot, "skills", s.Name()))
		g.AddNode(Node{ID: skillID(bot, s.Name()), Kind: KindSkill, Label: s.Name(), Path: rel})
		g.AddEdge(botID(bot), skillID(bot, s.Name()), RelUses)
	}
}

func addBotWorkflows(g *Graph, root, bot, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bot") {
			continue
		}
		abs := filepath.Join(dir, e.Name())
		wf, _, _, err := runview.CompileWorkflowPath(abs)
		if err != nil {
			// A bundle the compiler refuses is reported: a graph that
			// silently omits a workflow is a graph whose "no path
			// between these" answer is worthless.
			return fmt.Errorf("repograph: compile bots/%s/%s: %w", bot, e.Name(), err)
		}
		rel := filepath.ToSlash(filepath.Join("bots", bot, e.Name()))
		addWorkflow(g, bot, e.Name(), rel, wf)
	}
	return nil
}

func addWorkflow(g *Graph, bot, workflow, rel string, wf *ir.Workflow) {
	for id, n := range wf.Nodes {
		g.AddNode(Node{
			ID:    dslNodeID(bot, workflow, id),
			Kind:  KindDSLNode,
			Label: id,
			Path:  rel,
			Doc:   n.NodeKind().String(),
		})
		g.AddEdge(botID(bot), dslNodeID(bot, workflow, id), RelContains)
	}
	for _, e := range wf.Edges {
		if e == nil {
			continue
		}
		g.AddEdge(dslNodeID(bot, workflow, e.From), dslNodeID(bot, workflow, e.To), RelFlows)
	}
}

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			return firstSentence(rest, 100)
		}
	}
	return ""
}

func firstSentence(text string, max int) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}
	if i := strings.Index(text, ". "); i > 0 {
		text = text[:i+1]
	}
	if len(text) > max {
		text = strings.TrimSpace(text[:max]) + "…"
	}
	return text
}
