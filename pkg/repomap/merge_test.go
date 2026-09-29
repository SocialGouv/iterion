package repomap_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/repomap"
)

// The MERGEABLE ACROSS ENTRIES property of the package doc, checked the way
// the merge queue meets it: two branches that change different entries each
// regenerate the maps, git merges the two versions three ways, and the queue
// then asks the freshness gate about the result. A clean merge must BE the
// map generated for both branches together — a clean merge that differs is a
// stale tree nobody sees until the queue fails it. A conflict is allowed where
// the two branches touch neighbouring rows: it surfaces at the merge, not
// twenty minutes into CI. And entries that land in different places must
// merge clean, or the property holds only because nothing ever merges.
//
// Every kind of entry is added on BOTH branches by some case: a total of a
// kind (pages, ADRs, packages, interfaces, bundles, skills) moves the same
// way on both sides only when both add one, and that is the case that merges
// clean and stale. Two branches changing sources of the SAME entry are out of
// this property's reach (see the package doc).
func TestTwoBranchesMergeIntoTheRegeneratedMaps(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b func(t *testing.T, root string)
		// clean: the two branches touch rows far apart, so git must merge
		// them without a conflict. Otherwise a conflict is acceptable.
		clean bool
	}{
		{"packages added at both ends", addPackage("pkg/aaa"), addPackage("pkg/zzz"), true},
		{"interfaces added at both ends", addInterfacePackage("pkg/aaa"), addInterfacePackage("pkg/zzz"), true},
		{"pages added at both ends", addPage("docs/aaa.md"), addPage("docs/zzz.md"), true},
		{"ADRs added at both ends", addPage("docs/adr/000-first.md"), addPage("docs/adr/999-last.md"), true},
		{"bundles added at both ends", addBundle("aaa"), addBundle("zzz"), true},
		{"skills added at both ends of one bundle", addSkill("demo", "aaa"), addSkill("demo", "zzz"), true},
		{"files added to the same package", addFile("pkg/alpha/one.go"), addFile("pkg/alpha/two.go"), true},
		{"a skill beside a new bundle", addSkill("demo", "two"), addBundle("zeta"), true},
		{"a page removed while another is added", removePath("docs/guide.md"), addPage("docs/aaa.md"), true},
		{"packages added in the same gap", addPackage("pkg/alpha2"), addPackage("pkg/alpha3"), false},
		{"neighbouring rows edited", addInterface("pkg/beta/beta.go"), rewordPackage("pkg/alpha/alpha.go"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := generated(t, nil, nil)
			ours := generated(t, tc.a, nil)
			theirs := generated(t, nil, tc.b)
			both := generated(t, tc.a, tc.b)

			paths := make([]string, 0, len(both))
			for p := range both {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			for _, p := range paths {
				merged, conflicts := mergeFile(t, ours[p], base[p], theirs[p])
				if conflicts > 0 {
					if tc.clean {
						t.Errorf("%s: the two branches touch rows far apart, yet git reports %d conflict(s) — a line of this map depends on more than its own entry:\n%s",
							p, conflicts, merged)
					}
					continue
				}
				if merged != both[p] {
					t.Errorf("%s: git merged the two branches' maps CLEAN into a map that is not the one both branches together generate — the queue would fail this merge with a stale map.\nmerged:\n%s\nregenerated:\n%s",
						p, merged, both[p])
				}
			}
		})
	}
}

// generated builds the synthetic tree, applies the given branch changes and
// returns the maps it renders.
func generated(t *testing.T, a, b func(*testing.T, string)) map[string]string {
	t.Helper()
	root := syntheticTree(t)
	for _, change := range []func(*testing.T, string){a, b} {
		if change != nil {
			change(t, root)
		}
	}
	maps, err := repomap.Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	return maps
}

// mergeFile runs git's own three-way file merge — the algorithm a pull
// request's merge goes through — and returns the result and its conflict
// count (git merge-file's exit status).
func mergeFile(t *testing.T, ours, base, theirs string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"ours": ours, "base": base, "theirs": theirs}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := gittest.Cmd(dir, "merge-file", "-p", "ours", "base", "theirs")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() < 128:
		return string(out), exit.ExitCode()
	}
	t.Fatalf("git merge-file: %v\n%s", err, stderr.String())
	return "", 0
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func addPackage(dir string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		name := filepath.Base(dir)
		write(t, root, filepath.Join(dir, name+".go"),
			"// Package "+name+" does its own thing.\npackage "+name+"\n")
	}
}

func addInterfacePackage(dir string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		name := filepath.Base(dir)
		write(t, root, filepath.Join(dir, name+".go"),
			"// Package "+name+" does its own thing.\npackage "+name+"\n\n// Seam is a seam.\ntype Seam interface{ Do() }\n")
	}
}

// addFile adds a file that carries no package doc and no interface: it
// changes nothing a row renders.
func addFile(rel string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		fn := "Helper" + strings.TrimSuffix(filepath.Base(rel), ".go")
		write(t, root, rel, "package "+filepath.Base(filepath.Dir(rel))+"\n\n// "+fn+" helps.\nfunc "+fn+"() {}\n")
	}
}

func addInterface(rel string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		write(t, root, rel, "// Package beta does the second thing.\npackage beta\n\n// Run runs.\nfunc Run() {}\n\n// Runner is a seam.\ntype Runner interface{ Run() }\n")
	}
}

func rewordPackage(rel string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		write(t, root, rel, "// Package alpha does the first thing, differently.\npackage alpha\n\n// Doer is a seam.\ntype Doer interface{ Do() }\n")
	}
}

func addPage(rel string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		write(t, root, rel, "# "+filepath.Base(rel)+"\n\nIt says one thing.\n")
	}
}

func addSkill(bot, skill string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		write(t, root, filepath.Join("bots", bot, "skills", skill+".md"),
			"---\nname: "+bot+"-"+skill+"\ndescription: The "+skill+" skill.\n---\n\nBody.\n")
	}
}

func addBundle(bot string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		write(t, root, filepath.Join("bots", bot, "manifest.yaml"),
			"name: "+bot+"\ndisplay_name: "+bot+"\nicon: \"🤖\"\nversion: 1.0.0\ndescription: Another bundle.\n")
		write(t, root, filepath.Join("bots", bot, "main.bot"), "workflow main:\n  entry: done\n")
	}
}

func removePath(rel string) func(*testing.T, string) {
	return func(t *testing.T, root string) {
		if err := os.Remove(filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
}
