package bundle

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// MaxSyntaxProfile is the highest `dsl: N` profile declared by a bundle's
// executable sources: its main.bot and every child a `subbot source:`
// reaches from it, transitively, within the bundle. files maps a
// bundle-relative path to its content, as a bot source carries it. The
// second result names the files that declare the profile returned.
//
// Why the children count: a cloud runner receives the main workflow as an
// AST, but re-parses a subbot child as TEXT with its own binary, so a child
// written in a newer profile than the runner reads fails at its first parse
// — which is what a declared `requires.iterion` floor exists to refuse at
// admission instead. A source outside the bundle (absolute, or climbing out
// of it) is not part of the bundle and is not read; a missing one is the
// compiler's to report.
func MaxSyntaxProfile(files map[string]string) (profile int, declaredBy []string) {
	return maxSyntaxProfile(func(rel string) (string, bool) {
		src, ok := files[rel]
		return src, ok
	})
}

// MaxSyntaxProfileDir is MaxSyntaxProfile over a bundle directory on disk,
// reading only the files the walk reaches.
func MaxSyntaxProfileDir(dir string) (profile int, declaredBy []string) {
	return maxSyntaxProfile(func(rel string) (string, bool) {
		src, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", false
		}
		return string(src), true
	})
}

func maxSyntaxProfile(read func(rel string) (string, bool)) (int, []string) {
	profile := 0
	var declaredBy []string
	visited := map[string]bool{}
	var visit func(rel string)
	visit = func(rel string) {
		rel = filepath.ToSlash(filepath.Clean(rel))
		if visited[rel] || rel == "." || strings.HasPrefix(rel, "../") || rel == ".." || filepath.IsAbs(rel) {
			return
		}
		visited[rel] = true
		src, ok := read(rel)
		if !ok {
			return
		}
		pr := parser.Parse(rel, src)
		p := pr.File.EffectiveProfile()
		switch {
		case p > profile:
			profile = p
			declaredBy = nil
			if p > 1 {
				declaredBy = []string{rel}
			}
		case p == profile && p > 1:
			declaredBy = append(declaredBy, rel)
		}
		base := filepath.ToSlash(filepath.Dir(rel))
		for _, sb := range pr.File.Subbots {
			if sb.Source == "" {
				continue
			}
			child := sb.Source
			if !filepath.IsAbs(child) && base != "." {
				child = filepath.ToSlash(filepath.Join(base, child))
			}
			visit(child)
		}
	}
	visit(MainBotFile)
	sort.Strings(declaredBy)
	return profile, declaredBy
}
