package main

import (
	"fmt"
	"path"
	"strings"
)

// change is one path the diff touches. Deleted reports a deletion (a rename
// arrives as a deletion of the old path plus an addition of the new one).
type change struct {
	Path    string
	Deleted bool
}

// result is the selector's answer, the JSON the CLI prints.
type result struct {
	Base    string   `json:"base"`
	Head    string   `json:"head"`
	Changed []string `json:"changed"`
	// All: a global trigger fired, or a file nothing claims changed. Every
	// package and every leg is selected; Reasons say why.
	All      bool          `json:"all"`
	Reasons  []string      `json:"reasons,omitempty"`
	Packages []selectedPkg `json:"packages"`
	Legs     []selectedLeg `json:"legs"`
	// TestPackages is the number of packages with tests in the graph, the
	// denominator of a share.
	TestPackages int `json:"test_packages"`
}

type selectedPkg struct {
	ImportPath string   `json:"import_path"`
	Dir        string   `json:"dir"`
	Why        []string `json:"why"`
}

type selectedLeg struct {
	Name string   `json:"name"`
	Why  []string `json:"why"`
}

// claims reports whether a match of glob g vouches that the file is known.
// A glob opening on `**` (a whole-tree sweep: `**`, `**/*.go`) matches files
// nobody thought of, so it selects its reader without vouching that no
// undeclared reader exists; only a rooted glob does.
func claims(g string) bool { return !strings.HasPrefix(g, "**") }

// claimedBy reports whether a rooted glob of gs matches p.
func claimedBy(gs []string, p string) bool {
	for _, g := range gs {
		if claims(g) && matchGlob(g, p) {
			return true
		}
	}
	return false
}

// maxWhy bounds the reasons kept per package; the count of the rest is said.
const maxWhy = 6

type reasons struct {
	list  []string
	seen  map[string]bool
	extra int
}

func (r *reasons) add(s string) {
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	if r.seen[s] {
		return
	}
	r.seen[s] = true
	if len(r.list) < maxWhy {
		r.list = append(r.list, s)
		return
	}
	r.extra++
}

func (r *reasons) out() []string {
	out := append([]string(nil), r.list...)
	if r.extra > 0 {
		out = append(out, fmt.Sprintf("... and %d more", r.extra))
	}
	return out
}

