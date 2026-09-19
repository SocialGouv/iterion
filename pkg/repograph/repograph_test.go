package repograph_test

import (
	"os"
	"path/filepath"
	"testing"

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
