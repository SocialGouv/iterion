package layerguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot climbs from the test's working directory to the go.mod — the
// guard must run `go list` from the module root, wherever the test runs from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test — run from inside the module")
		}
		dir = parent
	}
}

// The layer map, as an executable guard. Each rule forbids a package family
// from importing another — the map codifies what the tree already does
// (verified 2026-10-23 over 903 internal import pairs: zero violations), so
// the guard bites on the FIRST new violation, not after a refactor.
//
// The ratchet: a violation that review accepts goes into allowExceptions
// with its reason, and expectedAllowCount moves with it — both are edits a
// reviewer sees. The list can only shrink without a review.
var layers = []struct {
	name    string
	sources []string // package path prefixes the rule binds
	banned  []string // package path prefixes they must not import
}{
	{"pkg/dsl/ir is a leaf — no executor, server, dispatcher, CLI, backend, forge or runner imports", []string{"pkg/dsl/ir"}, []string{"pkg/runtime", "pkg/server", "pkg/dispatcher", "pkg/cli", "pkg/backend", "pkg/forge", "pkg/runner"}},
	{"pkg/dsl compiles without an executor behind it", []string{"pkg/dsl"}, []string{"pkg/runtime", "pkg/server", "pkg/dispatcher", "pkg/cli", "pkg/backend"}},
	{"no pkg package imports the composition root — the server is reached through cmd/ and the seams", []string{"pkg"}, []string{"pkg/server"}},
	{"pkg/runtime never imports the server, the dispatcher or the CLI", []string{"pkg/runtime"}, []string{"pkg/server", "pkg/dispatcher", "pkg/cli"}},
	{"the backends never import the server, the dispatcher or the CLI", []string{"pkg/backend"}, []string{"pkg/server", "pkg/dispatcher", "pkg/cli"}},
	{"the dispatcher never imports the server or the CLI", []string{"pkg/dispatcher"}, []string{"pkg/server", "pkg/cli"}},
	{"internal never imports the composition root", []string{"internal"}, []string{"pkg/server"}},
	{"the store stays a leaf — no executor, server, dispatcher, backend or CLI imports", []string{"pkg/store"}, []string{"pkg/server", "pkg/runtime", "pkg/dispatcher", "pkg/backend", "pkg/cli"}},
}

// allowExceptions maps "source -> banned" violations that review has
// accepted, each with the reason that keeps it honest. EMPTY by design —
// expectedAllowCount pins the count so an addition is an edit of this file,
// visible in review, never a silent rot.
var allowExceptions = map[string]string{}

const expectedAllowCount = 0

func importsPairs(t *testing.T) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{join .Imports \" \"}}", "./pkg/...", "./internal/...")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list failed: %v\n%s", err, out)
	}
	pairs := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "|") {
			continue
		}
		pkg, imps, _ := strings.Cut(line, "|")
		pairs[pkg] = strings.Fields(imps)
	}
	return pairs
}

func TestThePackageLayersHold(t *testing.T) {
	pairs := importsPairs(t)
	module := "github.com/SocialGouv/iterion/"
	violations := []string{}
	for _, l := range layers {
		for pkg, imps := range pairs {
			bound := false
			for _, s := range l.sources {
				if pkg == s || strings.HasPrefix(pkg, s+"/") {
					bound = true
					break
				}
			}
			if !bound {
				continue
			}
			for _, imp := range imps {
				imp = strings.TrimPrefix(imp, module)
				for _, b := range l.banned {
					if imp == b || strings.HasPrefix(imp, b+"/") {
						key := pkg + " -> " + imp
						if _, ok := allowExceptions[key]; !ok {
							violations = append(violations, l.name+": "+key)
						}
					}
				}
			}
		}
	}
	if len(allowExceptions) != expectedAllowCount {
		t.Errorf("allowExceptions holds %d entries, want the pinned %d — an exception leaves only by shrinking this list", len(allowExceptions), expectedAllowCount)
	}
	if len(violations) != 0 {
		t.Errorf("the layer map is violated %d time(s):\n%s", len(violations), strings.Join(violations, "\n"))
	}
}
