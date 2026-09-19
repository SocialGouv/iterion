package repomap

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// goPackages maps the module's own Go packages: what each one is for, in
// its own words, and the interfaces it exposes.
//
// Interfaces are the column that earns the file. This project's stated
// architecture is that a new capability implements an existing seam
// rather than adding a branch to the core — so "which interfaces exist,
// and where" is the question an author asks before writing code, and the
// one that costs the most greps to answer.
//
// The extractor uses go/parser from the standard library: iterion is
// written in Go, so its own call graph is exact rather than approximated.
// golang.org/x/tools is deliberately not a dependency of this package.
type goPackages struct{}

func (goPackages) Stem() string  { return "packages" }
func (goPackages) Title() string { return "Package map" }

type pkgRow struct {
	Dir        string
	Name       string
	Doc        string
	Interfaces []string
	Files      int
	Exported   int
}

func (g goPackages) Extract(root string) (string, error) {
	var rows []pkgRow
	err := walkDirs(root, func(rel string, entries []os.DirEntry) error {
		row, ok, err := parsePackageDir(filepath.Join(root, rel), rel, entries)
		if err != nil {
			return err
		}
		if ok {
			rows = append(rows, row)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Dir < rows[j].Dir })

	var b strings.Builder
	fmt.Fprintf(&b, "%d packages, excluding vendored and generated trees. "+
		"The third column lists exported **interfaces** — the seams a new "+
		"variant plugs into rather than branching the core.\n\n", len(rows))
	b.WriteString("| Package | What it is | Interfaces | Files · exported |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, r := range rows {
		ifaces := "—"
		if len(r.Interfaces) > 0 {
			ifaces = "`" + strings.Join(r.Interfaces, "`, `") + "`"
		}
		doc := r.Doc
		if doc == "" {
			doc = "—"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %d · %d |\n", r.Dir, doc, ifaces, r.Files, r.Exported)
	}
	return b.String(), nil
}

// parsePackageDir reads one directory's non-test Go files. It returns
// ok=false for a directory that holds no package.
func parsePackageDir(abs, rel string, entries []os.DirEntry) (pkgRow, bool, error) {
	fset := token.NewFileSet()
	row := pkgRow{Dir: rel}
	var docs []string
	seen := map[string]bool{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(abs, name))
		if err != nil {
			return row, false, fmt.Errorf("read %s/%s: %w", rel, name, err)
		}
		file, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			// A file the parser refuses is reported, not skipped: a map
			// that silently omits what it cannot read is a map that lies
			// by the size of its blind spot.
			return row, false, fmt.Errorf("parse %s/%s: %w", rel, name, err)
		}
		row.Files++
		if row.Name == "" {
			row.Name = file.Name.Name
		}
		// A package comment on doc.go wins; otherwise the first one seen.
		if file.Doc != nil {
			text := file.Doc.Text()
			if name == "doc.go" {
				docs = append([]string{text}, docs...)
			} else {
				docs = append(docs, text)
			}
		}
		collectExports(file, &row, seen)
	}
	if row.Files == 0 {
		return row, false, nil
	}
	if len(docs) > 0 {
		row.Doc = firstSentence(stripPackagePrefix(docs[0], row.Name), 150)
	}
	sort.Strings(row.Interfaces)
	return row, true, nil
}

// collectExports counts a file's exported declarations and records the
// names of its exported interface types.
// collectExports counts EVERY exported declaration — functions, methods,
// types, constants and variables. Counting only functions and types made
// the number wrong on 174 of 238 packages (4 418 against a true 10 035,
// `pkg/dsl/parser` reading 22 for 220), and no const, var or method could
// ever move the rendered bytes, so the freshness gate could not see the
// error either.
func collectExports(file *ast.File, row *pkgRow, seen map[string]bool) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.IsExported() {
				row.Exported++ // methods included: they are part of the surface
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					row.Exported++
					if _, isIface := s.Type.(*ast.InterfaceType); isIface && !seen[s.Name.Name] {
						seen[s.Name.Name] = true
						row.Interfaces = append(row.Interfaces, s.Name.Name)
					}
				case *ast.ValueSpec:
					for _, id := range s.Names {
						if id.IsExported() {
							row.Exported++
						}
					}
				}
			}
		}
	}
}

// stripPackagePrefix drops the "Package foo " opening every Go doc
// comment starts with, so the column reads as a description rather than
// as the same two words repeated down the page.
func stripPackagePrefix(text, name string) string {
	text = strings.TrimSpace(text)
	prefix := "Package " + name + " "
	if !strings.HasPrefix(text, prefix) {
		return text
	}
	text = strings.TrimSpace(text[len(prefix):])
	// "Package eventbus is the spine…" would otherwise open the column
	// with "is the spine…". Only the copulas are dropped: a verb like
	// "provides" or "implements" carries meaning and stays.
	for _, copula := range []string{"is ", "are "} {
		if strings.HasPrefix(text, copula) {
			return strings.TrimSpace(text[len(copula):])
		}
	}
	return text
}
