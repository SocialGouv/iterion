package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// buildVariant is one `go list` configuration. The import graph is the UNION
// over the variants: a file only one build configuration compiles (a `live`
// e2e test, the wails desktop shell, a cgo or !cgo twin) still has its edges,
// so a change it reaches still selects it.
type buildVariant struct {
	name string
	tags string
	cgo  string
}

var buildVariants = []buildVariant{
	{name: "default (CGO_ENABLED=0, the unit legs)", cgo: "0"},
	{name: "cgo (CGO_ENABLED=1, the race legs)", cgo: "1"},
	{name: "live tag (test:live:compile)", tags: "live", cgo: "0"},
	{name: "desktop tags (desktop vets)", tags: "desktop,webkit2_41", cgo: "1"},
}

// pkgInfo is one main-module package, merged over every build variant.
type pkgInfo struct {
	importPath string
	dir        string // repo-relative, slash-separated
	// hasTests: some variant builds a test binary for it.
	hasTests bool
	// testDeps: main-module packages the test binary links.
	testDeps map[string]bool
	// prodDeps: main-module packages the non-test package links.
	prodDeps map[string]bool
	// embeds: repo-relative files the non-test package embeds.
	embeds map[string]bool
}

type graph struct {
	module string
	pkgs   map[string]*pkgInfo // by import path
	byDir  map[string]*pkgInfo // by repo-relative dir
	// nestedModules: repo-relative directories holding a go.mod other than
	// the root's; their Go files are fixtures, never module packages.
	nestedModules map[string]bool
}

// listEntry is the subset of `go list -json` this tool reads.
type listEntry struct {
	ImportPath string
	Dir        string
	ForTest    string
	Standard   bool
	Module     *struct{ Path string }
	Deps       []string
	EmbedFiles []string
	Error      *struct{ Err string }
	DepsErrors []*struct{ Err string }
}

// loadGraph runs `go list -mod=vendor -deps -test -json ./...` in root under
// every build variant, in parallel, and merges the results. Any failing
// variant fails the load: a graph missing a variant's edges under-selects.
func loadGraph(ctx context.Context, root, module string) (*graph, error) {
	outs := make([][]byte, len(buildVariants))
	errs := make([]error, len(buildVariants))
	var wg sync.WaitGroup
	for i, v := range buildVariants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = goList(ctx, root, v)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	g := &graph{module: module, pkgs: map[string]*pkgInfo{}, byDir: map[string]*pkgInfo{}, nestedModules: map[string]bool{}}
	for i, out := range outs {
		if err := g.merge(root, bytes.NewReader(out)); err != nil {
			return nil, fmt.Errorf("go list (%s): %w", buildVariants[i].name, err)
		}
	}
	if len(g.pkgs) == 0 {
		return nil, fmt.Errorf("go list listed no package of module %s", module)
	}
	return g, nil
}

func goList(ctx context.Context, root string, v buildVariant) ([]byte, error) {
	args := []string{"list", "-mod=vendor", "-deps", "-test", "-json"}
	if v.tags != "" {
		args = append(args, "-tags", v.tags)
	}
	args = append(args, "./...")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+v.cgo, "GOFLAGS=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s (%s): %w: %s", strings.Join(args, " "), v.name, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// basePath strips the " [x.test]" suffix go list gives a package recompiled
// for a test binary.
func basePath(p string) string {
	if i := strings.Index(p, " ["); i >= 0 {
		return p[:i]
	}
	return p
}

func (g *graph) inModule(p string) bool {
	return p == g.module || strings.HasPrefix(p, g.module+"/")
}

func (g *graph) get(importPath string) *pkgInfo {
	p := g.pkgs[importPath]
	if p == nil {
		p = &pkgInfo{importPath: importPath, testDeps: map[string]bool{}, prodDeps: map[string]bool{}, embeds: map[string]bool{}}
		g.pkgs[importPath] = p
	}
	return p
}

func (g *graph) merge(root string, r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var e listEntry
		if err := dec.Decode(&e); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		if e.Standard {
			continue
		}
		if e.Error != nil {
			return fmt.Errorf("package %s: %s", e.ImportPath, e.Error.Err)
		}
		for _, de := range e.DepsErrors {
			return fmt.Errorf("package %s: dependency error: %s", e.ImportPath, de.Err)
		}
		base := basePath(e.ImportPath)
		// The synthesized test main `p.test` links the whole test binary.
		if e.ForTest == "" && strings.HasSuffix(base, ".test") && e.ImportPath == base {
			under := strings.TrimSuffix(base, ".test")
			if !g.inModule(under) {
				continue
			}
			p := g.get(under)
			p.hasTests = true
			for _, d := range e.Deps {
				if d = basePath(d); g.inModule(d) {
					p.testDeps[d] = true
				}
			}
			continue
		}
		if !g.inModule(base) || e.ImportPath != base || e.ForTest != "" {
			// A recompiled-for-test variant or an external test package:
			// the test main's Deps already carry their edges.
			continue
		}
		rel, err := filepath.Rel(root, e.Dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("package %s: directory %s is outside %s", base, e.Dir, root)
		}
		p := g.get(base)
		p.dir = filepath.ToSlash(rel)
		g.byDir[p.dir] = p
		for _, d := range e.Deps {
			if d = basePath(d); g.inModule(d) {
				p.prodDeps[d] = true
			}
		}
		for _, f := range e.EmbedFiles {
			p.embeds[path.Join(p.dir, filepath.ToSlash(f))] = true
		}
	}
}

// check refuses a graph in which a package has edges but no directory: its
// files could never map to it.
func (g *graph) check() error {
	var bad []string
	for ip, p := range g.pkgs {
		if p.dir == "" {
			bad = append(bad, ip)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("go list gave no directory for %s", strings.Join(bad, ", "))
	}
	return nil
}

// testPackages returns the import paths of every package with a test binary,
// sorted.
func (g *graph) testPackages() []string {
	var out []string
	for ip, p := range g.pkgs {
		if p.hasTests {
			out = append(out, ip)
		}
	}
	sort.Strings(out)
	return out
}

// readModulePath reads the module path from root/go.mod.
func readModulePath(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			return f[1], nil
		}
	}
	return "", fmt.Errorf("%s/go.mod declares no module", root)
}
