package mongo

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestMongoGatedPackagesAreInTheCIJob keeps the mongo-conformance CI job
// honest: a suite gated on ITERION_TEST_MONGO_URI that no job runs is a
// test that cannot fail — it skips locally AND in CI, and the required
// check reads green over semantics nobody verified (the boardmongo suite
// sat outside the job exactly this way). The job's own comment asks for
// this grep by hand; this test performs it on every run.
func TestMongoGatedPackagesAreInTheCIJob(t *testing.T) {
	root := "../../.." // pkg/store/mongo → repo root

	// 1. Every package under pkg/ whose tests read the gate variable.
	gated := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(root, "pkg"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if !strings.Contains(string(data), "ITERION_TEST_MONGO_URI") {
			return nil
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		gated["./"+filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk pkg/: %v", err)
	}
	if len(gated) == 0 {
		t.Fatal("no gated packages found — the walk itself is broken (this file is one)")
	}

	// 2. The ./pkg/... patterns of the mongo-conformance job's go test run.
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "tests.yml"))
	if err != nil {
		t.Fatalf("read tests.yml: %v", err)
	}
	patterns := regexp.MustCompile(`\./pkg/[\w/-]+/\.\.\.`).FindAllString(string(wf), -1)
	if len(patterns) == 0 {
		t.Fatal("no ./pkg/.../... patterns found in tests.yml — the job command moved; update this parser")
	}
	covered := func(pkg string) bool {
		for _, p := range patterns {
			prefix := strings.TrimSuffix(p, "...")
			if strings.HasPrefix(pkg+"/", prefix) {
				return true
			}
		}
		return false
	}

	var missing []string
	for pkg := range gated {
		if !covered(pkg) {
			missing = append(missing, pkg)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("packages gated on ITERION_TEST_MONGO_URI but absent from the mongo-conformance job's go test list (their Mongo suites never run anywhere): %v — add them to .github/workflows/tests.yml", missing)
	}
}
