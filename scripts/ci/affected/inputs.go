package main

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// inputsFile is the checked-in declarations file, repo-relative.
const inputsFile = "scripts/ci/affected/inputs.yaml"

// rule is one path glob with the reason it is there. Every rule says why:
// a glob nobody can explain is a glob nobody dares to remove or narrow.
type rule struct {
	Pattern string `yaml:"pattern"`
	Why     string `yaml:"why"`
}

// readDecl declares the repository paths one package's tests read outside
// the package directory. The guard test (guard_test.go) refuses a test that
// reaches outside its directory without one.
//
//   - Reads: path globs; a changed file matching one selects the package.
//   - Module: the tests compile or type-check the whole module (`go list
//     -export ./...`, `go list -test ./...`): any changed Go file, and any
//     change to a file a package embeds, selects the package.
//   - Reach: the tests build these import paths (`go build ./cmd/iterion`):
//     a production change in their dependency closure selects the package.
//
// An entry with none of the three declares that the scan's hit reads
// nothing of the repository; its why says how that was established.
type readDecl struct {
	Reads  []string `yaml:"reads"`
	Module bool     `yaml:"module"`
	Reach  []string `yaml:"reach"`
	Why    string   `yaml:"why"`
}

// leg maps one non-Go-test CI job of .github/workflows/tests.yml to what can
// change its verdict.
//
//   - Paths: a changed file matching one selects the leg.
//   - Packages: the leg runs `go test` over these package directories (a
//     `dir/...` suffix covers the subtree); it is selected when the Go
//     selection reaches one of them.
//   - Reach: the leg runs a binary built from these import paths (`go run
//     ./cmd/iterion openapi`, `go vet ./cmd/iterion-desktop/`); it is
//     selected when a production file of a package in that binary's
//     dependency closure changes.
//   - OnDelete: a deleted or renamed file selects the leg (a link checker
//     judges every link target, whatever its type).
//   - Aggregates: the leg is a pure aggregator of these legs.
type leg struct {
	Paths      []string `yaml:"paths"`
	Packages   []string `yaml:"packages"`
	Reach      []string `yaml:"reach"`
	OnDelete   bool     `yaml:"on_delete"`
	Aggregates []string `yaml:"aggregates"`
	Why        string   `yaml:"why"`
}

type inputs struct {
	Global   []rule              `yaml:"global"`
	Packages map[string]readDecl `yaml:"packages"`
	Legs     map[string]leg      `yaml:"legs"`
}

func loadInputs(file string) (*inputs, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read declarations: %w", err)
	}
	var in inputs
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	if err := in.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &in, nil
}

// validate refuses a declaration that cannot mean what it says: an empty or
// malformed glob, a rule without a reason, a leg that names an unknown
// aggregate.
func (in *inputs) validate() error {
	if len(in.Global) == 0 {
		return fmt.Errorf("no global triggers declared")
	}
	for _, r := range in.Global {
		if err := checkGlob(r.Pattern); err != nil {
			return fmt.Errorf("global: %w", err)
		}
		if strings.TrimSpace(r.Why) == "" {
			return fmt.Errorf("global %q: no why", r.Pattern)
		}
	}
	for dir, d := range in.Packages {
		if dir == "" || strings.HasSuffix(dir, "/") || path.Clean(dir) != dir {
			return fmt.Errorf("package %q: not a clean repo-relative directory", dir)
		}
		if strings.TrimSpace(d.Why) == "" {
			return fmt.Errorf("package %s: no why", dir)
		}
		for _, g := range d.Reads {
			if err := checkGlob(g); err != nil {
				return fmt.Errorf("package %s: %w", dir, err)
			}
		}
	}
	for name, l := range in.Legs {
		if strings.TrimSpace(l.Why) == "" {
			return fmt.Errorf("leg %s: no why", name)
		}
		for _, g := range l.Paths {
			if err := checkGlob(g); err != nil {
				return fmt.Errorf("leg %s: %w", name, err)
			}
		}
		for _, a := range l.Aggregates {
			if _, ok := in.Legs[a]; !ok {
				return fmt.Errorf("leg %s aggregates unknown leg %s", name, a)
			}
		}
		if len(l.Paths)+len(l.Packages)+len(l.Reach)+len(l.Aggregates) == 0 && !l.OnDelete {
			return fmt.Errorf("leg %s: nothing can select it", name)
		}
	}
	return nil
}

func (in *inputs) legNames() []string {
	names := make([]string, 0, len(in.Legs))
	for n := range in.Legs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// checkGlob refuses a pattern matchGlob would misread.
func checkGlob(g string) error {
	if g == "" || strings.HasPrefix(g, "/") || strings.HasPrefix(g, "./") || strings.HasSuffix(g, "/") {
		return fmt.Errorf("glob %q: want a repo-relative slash path", g)
	}
	for _, seg := range strings.Split(g, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("glob %q: empty or relative segment", g)
		}
		if seg != "**" && strings.Contains(seg, "**") {
			return fmt.Errorf("glob %q: ** must be a whole segment", g)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("glob %q: %w", g, err)
		}
	}
	return nil
}

// matchGlob reports whether the slash path p matches pattern g, where a `**`
// segment matches zero or more whole segments and every other segment is a
// path.Match pattern.
func matchGlob(g, p string) bool {
	return matchSegs(strings.Split(g, "/"), strings.Split(p, "/"))
}

func matchSegs(g, p []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			for i := 0; i <= len(p); i++ {
				if matchSegs(g[1:], p[i:]) {
					return true
				}
			}
			return false
		}
		if len(p) == 0 {
			return false
		}
		if ok, _ := path.Match(g[0], p[0]); !ok {
			return false
		}
		g, p = g[1:], p[1:]
	}
	return len(p) == 0
}

// firstMatch returns the first glob of gs that matches p, or "".
func firstMatch(gs []string, p string) string {
	for _, g := range gs {
		if matchGlob(g, p) {
			return g
		}
	}
	return ""
}
