package treeskip_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/treeskip"
	"github.com/SocialGouv/iterion/pkg/repograph"
	"github.com/SocialGouv/iterion/pkg/repomap"
)

// candidates are the directory names the fixture plants a canary in. It is
// deliberately NOT derived from treeskip: a fixture that follows the set
// under test cannot redden when the set changes — the first version of this
// guard planted its canaries from treeskip.Names(), so deleting an entry
// deleted the canary that would have caught the deletion, and the test
// stayed green.
//
// `keepme` is the control: a name nothing skips, whose canary BOTH artifacts
// must describe. Without it every assertion below would hold over two empty
// artifacts.
var candidates = []string{
	"vendor", "node_modules", ".git", "testdata", "dist", ".vitepress", "keepme",
}

// floor is the subset of candidates every generated artifact must skip
// whatever else the set holds. A floor is not a second copy of the set: it
// is shorter on purpose and says "at minimum these", so adding a tree to
// treeskip needs no edit here while removing one of these reddens.
var floor = map[string]bool{
	"vendor": true, "node_modules": true, ".git": true,
	"testdata": true, "dist": true, ".vitepress": true,
}

// The two generated artifacts of this repository — the committed maps and
// the graph — must skip the SAME trees, and this asserts it by RUNNING both
// of them over a tree that carries a canary in each candidate directory,
// never by comparing two lists.
//
// A list comparison is what the divergence survived: the two sets were
// byte-identical copies, one gained `.vitepress`, and the only thing that
// would have noticed was a reader of both files.
func TestTheMapsAndTheGraphSkipTheSameTrees(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A tree both generators accept: a module, a docs/ root, a bots/ dir.
	write("go.mod", "module example.com/fixture\n\ngo 1.26\n")
	write("docs/page.md", "# A page\n\nThe one page both artifacts always describe.\n")
	if err := os.MkdirAll(filepath.Join(root, "bots"), 0o755); err != nil {
		t.Fatal(err)
	}
	// One canary per candidate, on every surface either generator reads: a
	// documentation page under docs/, and a Go package at the repository root.
	for _, name := range candidates {
		write("docs/"+name+"/canary.md", "# Canary "+name+"\n\nA page inside "+name+".\n")
		write(name+"/canary/canary.go", "// Package canary sits inside "+name+".\npackage canary\n")
	}

	maps, err := repomap.Generate(root)
	if err != nil {
		t.Fatalf("generate the maps over the fixture: %v", err)
	}
	if len(maps) == 0 {
		t.Fatal("no map generated — this test would prove nothing")
	}
	graph, err := repograph.Build(root)
	if err != nil {
		t.Fatalf("build the graph over the fixture: %v", err)
	}

	mapped := map[string]bool{}
	for _, body := range maps {
		for _, name := range candidates {
			if strings.Contains(body, name+"/canary") {
				mapped[name] = true
			}
		}
	}
	graphed := map[string]bool{}
	for _, n := range graph.Nodes {
		for _, name := range candidates {
			if strings.Contains(n.Path, name+"/canary") {
				graphed[name] = true
			}
		}
	}

	for _, name := range candidates {
		if mapped[name] != graphed[name] {
			t.Errorf("%s/: the maps %s it and the graph %s it — the two artifacts no longer read one skip set",
				name, describe(mapped[name]), describe(graphed[name]))
		}
		if floor[name] {
			if mapped[name] {
				t.Errorf("%s/ is in the floor every artifact must skip, and the maps describe it", name)
			}
			if graphed[name] {
				t.Errorf("%s/ is in the floor every artifact must skip, and the graph describes it", name)
			}
			if !treeskip.Dir(name) {
				t.Errorf("treeskip.Dir(%q) is false, and this repository's generators must skip it", name)
			}
		}
	}
	// The control: without it, "skipped by both" would hold over two empty
	// artifacts and this guard would pass a generator that indexes nothing.
	if !mapped["keepme"] || !graphed["keepme"] {
		t.Errorf("the control tree keepme/ is described by maps=%v graph=%v — the fixture, not the skip set, decided this run",
			mapped["keepme"], graphed["keepme"])
	}
	if names := treeskip.Names(); len(names) < len(floor) {
		t.Errorf("treeskip names %v, fewer entries than the floor this guard plants", names)
	}
}

func describe(indexed bool) string {
	if indexed {
		return "DESCRIBES"
	}
	return "skips"
}

func TestNamesReportsTheSetTheWalksAsk(t *testing.T) {
	names := treeskip.Names()
	if !sort.StringsAreSorted(names) {
		t.Errorf("Names is not sorted: %v", names)
	}
	for _, name := range names {
		if !treeskip.Dir(name) {
			t.Errorf("Names reports %q, which Dir does not skip", name)
		}
		if !treeskip.Path("a/" + name + "/b") {
			t.Errorf("Path does not skip a path through %q, which Dir skips", name)
		}
	}
	if treeskip.Path("pkg/repomap/repomap.go") {
		t.Error("Path skips a path with no skipped segment in it")
	}
	// A skipped NAME is a whole path segment, never a substring of one.
	if treeskip.Path("pkg/vendored/x.go") {
		t.Error(`Path treats the segment "vendored" as the skipped tree "vendor"`)
	}
}
