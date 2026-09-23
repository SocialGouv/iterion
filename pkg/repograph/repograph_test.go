package repograph_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/internal/mdcode"
	"github.com/SocialGouv/iterion/pkg/repograph"
)

// fixture writes a tiny module: two packages, one calling the other and
// one naming its interface without ever calling it.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/demo\n\ngo 1.26\n")
	write("pkg/seam/seam.go", `package seam

// Store is the seam every adapter implements.
type Store interface{ Put(k string) error }

// Helper is called, not merely named.
func Helper() int { return 1 }
`)
	write("pkg/user/user.go", `package user

import "example.test/demo/pkg/seam"

// Adapter holds the seam by TYPE — it never calls it.
type Adapter struct{ S seam.Store }

// Run calls the helper.
func Run() int { return seam.Helper() }
`)
	write("docs/a.md", "# A\n\nSee [B](b.md) and [the web](https://example.test).\n")
	write("docs/b.md", "# B\n\nBack to [A](a.md).\n")
	return root
}

func build(t *testing.T, root string) *repograph.Graph {
	t.Helper()
	g, err := repograph.Build(root)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return g
}

func hasEdge(g *repograph.Graph, from, to string, rel repograph.Rel) bool {
	for _, e := range g.Out(from) {
		if e.To == to && e.Rel == rel {
			return true
		}
	}
	return false
}

// A seam is NAMED, never called. Collapsing references into calls — or
// dropping them — makes the graph answer "nothing depends on this" about
// the declarations the architecture hangs from, which is the whole reason
// the relation exists.
func TestAReferencedInterfaceIsNotACall(t *testing.T) {
	g := build(t, fixture(t))

	if !hasEdge(g, "sym:pkg/user.Adapter", "sym:pkg/seam.Store", repograph.RelReferences) {
		t.Error("holding a seam as a field produced no `references` edge")
	}
	if hasEdge(g, "sym:pkg/user.Adapter", "sym:pkg/seam.Store", repograph.RelCalls) {
		t.Error("holding a seam as a field was recorded as a call")
	}
	if !hasEdge(g, "sym:pkg/user.Run", "sym:pkg/seam.Helper", repograph.RelCalls) {
		t.Error("an actual call produced no `calls` edge")
	}
}

// An import inside the module is an edge; an import of anything else is
// not a node this graph invents.
func TestOnlyModuleImportsBecomeEdges(t *testing.T) {
	g := build(t, fixture(t))
	if !hasEdge(g, "pkg:pkg/user", "pkg:pkg/seam", repograph.RelImports) {
		t.Fatal("a module-internal import produced no edge")
	}
	for id := range g.Nodes {
		if id == "pkg:errors" || id == "pkg:fmt" {
			t.Fatalf("the standard library leaked into the graph as %q", id)
		}
	}
}

// A link between two pages is an edge; an external URL and a missing
// target are not. The graph records what exists — broken links are
// #1233's subject, and inventing nodes for them would corrupt every
// "is there a path" answer.
func TestDocLinksOnlyConnectPagesThatExist(t *testing.T) {
	g := build(t, fixture(t))
	if !hasEdge(g, "doc:docs/a.md", "doc:docs/b.md", repograph.RelLinks) {
		t.Error("a relative link between two pages produced no edge")
	}
	for _, e := range g.Out("doc:docs/a.md") {
		if _, known := g.Nodes[e.To]; !known {
			t.Errorf("a link produced an edge to a node that does not exist: %s", e.To)
		}
	}
}

// Path returns a shortest path, and nothing when there is none in the
// direction asked. A graph that answered with an undirected path would
// claim `seam` depends on `user`.
func TestPathIsDirectedAndShortest(t *testing.T) {
	g := build(t, fixture(t))
	hops := g.Path("pkg:pkg/user", "sym:pkg/seam.Helper")
	if len(hops) == 0 {
		t.Fatal("no path from the importing package to the symbol it calls")
	}
	if hops[0] != "pkg:pkg/user" || hops[len(hops)-1] != "sym:pkg/seam.Helper" {
		t.Fatalf("path does not run end to end: %v", hops)
	}
	if back := g.Path("sym:pkg/seam.Helper", "pkg:pkg/user"); len(back) != 0 {
		t.Fatalf("a directed path was found backwards: %v", back)
	}
}

// Impact is bounded on purpose: the transitive closure of a common
// symbol is most of the repository, and "everything" is not actionable.
func TestImpactRespectsItsDepthBound(t *testing.T) {
	g := build(t, fixture(t))
	near := g.Impacted("sym:pkg/seam.Helper", 1)
	far := g.Impacted("sym:pkg/seam.Helper", 4)
	if len(near) == 0 {
		t.Fatal("nothing reaches a symbol that is called")
	}
	if len(far) < len(near) {
		t.Fatalf("a deeper walk returned fewer nodes: %d < %d", len(far), len(near))
	}
	for _, n := range near {
		if n.ID == "sym:pkg/seam.Helper" {
			t.Fatal("impact included the node itself")
		}
	}
}

