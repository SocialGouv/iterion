package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func scanFixture(t *testing.T) []evidence {
	t.Helper()
	root := writeTree(t, map[string]string{
		"go.mod":        "module example.com/m\n",
		"docs/x.md":     "x",
		"docs/y.md":     "y",
		"docs/z.md":     "z",
		"docs/adr/1.md": "1",
		"pkg/a/a.go":    "package a\n",
		"pkg/a/a_test.go": `package a
import ("os"; "path/filepath"; "testing")
func TestA(t *testing.T) { os.ReadFile(filepath.Join("..", "..", "docs", "x.md")) }
`,
		"pkg/b/b_test.go": `package b
import ("os"; "testing")
func TestB(t *testing.T) { os.ReadFile("../../docs/y.md"); os.ReadDir("../../docs/adr") }
`,
		"internal/helper/helper.go": `package helper
import "os"
// Root climbs to go.mod.
func Root() string { d, _ := os.Getwd(); _, _ = os.Stat(d + "/go.mod"); return d }
`,
		"pkg/c/c_test.go": `package c
import ("os"; "path/filepath"; "testing"; "example.com/m/internal/helper")
func TestC(t *testing.T) { os.ReadFile(filepath.Join(helper.Root(), "docs", "z.md")) }
`,
		"pkg/d/d_test.go": `package d
import ("os"; "path/filepath"; "strings"; "testing")
func TestD(t *testing.T) {
	_ = strings.Contains("a/../b", "..")
	os.ReadFile(filepath.Join(t.TempDir(), "..", "docs"))
	os.ReadFile("../../etc/passwd")
	os.ReadFile("testdata/own.txt")
}
`,
		"examples/a.bot":         "a",
		"examples/sub/b.bot":     "b",
		"bots/one/manifest.yaml": "m",
		"bots/one/main.bot":      "m",
		"pkg/f/f_test.go": `package f
import ("path/filepath"; "testing")
func TestF(t *testing.T) { filepath.Glob(filepath.Join("..", "..", "examples", "*.bot")) }
`,
		"pkg/g/g_test.go": `package g
import ("path/filepath"; "testing")
func TestG(t *testing.T) { name := "manifest.yaml"; filepath.Glob(filepath.Join("..", "..", "bots", "*", name)) }
`,
		"pkg/h/h_test.go": `package h
import ("path/filepath"; "testing")
func TestH(t *testing.T) { filepath.Glob("../../nowhere/*.bot") }
`,
		"pkg/e/e.go": `package e
import "os"
func Load() { os.ReadFile("../../docs/x.md") }
`,
	})
	ev, err := scanTree(root, "example.com/m")
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestTheScanFindsEachReadShape(t *testing.T) {
	ev := scanFixture(t)
	got := map[string][]string{}
	for _, e := range ev {
		got[e.Pkg] = append(got[e.Pkg], e.Kind+" "+e.Path)
	}
	for pkg, want := range map[string][]string{
		"pkg/a": {"relative docs/x.md"},
		"pkg/b": {"relative docs/adr", "relative docs/y.md"},
		"pkg/c": {"anchored docs/z.md", "root ."},
		"pkg/f": {"relative examples/*.bot"},
		"pkg/g": {"relative bots/*"},
	} {
		g := got[pkg]
		slices.Sort(g)
		if !slices.Equal(g, want) {
			t.Errorf("%s: scan = %v, want %v", pkg, g, want)
		}
	}
	// A traversal fixture, a climb relative to a temp dir, a path naming
	// nothing in the tree, a glob matching nothing, and production code:
	// none is a test's read.
	for _, pkg := range []string{"pkg/d", "pkg/e", "pkg/h"} {
		if len(got[pkg]) > 0 {
			t.Errorf("%s: scan = %v, want nothing", pkg, got[pkg])
		}
	}
}

func TestUndeclaredJudgesFilesDirectoriesAndRoots(t *testing.T) {
	ev := scanFixture(t)
	tracked := []string{"bots/one/main.bot", "bots/one/manifest.yaml", "docs/adr/1.md", "docs/x.md", "docs/y.md", "docs/z.md", "examples/a.bot", "examples/sub/b.bot", "go.mod"}
	in := &inputs{Packages: map[string]readDecl{
		"pkg/a": {Reads: []string{"docs/x.md"}, Why: "w"},
		"pkg/b": {Reads: []string{"docs/y.md"}, Why: "w"}, // misses docs/adr/
		// pkg/c has no entry: its root read is refused.
		"pkg/f": {Reads: []string{"examples/sub/*.bot"}, Why: "w"},   // misses examples/a.bot
		"pkg/g": {Reads: []string{"bots/*/manifest.yaml"}, Why: "w"}, // the computed name reads every bundle file
	}}
	got := undeclared(in, tracked, ev)
	if len(got) != 5 {
		t.Fatalf("undeclared = %q, want pkg/b's directory, pkg/c's root and file, pkg/f's glob and pkg/g's glob directory", got)
	}
	in.Packages["pkg/b"] = readDecl{Reads: []string{"docs/**"}, Why: "w"}
	in.Packages["pkg/c"] = readDecl{Reads: []string{"docs/z.md"}, Why: "w"}
	in.Packages["pkg/f"] = readDecl{Reads: []string{"examples/*.bot"}, Why: "w"}
	in.Packages["pkg/g"] = readDecl{Reads: []string{"bots/**"}, Why: "w"}
	if got := undeclared(in, tracked, ev); len(got) != 0 {
		t.Fatalf("undeclared = %q, want none", got)
	}
}

// TestTheScanSeesEveryRootFinder holds one fixture per way a test finds the
// repository root without a literal climb; each package must come out
// root-anchored, with the file it then reads.
func TestTheScanSeesEveryRootFinder(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":        "module example.com/m\n",
		"docs/x.md":     "x",
		"docs/y.md":     "y",
		"docs/z.md":     "z",
		"docs/adr/1.md": "1",
		// go env GOMOD, in a helper of the test file.
		"pkg/gomod/gomod_test.go": `package gomod
import ("os"; "os/exec"; "path/filepath"; "strings"; "testing")
func repoRoot(t *testing.T) string { out, _ := exec.Command("go", "env", "GOMOD").Output(); return filepath.Dir(strings.TrimSpace(string(out))) }
func TestG(t *testing.T) { os.ReadFile(filepath.Join(repoRoot(t), "docs", "x.md")) }
`,
		// go list -m -f {{.Dir}}, in a production helper another package calls.
		"internal/modroot/modroot.go": `package modroot
import ("os/exec"; "strings")
func Dir() string { out, _ := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output(); return strings.TrimSpace(string(out)) }
`,
		"pkg/listm/listm_test.go": `package listm
import ("os"; "path/filepath"; "testing"; "example.com/m/internal/modroot")
func TestL(t *testing.T) { os.ReadFile(filepath.Join(modroot.Dir(), "docs", "y.md")) }
`,
		// A Test function that anchors itself: a finder nothing calls.
		"pkg/self/self_test.go": `package self
import ("os"; "os/exec"; "path/filepath"; "strings"; "testing")
func TestS(t *testing.T) {
	out, _ := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "docs", "z.md"))
}
`,
		// runtime.Caller under an alias, then a Join on the file's directory.
		"pkg/caller/caller_test.go": `package caller
import ("os"; "path/filepath"; goruntime "runtime"; "testing")
func TestC(t *testing.T) {
	_, src, _, _ := goruntime.Caller(0)
	os.ReadFile(filepath.Join(filepath.Dir(src), "../../docs/adr/1.md"))
	dir := filepath.Dir(src)
	os.ReadFile(filepath.Join(dir, "..", "..", "docs", "x.md"))
}
`,
		// The working directory climbed with filepath.Dir, in a helper the
		// test calls: the package's grandparent is the root.
		"pkg/getwd/getwd_test.go": `package getwd
import ("os"; "path/filepath"; "testing")
func repoRoot() string { wd, err := os.Getwd(); if err != nil { return "." }; return filepath.Dir(filepath.Dir(wd)) }
func TestW(t *testing.T) { os.ReadFile(filepath.Join(repoRoot(), "docs", "x.md")) }
`,
		// The working directory climbed by a ".." Join, through a second name.
		"pkg/getwdjoin/getwdjoin_test.go": `package getwdjoin
import ("os"; "path/filepath"; "testing")
func TestJ(t *testing.T) {
	cwd, _ := os.Getwd()
	here := cwd
	_ = here
	os.ReadFile(filepath.Join(cwd, "..", "..", "docs", "y.md"))
}
`,
		// The working directory used in place reads the package's own files.
		"pkg/getwdhere/getwdhere_test.go": `package getwdhere
import ("os"; "path/filepath"; "testing")
func TestH(t *testing.T) { wd, _ := os.Getwd(); os.ReadFile(filepath.Join(wd, "testdata", "x")); _ = filepath.Dir(t.TempDir()) }
`,
		// A go.mod a test writes into a fixture finds nothing.
		"pkg/fixture/fixture_test.go": `package fixture
import ("os"; "path/filepath"; "testing")
func TestF(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), nil, 0o644)
	_ = map[string]string{"go.mod": "module x"}
}
`,
	})
	ev, err := scanTree(root, "example.com/m")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, e := range ev {
		got[e.Pkg] = append(got[e.Pkg], e.Kind+" "+e.Path)
	}
	for pkg, want := range map[string][]string{
		"pkg/gomod":     {"anchored docs/x.md", "root ."},
		"pkg/listm":     {"anchored docs/y.md", "root ."},
		"pkg/self":      {"anchored docs/z.md", "root ."},
		"pkg/caller":    {"relative docs/adr/1.md", "relative docs/x.md", "root ."},
		"pkg/getwd":     {"anchored docs/x.md", "root ."},
		"pkg/getwdjoin": {"root ."},
	} {
		g := got[pkg]
		slices.Sort(g)
		if !slices.Equal(g, want) {
			t.Errorf("%s: scan = %v, want %v", pkg, g, want)
		}
	}
	if len(got["pkg/getwdhere"]) > 0 {
		t.Errorf("pkg/getwdhere: scan = %v, want nothing (the working directory itself is the package's)", got["pkg/getwdhere"])
	}
	if len(got["pkg/fixture"]) > 0 {
		t.Errorf("pkg/fixture: scan = %v, want nothing (a go.mod written into a temp dir finds no root)", got["pkg/fixture"])
	}
}
