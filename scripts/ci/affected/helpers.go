package main

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// sourceExts are the files `go build` compiles into a package besides .go.
var sourceExts = map[string]bool{".go": true, ".c": true, ".h": true, ".s": true, ".S": true, ".cc": true, ".cpp": true, ".cxx": true, ".hh": true, ".hpp": true, ".m": true, ".f": true, ".F": true, ".syso": true}

// isProductionFile reports whether a change to p alters what pk compiles for
// its importers: a non-test source file directly in its directory, or a file
// its //go:embed patterns name.
func isProductionFile(pk *pkgInfo, p string) bool {
	if embeds(pk, p) {
		return true
	}
	if path.Dir(p) != pk.dir || strings.HasSuffix(p, "_test.go") {
		return false
	}
	return sourceExts[path.Ext(p)]
}

// embeds reports whether pk's non-test package embeds p, or would: a file
// head's EmbedFiles lists, or one a //go:embed pattern of pk names. The
// pattern judges what head's file list cannot: a file deleted from a
// directory or glob embed (base embedded it, head no longer does) and a file
// added under one.
func embeds(pk *pkgInfo, p string) bool {
	if pk.embeds[p] {
		return true
	}
	for _, pat := range pk.embedPatterns {
		if embedMatches(pat, pk.dir, p) {
			return true
		}
	}
	return false
}

// embedMatches reports whether the //go:embed pattern pat of the package in
// dir names the repo-relative file p, under go:embed's rules: the pattern is
// a path.Match glob over slash paths relative to dir; a match that is a
// directory embeds its whole subtree, except the files and directories
// whose name starts with '.' or '_' — which an `all:` pattern includes. A
// glob matching such a name directly (`image/*` and `image/.keep`) embeds
// it either way.
func embedMatches(pat, dir, p string) bool {
	all := strings.HasPrefix(pat, "all:")
	pat = strings.TrimPrefix(pat, "all:")
	rel := p
	if dir != "." {
		var ok bool
		if rel, ok = strings.CutPrefix(p, dir+"/"); !ok {
			return false
		}
	}
	segs := strings.Split(rel, "/")
	k := strings.Count(pat, "/") + 1
	if k > len(segs) {
		return false
	}
	if ok, err := path.Match(pat, strings.Join(segs[:k], "/")); err != nil || !ok {
		return false
	}
	if all {
		return true
	}
	for _, s := range segs[k:] {
		if strings.HasPrefix(s, ".") || strings.HasPrefix(s, "_") {
			return false
		}
	}
	return true
}

// embeddedBy returns the packages, other than the file's own directory's,
// that embed p from a subdirectory.
func embeddedBy(g *graph, p string) []*pkgInfo {
	var out []*pkgInfo
	for d := path.Dir(path.Dir(p)); ; d = path.Dir(d) {
		if pk := g.byDir[d]; pk != nil && embeds(pk, p) {
			out = append(out, pk)
		}
		if d == "." || d == "/" {
			return out
		}
	}
}

// underFixture reports whether a Go file at p is outside any buildable
// package of the module by construction: under testdata, an underscore or
// dot directory (all ignored by `./...`), or inside a nested module.
func underFixture(g *graph, p string) bool {
	segs := strings.Split(path.Dir(p), "/")
	for _, s := range segs {
		if s == "testdata" || strings.HasPrefix(s, "_") || strings.HasPrefix(s, ".") {
			return true
		}
	}
	for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
		if g.nestedModules[d] {
			return true
		}
	}
	return false
}

// selectAll is the answer when a global trigger fires: every test package and
// every leg, each carrying the reasons.
func selectAll(g *graph, in *inputs, res result, why []string) result {
	why = sortedUnique(why)
	res.All = true
	res.Reasons = why
	head := "all: " + why[0]
	if len(why) > 1 {
		head += fmt.Sprintf(" (and %d more reasons)", len(why)-1)
	}
	res.Packages = nil
	for _, ip := range g.testPackages() {
		res.Packages = append(res.Packages, selectedPkg{ImportPath: ip, Dir: g.pkgs[ip].dir, Why: []string{head}})
	}
	res.Legs = nil
	for _, name := range in.legNames() {
		res.Legs = append(res.Legs, selectedLeg{Name: name, Why: []string{head}})
	}
	return res
}

// dirMatches reports whether package dir d is named by spec: an exact
// directory, `x/...` for x and everything below it, or `...` for every one.
func dirMatches(spec, d string) bool {
	if spec == "..." {
		return true
	}
	if base, ok := strings.CutSuffix(spec, "/..."); ok {
		return d == base || strings.HasPrefix(d, base+"/")
	}
	return d == spec
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedUnique(s []string) []string {
	m := map[string]bool{}
	for _, x := range s {
		m[x] = true
	}
	return sortedKeys(m)
}

func firstAndCount(files []string) string {
	if len(files) == 1 {
		return files[0]
	}
	return fmt.Sprintf("%s and %d more", files[0], len(files)-1)
}