// Two builds of the same tree must produce the same bytes, or the cache
// key means nothing and every diff is noise.
func TestBuildIsDeterministic(t *testing.T) {
	root := fixture(t)
	a, b := build(t, root), build(t, root)
	if len(a.Edges) != len(b.Edges) {
		t.Fatalf("edge count differs between builds: %d vs %d", len(a.Edges), len(b.Edges))
	}
	for i := range a.Edges {
		if a.Edges[i] != b.Edges[i] {
			t.Fatalf("edge %d differs between builds: %+v vs %+v", i, a.Edges[i], b.Edges[i])
		}
	}
}

// Rank must not hand back the nodes it was seeded with — "what else
// matters, given where I am" excludes where you are — and must order the
// same way every run.
func TestRankExcludesItsSeedsAndIsStable(t *testing.T) {
	g := build(t, fixture(t))
	seeds := []string{"sym:pkg/seam.Store"}
	first := g.Rank(seeds, 10)
	if len(first) == 0 {
		t.Fatal("ranking returned nothing")
	}
	for _, r := range first {
		if r.Node.ID == seeds[0] {
			t.Fatal("the seed came back in its own ranking")
		}
	}
	second := g.Rank(seeds, 10)
	for i := range first {
		if first[i].Node.ID != second[i].Node.ID {
			t.Fatalf("ranking is unstable at %d: %s vs %s", i, first[i].Node.ID, second[i].Node.ID)
		}
	}
}

// The cache must be keyed on the tree, not on time: an unchanged tree
// reuses it, and a changed file rebuilds. A cache that missed the second
// case would describe a repository that no longer exists — the single
// failure an index cannot have.
func TestCacheRebuildsWhenTheTreeChanges(t *testing.T) {
	root := fixture(t)
	if _, rebuilt, err := repograph.Load(root); err != nil || !rebuilt {
		t.Fatalf("first Load: rebuilt=%v err=%v — want a build", rebuilt, err)
	}
	if _, rebuilt, err := repograph.Load(root); err != nil || rebuilt {
		t.Fatalf("second Load: rebuilt=%v err=%v — want the cache", rebuilt, err)
	}

	extra := filepath.Join(root, "pkg", "seam", "more.go")
	if err := os.WriteFile(extra, []byte("package seam\n\n// Added is new.\nfunc Added() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g, rebuilt, err := repograph.Load(root)
	if err != nil || !rebuilt {
		t.Fatalf("after a new file: rebuilt=%v err=%v — want a rebuild", rebuilt, err)
	}
	if _, ok := g.Nodes["sym:pkg/seam.Added"]; !ok {
		t.Fatal("the rebuilt graph does not contain the symbol that was added")
	}
}

// The fingerprint is what the cache trusts. If it ignored a file's
// content changing in place, every test above would still pass and the
// cache would still be wrong.
func TestFingerprintMovesWhenAFileChanges(t *testing.T) {
	root := fixture(t)
	before, err := repograph.Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "pkg", "seam", "seam.go")
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(body, []byte("\n// trailing change\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := repograph.Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("editing a file left the fingerprint unchanged")
	}
}

// writeTree is fixture's general form: it builds a tree from a map of
// relative paths to bodies.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A trailing comment is not part of a replace target. Left in, it named
// a directory that does not exist: 703 edges gone, exit 0, no warning,
// and `map impact` answering "nothing reaches this package" about a
// package eleven nodes reach.
func TestACommentOnAReplaceLineDoesNotLoseTheModule(t *testing.T) {
	for _, tc := range []struct{ name, replace string }{
		{"bare", "replace example.test/dep => ./third_party/dep\n"},
		{"trailing comment", "replace example.test/dep => ./third_party/dep // pinned\n"},
		{"comment with no space", "replace example.test/dep => ./third_party/dep// pinned\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"go.mod":                 "module example.test/demo\n\ngo 1.26\n\n" + tc.replace,
				"third_party/dep/dep.go": "package dep\n\n// Helper is reachable only through the replace.\nfunc Helper() int { return 1 }\n",
				"pkg/user/user.go": `package user

import "example.test/dep"

// Run calls across the replace.
func Run() int { return dep.Helper() }
`,
			})
			g := build(t, root)
			if _, ok := g.Nodes["sym:third_party/dep.Helper"]; !ok {
				t.Fatal("the replaced module's symbol is absent — the target did not resolve")
			}
			if !hasEdge(g, "pkg:pkg/user", "pkg:third_party/dep", repograph.RelImports) {
				t.Fatal("no import edge onto the replaced module")
			}
		})
	}
}

