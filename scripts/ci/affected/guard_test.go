package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The proof behind the selector: a test that reads the repository outside
// its own directory is selected only if inputs.yaml says so, so an
// undeclared read is a future false green. These tests hold the
// declarations against the tree.

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	return root
}

func realInputs(t *testing.T, root string) *inputs {
	t.Helper()
	in, err := loadInputs(filepath.Join(root, filepath.FromSlash(inputsFile)))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func realScan(t *testing.T, root string) ([]evidence, []string) {
	t.Helper()
	module, err := readModulePath(root)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := scanTree(root, module)
	if err != nil {
		t.Fatal(err)
	}
	tracked, err := trackedFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return ev, tracked
}

// TestEveryOutOfDirectoryReadIsDeclared is the guard: every read the static
// scan finds outside a package's directory is covered by inputs.yaml.
func TestEveryOutOfDirectoryReadIsDeclared(t *testing.T) {
	root := repoRoot(t)
	in := realInputs(t, root)
	ev, tracked := realScan(t, root)
	if len(ev) < 100 {
		t.Fatalf("the scan found %d reads; the tree holds hundreds — is it still looking?", len(ev))
	}
	for _, u := range undeclared(in, tracked, ev) {
		t.Errorf("undeclared read: %s", u)
	}
	if t.Failed() {
		t.Log("declare each read under `packages:` in " + inputsFile + " (`go run ./scripts/ci/affected -scan` lists them)")
	}
}

// TestTheGuardRefusesEachRemovedDeclaration proves the guard can go red: with
// any one of these real declarations removed, the scan of the real tree
// reports the package. A declaration the scan does not need would pass
// here; these are the sweepers whose loss would hide the most.
func TestTheGuardRefusesEachRemovedDeclaration(t *testing.T) {
	root := repoRoot(t)
	in := realInputs(t, root)
	ev, tracked := realScan(t, root)
	for _, dir := range []string{"internal/mdcode", "internal/ciguard", "pkg/repomap", "pkg/git", "bots", "pkg/server"} {
		if _, ok := in.Packages[dir]; !ok {
			t.Errorf("%s has no declaration to remove", dir)
			continue
		}
		cut := *in
		cut.Packages = map[string]readDecl{}
		for k, v := range in.Packages {
			if k != dir {
				cut.Packages[k] = v
			}
		}
		named := false
		for _, u := range undeclared(&cut, tracked, ev) {
			if strings.HasPrefix(u, dir+": ") {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("removing the declaration of %s left the guard green", dir)
		}
	}
}

// TestEveryDeclarationNamesAPackage refuses a declaration for a directory
// the module does not build: it would select nothing, silently.
func TestEveryDeclarationNamesAPackage(t *testing.T) {
	root := repoRoot(t)
	in := realInputs(t, root)
	dirs, err := packageDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range sortedKeys(in.Packages) {
		if !slices.Contains(dirs, d) {
			t.Errorf("packages: %s is no package directory", d)
		}
		decl := in.Packages[d]
		for _, r := range decl.Reach {
			if !slices.Contains(dirs, r) {
				t.Errorf("packages: %s reaches %s, no package directory", d, r)
			}
		}
	}
	for _, name := range in.legNames() {
		for _, r := range in.Legs[name].Reach {
			if !slices.Contains(dirs, r) {
				t.Errorf("legs: %s reaches %s, no package directory", name, r)
			}
		}
		for _, p := range in.Legs[name].Packages {
			base := strings.TrimSuffix(p, "/...")
			if p == "..." {
				continue
			}
			ok := false
			for _, d := range dirs {
				if d == base || strings.HasPrefix(d, base+"/") {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("legs: %s tests %s, which matches no package directory", name, p)
			}
		}
	}
}

type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readWorkflow(t *testing.T, root string) workflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "tests.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Jobs) == 0 {
		t.Fatal("tests.yml has no jobs")
	}
	return w
}

// TestTheLegsAreTheWorkflowsJobs holds `legs:` total over tests.yml, both
// directions: a new job nobody mapped would never be selected.
func TestTheLegsAreTheWorkflowsJobs(t *testing.T) {
	root := repoRoot(t)
	in := realInputs(t, root)
	w := readWorkflow(t, root)
	jobs := sortedKeys(w.Jobs)
	if got := in.legNames(); !slices.Equal(got, jobs) {
		t.Fatalf("legs %v\n  != tests.yml jobs %v", got, jobs)
	}
}

var goTestPkgArg = regexp.MustCompile(`(?:^|\s)\./(pkg/[A-Za-z0-9_./-]*)`)

// TestTheConformanceLegsTestWhatTheirJobsTest reads the package arguments of
// every `go test` line of the conformance jobs and requires each in the
// leg's packages, so a package added to a job is selected for it.
func TestTheConformanceLegsTestWhatTheirJobsTest(t *testing.T) {
	root := repoRoot(t)
	in := realInputs(t, root)
	w := readWorkflow(t, root)
	for _, job := range []string{"mongo-conformance", "nats-conformance", "test-native-exec"} {
		var args []string
		for _, s := range w.Jobs[job].Steps {
			if !strings.Contains(s.Run, "go test") {
				continue
			}
			for _, m := range goTestPkgArg.FindAllStringSubmatch(s.Run, -1) {
				args = append(args, strings.TrimSuffix(m[1], "/"))
			}
		}
		if len(args) == 0 {
			t.Errorf("%s: no ./pkg/ argument found in its go test lines — has the job changed shape?", job)
		}
		for _, a := range args {
			if !slices.ContainsFunc(in.Legs[job].Packages, func(spec string) bool { return spec == a || dirMatches(spec, a) }) {
				t.Errorf("%s: the job tests ./%s, the leg's packages do not name it", job, a)
			}
		}
	}
}
