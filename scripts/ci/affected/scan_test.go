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
	} {
		g := got[pkg]
		slices.Sort(g)
		if !slices.Equal(g, want) {
			t.Errorf("%s: scan = %v, want %v", pkg, g, want)
		}
	}
	// A traversal fixture, a climb relative to a temp dir, a path naming
	// nothing in the tree, and production code: none is a test's read.
	for _, pkg := range []string{"pkg/d", "pkg/e"} {
		if len(got[pkg]) > 0 {
			t.Errorf("%s: scan = %v, want nothing", pkg, got[pkg])
		}
	}
}

func TestUndeclaredJudgesFilesDirectoriesAndRoots(t *testing.T) {
	ev := scanFixture(t)
	tracked := []string{"docs/adr/1.md", "docs/x.md", "docs/y.md", "docs/z.md", "go.mod"}
	in := &inputs{Packages: map[string]readDecl{
		"pkg/a": {Reads: []string{"docs/x.md"}, Why: "w"},
		"pkg/b": {Reads: []string{"docs/y.md"}, Why: "w"}, // misses docs/adr/
		// pkg/c has no entry: its root read is refused.
	}}
	got := undeclared(in, tracked, ev)
	if len(got) != 3 {
		t.Fatalf("undeclared = %q, want pkg/b's directory and pkg/c's root and file", got)
	}
	in.Packages["pkg/b"] = readDecl{Reads: []string{"docs/**"}, Why: "w"}
	in.Packages["pkg/c"] = readDecl{Reads: []string{"docs/z.md"}, Why: "w"}
	if got := undeclared(in, tracked, ev); len(got) != 0 {
		t.Fatalf("undeclared = %q, want none", got)
	}
}