// Two replaces sharing a prefix were resolved by ranging over a Go map,
// so which one matched depended on iteration order: eight cold builds of
// one unchanged tree produced eight different graphs, and the cache
// froze whichever won. Longest prefix first is what `go` itself does,
// and this asserts BOTH that the builds agree and that the right one
// wins — agreement alone would pass on a consistently wrong answer.
func TestOverlappingReplacesResolveTheSameWayEveryBuild(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.test/demo\n\ngo 1.26\n\n" +
			"replace example.test/dep => ./third_party/outer\n" +
			"replace example.test/dep/inner => ./third_party/inner\n",
		"third_party/outer/inner/x/x.go": "package x\n\n// Outer must lose.\nfunc Outer() int { return 1 }\n",
		"third_party/inner/x/x.go":       "package x\n\n// Inner must win.\nfunc Inner() int { return 2 }\n",
		"pkg/user/user.go": `package user

import "example.test/dep/inner/x"

// Run reaches through the more specific replace.
func Run() int { return x.Inner() }
`,
	}
	root := writeTree(t, files)
	first := build(t, root)

	if !hasEdge(first, "pkg:pkg/user", "pkg:third_party/inner/x", repograph.RelImports) {
		t.Fatal("the longest matching prefix did not win: the import did not land on third_party/inner/x")
	}
	for i := 2; i <= 9; i++ {
		g := build(t, root)
		if len(g.Edges) != len(first.Edges) {
			t.Fatalf("build %d has %d edges, build 1 had %d — the resolution depends on map order",
				i, len(g.Edges), len(first.Edges))
		}
		for j := range g.Edges {
			if g.Edges[j] != first.Edges[j] {
				t.Fatalf("build %d, edge %d: %+v, build 1 had %+v", i, j, g.Edges[j], first.Edges[j])
			}
		}
	}
}

// linkDocs is the one site that reaches the filesystem by `os.Stat`
// instead of by a guarded walk, so it minted doc nodes for files the
// fingerprint never hashes. Deleting one then left the cache serving a
// graph of a tree that no longer existed — the single failure an index
// cannot have, and the same one a previous round closed at a sibling.
func TestALinkIntoASkippedTreeMintsNoNode(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":                 "module example.test/demo\n\ngo 1.26\n",
		"docs/a.md":              "# A\n\nSee [probe](testdata/probe.md) and [vendored](../vendor/dep/README.md).\n",
		"docs/testdata/probe.md": "# Probe\n\nA fixture, not a page.\n",
		"vendor/dep/README.md":   "# Vendored\n\nNot this repository.\n",
	})
	g := build(t, root)
	for id, n := range g.Nodes {
		for _, skipped := range []string{"testdata/", "vendor/"} {
			if strings.Contains(n.Path, skipped) {
				t.Errorf("node %s (%s) was minted inside a skipped tree the fingerprint never hashes", id, n.Path)
			}
		}
	}
}

// A doc comment is cut at a BYTE bound. Cutting inside a multi-byte rune
// writes invalid UTF-8 into a node's Doc, json.Marshal rewrites those
// bytes to U+FFFD, and a freshly built graph stops agreeing with the
// cached one about an unchanged tree. The assertion is the property, so
// it holds for a rune the fixture never thought of.
func TestNoDocCommentIsCutMidRune(t *testing.T) {
	var src strings.Builder
	src.WriteString("package edge\n")
	// Walk the ellipsis across every byte bound this package cuts at
	// (100 and 120), so at least one symbol has a multi-byte rune
	// straddling one of them. A fixture that misses the bound produces a
	// test that cannot redden, which is worse than no test.
	for pad := 88; pad <= 145; pad++ {
		fmt.Fprintf(&src, "\n// Sym%d %s… and the sentence keeps going so the cut is taken\nfunc Sym%d() {}\n",
			pad, strings.Repeat("a", pad), pad)
	}
	root := writeTree(t, map[string]string{
		"go.mod":           "module example.test/demo\n\ngo 1.26\n",
		"pkg/edge/edge.go": src.String(),
	})
	g := build(t, root)
	checked := 0
	for id, n := range g.Nodes {
		if n.Doc == "" {
			continue
		}
		checked++
		if !utf8.ValidString(n.Doc) {
			t.Errorf("node %s carries invalid UTF-8 in its Doc: %q", id, n.Doc)
		}
	}
	if checked == 0 {
		t.Fatal("no node carried a doc comment — the fixture proves nothing")
	}
}

