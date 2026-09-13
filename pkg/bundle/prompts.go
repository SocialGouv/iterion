package bundle

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// MergePromptFiles is the ONE rule every surface merges bundle prompts by
// — a bundle on disk (runview.MergeBundlePrompts), a bundle still in
// memory (a scaffold compiling before it writes, a cloud bot's stored
// files): a file whose bundle-relative slash path is `prompts/<name>.md`
// (the suffix in any case, the top level of prompts/ only) declares the
// prompt `<name>`, unless the workflow declares that name itself.
// Deterministic in path order. originDir, when set, prefixes the
// declaration's origin so a diagnostic inside the body points at the file
// on disk. It lives here, a leaf, so the scaffold and the server reach it
// without pulling the run engine in.
func MergePromptFiles(f *ast.File, files map[string]string, originDir string) {
	if f == nil || len(files) == 0 {
		return
	}
	declared := make(map[string]struct{}, len(f.Prompts))
	for _, p := range f.Prompts {
		declared[p.Name] = struct{}{}
	}
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		if path.Dir(rel) != DirPrompts || !strings.HasSuffix(strings.ToLower(rel), ".md") {
			continue
		}
		name := path.Base(rel)
		stem := strings.TrimSuffix(name, path.Ext(name))
		if _, exists := declared[stem]; exists {
			// Workflow-declared prompt wins on name collision.
			continue
		}
		origin := rel
		if originDir != "" {
			origin = filepath.Join(originDir, filepath.FromSlash(rel))
		}
		// The declaration's origin is the markdown file: a diagnostic on a
		// reference inside the body then points there (line 1 — the file
		// IS the body), not at the main.bot line of the node that consumes
		// the prompt, which contains no reference at all.
		f.Prompts = append(f.Prompts, &ast.PromptDecl{
			Name: stem,
			Body: files[rel],
			Span: ast.Span{Start: ast.Pos{File: origin, Line: 1, Column: 1}},
		})
		declared[stem] = struct{}{}
	}
}