// selectTests computes the selection for changes over g, under the
// declarations in. It is a pure function: the CLI and the replay feed it.
func selectTests(g *graph, in *inputs, changes []change) result {
	res := result{TestPackages: len(g.testPackages())}
	for _, c := range changes {
		res.Changed = append(res.Changed, c.Path)
	}

	direct := map[string]*reasons{}  // package -> why its tests are selected directly
	changed := map[string][]string{} // package -> production files that changed it
	legWhy := map[string]*reasons{}
	declared := map[string]*reasons{} // declaring package dir -> the reads that hit
	addDirect := func(ip, why string) {
		if direct[ip] == nil {
			direct[ip] = &reasons{}
		}
		direct[ip].add(why)
	}
	addLeg := func(name, why string) {
		if legWhy[name] == nil {
			legWhy[name] = &reasons{}
		}
		legWhy[name].add(why)
	}
	var allReasons []string
	anyDeleted := false
	goFile := "" // the first changed Go file, test files included

	for _, c := range changes {
		p := c.Path
		claimed := false
		if c.Deleted {
			anyDeleted = true
		}
		if goFile == "" && strings.HasSuffix(p, ".go") {
			goFile = p
		}
		for _, r := range in.Global {
			if matchGlob(r.Pattern, p) {
				allReasons = append(allReasons, fmt.Sprintf("%s matches global trigger %s (%s)", p, r.Pattern, r.Why))
				claimed = true
				break
			}
		}

		// Ownership: every package whose directory contains the file reads
		// it without leaving its directory (testdata, fixtures, embeds).
		dir := path.Dir(p)
		own := g.byDir[dir]
		if own != nil && isProductionFile(own, p) {
			changed[own.importPath] = append(changed[own.importPath], p)
			claimed = true
		}
		for _, pk := range embeddedBy(g, p) {
			changed[pk.importPath] = append(changed[pk.importPath], p)
			claimed = true
		}
		for d := dir; ; d = path.Dir(d) {
			if pk := g.byDir[d]; pk != nil {
				if d == dir {
					addDirect(pk.importPath, "changed: "+p)
				} else {
					addDirect(pk.importPath, "contains: "+p)
				}
				claimed = true
			}
			if d == "." || d == "/" {
				break
			}
		}
		if strings.HasSuffix(p, ".go") && own == nil && !underFixture(g, p) {
			allReasons = append(allReasons, fmt.Sprintf("%s is a Go file in a directory that is no package at head (deleted or excluded package): its importers cannot be computed", p))
			claimed = true
		}

		// Declared reads.
		for dirDecl, d := range in.Packages {
			if gl := firstMatch(d.Reads, p); gl != "" {
				if declared[dirDecl] == nil {
					declared[dirDecl] = &reasons{}
				}
				declared[dirDecl].add(fmt.Sprintf("reads %s (declared %s)", p, gl))
				claimed = claimed || claimedBy(d.Reads, p)
			}
		}

		// Legs by path.
		for _, name := range in.legNames() {
			if gl := firstMatch(in.Legs[name].Paths, p); gl != "" {
				addLeg(name, fmt.Sprintf("%s matches %s", p, gl))
				claimed = claimed || claimedBy(in.Legs[name].Paths, p)
			}
		}

		if !claimed {
			allReasons = append(allReasons, fmt.Sprintf("%s is claimed by no package directory and no rooted glob of a declaration or leg (unknown file = everything)", p))
		}
	}
	res.Changed = sortedUnique(res.Changed)
	if len(allReasons) > 0 {
		return selectAll(g, in, res, allReasons)
	}

	// Resolve declarations to import paths.
	for _, dirDecl := range sortedKeys(declared) {
		r := declared[dirDecl]
		pk := g.byDir[dirDecl]
		if pk == nil {
			allReasons = append(allReasons, fmt.Sprintf("%s declares reads but is no package at head", dirDecl))
			continue
		}
		for _, w := range r.list {
			addDirect(pk.importPath, w)
		}
	}
	if len(allReasons) > 0 {
		return selectAll(g, in, res, allReasons)
	}

	// Closure: a test binary linking a changed package.
	changedPaths := sortedKeys(changed)
	for _, ip := range g.testPackages() {
		pk := g.pkgs[ip]
		for _, c := range changedPaths {
			if c == ip || pk.testDeps[c] {
				if c == ip {
					addDirect(ip, "changed: "+strings.Join(changed[c], ", "))
				} else {
					addDirect(ip, fmt.Sprintf("links %s (changed: %s)", strings.TrimPrefix(c, g.module+"/"), firstAndCount(changed[c])))
				}
			}
		}
	}
	// Declared builds: whole-module type-checks and binaries.
	for _, dirDecl := range sortedKeys(in.Packages) {
		d := in.Packages[dirDecl]
		if !d.Module && len(d.Reach) == 0 {
			continue
		}
		pk := g.byDir[dirDecl]
		if pk == nil {
			return selectAll(g, in, res, []string{fmt.Sprintf("%s declares module or reach but is no package at head", dirDecl)})
		}
		if d.Module {
			if len(changedPaths) > 0 {
				addDirect(pk.importPath, fmt.Sprintf("compiles the whole module (declared module), which a change to %s alters", strings.TrimPrefix(changedPaths[0], g.module+"/")))
			} else if goFile != "" {
				addDirect(pk.importPath, fmt.Sprintf("compiles the whole module (declared module), which %s alters", goFile))
			}
		}
		for _, bin := range d.Reach {
			bp := g.byDir[bin]
			if bp == nil {
				return selectAll(g, in, res, []string{fmt.Sprintf("%s declares reach %s, which is no package at head", dirDecl, bin)})
			}
			for _, c := range changedPaths {
				if c == bp.importPath || bp.prodDeps[c] {
					addDirect(pk.importPath, fmt.Sprintf("builds %s (declared reach), which links %s", bin, strings.TrimPrefix(c, g.module+"/")))
				}
			}
		}
	}
	for _, ip := range sortedKeys(direct) {
		pk := g.pkgs[ip]
		if pk == nil || !pk.hasTests {
			continue
		}
		res.Packages = append(res.Packages, selectedPkg{ImportPath: ip, Dir: pk.dir, Why: direct[ip].out()})
	}

	// Legs by Go selection and by binary reach.
	for _, name := range in.legNames() {
		l := in.Legs[name]
		for _, spec := range l.Packages {
			for _, sp := range res.Packages {
				if dirMatches(spec, sp.Dir) {
					addLeg(name, "Go selection reaches "+sp.Dir)
				}
			}
		}
		for _, bin := range l.Reach {
			bp := g.byDir[bin]
			if bp == nil {
				return selectAll(g, in, res, []string{fmt.Sprintf("leg %s reaches %s, which is no package at head", name, bin)})
			}
			for _, c := range changedPaths {
				if c == bp.importPath || bp.prodDeps[c] {
					addLeg(name, fmt.Sprintf("%s links %s (changed: %s)", bin, strings.TrimPrefix(c, g.module+"/"), firstAndCount(changed[c])))
				}
			}
		}
		if l.OnDelete && anyDeleted {
			addLeg(name, "a file is deleted or renamed")
		}
	}
	// Aggregators, to a fixed point.
	for again := true; again; {
		again = false
		for _, name := range in.legNames() {
			if legWhy[name] != nil {
				continue
			}
			for _, a := range in.Legs[name].Aggregates {
				if legWhy[a] != nil {
					addLeg(name, "aggregates "+a)
					again = true
					break
				}
			}
		}
	}
	for _, name := range in.legNames() {
		if legWhy[name] != nil {
			res.Legs = append(res.Legs, selectedLeg{Name: name, Why: legWhy[name].out()})
		}
	}
	if res.Packages == nil {
		res.Packages = []selectedPkg{}
	}
	if res.Legs == nil {
		res.Legs = []selectedLeg{}
	}
	return res
}
