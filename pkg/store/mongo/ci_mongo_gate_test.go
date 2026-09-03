package mongo

import (
	"fmt"
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
		// An actual env READ, not prose. A naive substring match counted
		// pkg/runner, which only NAMES the variable in a comment ("mirror
		// of ITERION_TEST_MONGO_URI in pkg/store/mongo") — a false
		// positive that happened to be masked by the job-slicing bug
		// below, and would have turned this guard red the moment that bug
		// was fixed alone.
		if !gateRead.Match(data) {
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

	// 2. The ./pkg/... patterns of the MONGO-CONFORMANCE job's go test run
	// — that job's block only. Scanning the whole workflow counted the
	// nats-conformance job's `./pkg/runner/... ./pkg/queue/...` (it
	// provisions no Mongo), so a Mongo-gated suite added under either
	// package would satisfy this guard while its assertions still ran
	// nowhere: precisely the silent green the test exists to end.
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "tests.yml"))
	if err != nil {
		t.Fatalf("read tests.yml: %v", err)
	}
	job, err := workflowJobBlock(string(wf), "mongo-conformance")
	if err != nil {
		t.Fatalf("%v — the workflow's shape changed; update this parser rather than widening the scan", err)
	}
	patterns := regexp.MustCompile(`\./pkg/[\w/-]+/\.\.\.`).FindAllString(job, -1)
	if len(patterns) == 0 {
		t.Fatal("no ./pkg/.../... patterns found in the mongo-conformance job — its command moved; update this parser")
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

// gateRead matches a real read of the gate variable, so a test file that
// merely MENTIONS it in a comment is not counted as gated.
var gateRead = regexp.MustCompile(`(?:Getenv|LookupEnv|Setenv)\(\s*"ITERION_TEST_MONGO_URI"`)

// workflowJobBlock returns the YAML lines belonging to one job under
// `jobs:` — from its own 2-space key to the next one (or EOF). Errors
// when the job cannot be located, so a renamed job fails loudly instead
// of silently degrading the guard to "scan everything", which is the
// weakening this helper exists to prevent.
func workflowJobBlock(yaml, job string) (string, error) {
	lines := strings.Split(yaml, "\n")
	nextJob := regexp.MustCompile(`^  [A-Za-z0-9_-]+:\s*$`)
	start := -1
	for i, ln := range lines {
		if strings.TrimRight(ln, " \t") == "  "+job+":" {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("job %q not found in the workflow", job)
	}
	for i := start + 1; i < len(lines); i++ {
		if nextJob.MatchString(lines[i]) {
			return strings.Join(lines[start:i], "\n"), nil
		}
	}
	return strings.Join(lines[start:], "\n"), nil
}

// TestMongoGateGuardIsNotVacuous pins the two halves that made the guard
// above weaker than its own doc claimed — each masking the other, so
// fixing either alone turns CI red on a false alarm.
func TestMongoGateGuardIsNotVacuous(t *testing.T) {
	root := "../../.."
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "tests.yml"))
	if err != nil {
		t.Fatalf("read tests.yml: %v", err)
	}
	// Half 1: the job slice must EXCLUDE the nats job's package list.
	// That job provisions no Mongo, so counting its packages as covered
	// is the silent green this guard exists to end.
	job, err := workflowJobBlock(string(wf), "mongo-conformance")
	if err != nil {
		t.Fatalf("locate mongo-conformance: %v", err)
	}
	if strings.Contains(job, "./pkg/queue/...") {
		t.Fatal("the mongo-conformance slice reaches the nats-conformance job's package list — the guard would " +
			"count a Mongo-gated suite as covered by a job that provisions no Mongo")
	}
	if !strings.Contains(job, "./pkg/store/mongo/...") {
		t.Fatal("the mongo-conformance slice lost the job's own package list — the parser is cutting in the wrong place")
	}

	// Half 2: the gated detector must read an ENV CALL, not prose. A
	// substring match counted pkg/runner, whose only mention of the
	// variable is a comment.
	if gateRead.MatchString(`// mirror of ITERION_TEST_MONGO_URI in pkg/store/mongo`) {
		t.Fatal("the gated detector matches a comment — a package that merely names the variable reads as gated, " +
			"and this guard fails on a suite that was never Mongo-gated at all")
	}
	if !gateRead.MatchString(`uri := os.Getenv("ITERION_TEST_MONGO_URI")`) {
		t.Fatal("the gated detector misses a real Getenv — every genuinely gated package would go unchecked")
	}
}