// The fingerprint hashes (path, size, mtime) for speed, which is blind
// to a restore that preserves both — `cp -p`, `rsync -t`, `tar -x`,
// `touch -r`. On go.mod that blindness costs the whole replaced module:
// 703 edges in this repository hang off its replace lines, so this one
// file is hashed by CONTENT.
func TestGoModContentMovesTheFingerprintWithItsMtimeRestored(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":                 "module example.test/demo\n\ngo 1.26\n\nreplace example.test/dep => ./third_party/dep\n",
		"third_party/dep/dep.go": "package dep\n\n// Helper is reachable only through the replace.\nfunc Helper() int { return 1 }\n",
	})
	mod := filepath.Join(root, "go.mod")
	info, err := os.Stat(mod)
	if err != nil {
		t.Fatal(err)
	}
	before, err := repograph.Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	// Same byte length, so a stat sees nothing move.
	body, err := os.ReadFile(mod)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body), "./third_party/dep", "./third_party/deX", 1)
	if len(edited) != len(body) {
		t.Fatalf("the edit changed the file length (%d vs %d) — it would move the fingerprint for the wrong reason", len(edited), len(body))
	}
	if err := os.WriteFile(mod, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(mod, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := repograph.Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("go.mod changed under a restored mtime and the fingerprint did not move — the cache would serve a graph missing the whole replaced module")
	}
}

// The fingerprint must name every file type the BUILDER opens, not the
// obvious ones: a bundle's manifest is read through the workflow
// compiler, and a file read but not hashed is a cache that reports
// "current" for a tree that changed.
func TestABundleManifestMovesTheFingerprint(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":                  "module example.test/demo\n\ngo 1.26\n",
		"bots/demo/manifest.yaml": "name: demo\nversion: 0.1.0\n",
		"bots/demo/main.bot":      "workflow demo {\n}\n",
	})
	before, err := repograph.Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bots/demo/manifest.yaml"),
		[]byte("name: demo\nversion: 0.2.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := repograph.Fingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("a bundle manifest changed and the fingerprint did not move — the builder reads it, so the cache must see it")
	}
}

// A link a page QUOTES is not a link the page makes. A documentation
// repository quotes link forms exactly when it documents links — in a fenced
// example, in a diagnostic's text, in a code span — and an edge minted from
// one makes `map path` answer that a route exists because a page printed it,
// which is the corruption linkDocs's own doc comment refuses for a broken
// target and used to allow for a quoted one.
//
// The mutation that reddens this: scan the raw body instead of the mask.
func TestALinkQuotedInsideCodeMintsNoEdge(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod": "module example.test/m\n\ngo 1.26\n",
		"docs/a.md": "# A\n\nA real link to [b](b.md).\n\n" +
			"An inline example: `[c](c.md)` is the form.\n\n" +
			"```\n[d](d.md)\n```\n",
		"docs/b.md": "# B\n",
		"docs/c.md": "# C\n",
		"docs/d.md": "# D\n",
	})
	g := build(t, root)

	if !hasEdge(g, "doc:docs/a.md", "doc:docs/b.md", repograph.RelLinks) {
		t.Error("the page's real link produced no edge — the mask hid a link instead of code")
	}
	for _, quoted := range []struct{ node, how string }{
		{"doc:docs/c.md", "a code span"},
		{"doc:docs/d.md", "a fenced block"},
	} {
		if hasEdge(g, "doc:docs/a.md", quoted.node, repograph.RelLinks) {
			t.Errorf("%s quoted in %s became an edge — the page quotes the form, it does not link the file", quoted.node, quoted.how)
		}
	}
}

// A doc comment longer than the bound is truncated for a symbol's Node.Doc,
// and the cut can land inside a code span. The stray backtick that leaves is
// what every reader of the node shows for the rest of the line.
//
// The mutation that reddens this: drop CloseDanglingSpan from this package's
// firstSentence.
func TestATruncatedDocCommentDoesNotEndInsideACodeSpan(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod": "module example.test/m\n\ngo 1.26\n",
		"pkg/long/long.go": "package long\n\n" +
			"// Thing has a doc comment that runs well past the bound, with a code span " +
			"carrying spaces in it such as `the form ](../../x.md) and its neighbour ](../y.md)` " +
			"which the cut lands inside.\nfunc Thing() {}\n",
	})
	g := build(t, root)

	var doc string
	for _, n := range g.Nodes {
		if n.Kind == repograph.KindSymbol && n.Label == "Thing" {
			doc = n.Doc
		}
	}
	if doc == "" {
		t.Fatal("no Doc on the symbol node — this test would prove nothing")
	}
	if !strings.Contains(doc, "…") {
		t.Fatalf("the doc comment was not truncated, so the cut is not exercised: %q", doc)
	}
	spans := mdcode.Spans(doc)
	for i := 0; i < len(doc); i++ {
		if doc[i] != '`' {
			continue
		}
		inside := false
		for _, r := range spans {
			if i >= r[0] && i < r[1] {
				inside = true
				break
			}
		}
		if !inside {
			t.Fatalf("Node.Doc carries a backtick outside any code span at byte %d: %q", i, doc)
		}
	}
}
