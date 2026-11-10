package ciguard

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// releaseWrittenPaths is what a release commit writes, each entry named by the
// writer that writes it. tests.yml's `push` trigger ignores exactly these, so
// a release creates no run and cancels no merge's witness (#3161).
//
// The pairing is the point: a path here with no writer is a path that stopped
// being part of a release, and ignoring it then costs a real run for nothing.
var releaseWrittenPaths = map[string]string{
	"package.json":              "release-it's own version bump",
	"CHANGELOG.md":              ".release-it.mjs: the conventional-changelog plugin's `infile`",
	"charts/iterion/Chart.yaml": ".release-it.mjs hook after:bump -> scripts/sync-chart-version.sh",
	"internal/mdcode/testdata/committed-reading.txt": ".release-it.mjs hook before:git:beforeRelease -> cmd/corpus-reading --cut",
}

const (
	testsWorkflowPath = "../../.github/workflows/tests.yml"
	releaseItConfig   = "../../.release-it.mjs"
	repoRoot          = "../.."
)

// TestTheReleaseCommitCreatesNoTestsRun holds the fix of #3161 against the two
// ways it dies in silence.
//
// It cannot catch a release that grows a FIFTH written path — `cmd/release-floors
// --apply` realigns syntax-floor pins and did touch a bot manifest once in the
// last twenty releases. That direction is safe by construction: the release then
// creates its run exactly as it did before the fix, so the cost is "no
// improvement", never a new defect, and no guard is owed for it.
//
// The dangerous direction is a path that stops existing — renamed, moved,
// regenerated elsewhere. `paths-ignore` matches on a string GitHub never
// validates, so a stale entry silently stops matching, the release resumes
// cancelling, and nothing anywhere says so. That is what this pins.
//
// Mutations seen red: renaming an ignored path in the workflow (the tree check
// names it); dropping one from the workflow (the writer check names it); and
// changing the `infile` in .release-it.mjs without touching the workflow (the
// config check names it).
func TestTheReleaseCommitCreatesNoTestsRun(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile(testsWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", testsWorkflowPath, err)
	}
	var wf struct {
		On struct {
			Push struct {
				Branches    []string `yaml:"branches"`
				PathsIgnore []string `yaml:"paths-ignore"`
			} `yaml:"push"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(src, &wf); err != nil {
		t.Fatalf("parse %s: %v", testsWorkflowPath, err)
	}

	ignored := wf.On.Push.PathsIgnore
	if len(ignored) == 0 {
		t.Fatalf("tests.yml's push trigger carries no paths-ignore: the release commit then shares the ref-keyed concurrency group with the merge it followed and cancels its run (#3161). "+
			"Restore the list, or if this is deliberate say so here and delete this guard — a fix nobody can see failing is worse than none. Expected at least: %v", sortedKeys(releaseWrittenPaths))
	}

	// (1) Every ignored path must exist. This is the silent killer.
	for _, p := range ignored {
		if strings.ContainsAny(p, "*?[") {
			continue // a glob is not a path the tree can be asked about
		}
		if _, statErr := os.Stat(filepath.Join(repoRoot, p)); statErr != nil {
			t.Errorf("tests.yml ignores %q on push, but the tree has no such file (%v): paths-ignore matches a string GitHub never validates, "+
				"so this entry stops matching in silence and the release resumes cancelling the merge's witness. Point it at the file's new name", p, statErr)
		}
	}

	// (2) Every path a release writes must be ignored, or the fix is partial
	// in a way no one notices until a witness goes missing again.
	ignoredSet := map[string]bool{}
	for _, p := range ignored {
		ignoredSet[p] = true
	}
	for p, writer := range releaseWrittenPaths {
		if !ignoredSet[p] {
			t.Errorf("a release writes %q (%s) and tests.yml's push trigger does NOT ignore it: the release commit then changes a file outside the list, "+
				"so its run is created and cancels the run of the merge it followed (#3161)", p, writer)
		}
	}

	// (3) Every ignored path must still be a release's to write. An entry whose
	// writer is gone is a real run skipped for nothing.
	for _, p := range ignored {
		if _, known := releaseWrittenPaths[p]; !known {
			t.Errorf("tests.yml ignores %q on push, but releaseWrittenPaths does not name a release writer for it: a commit touching only that file gets no Tests run on main. "+
				"Either name the writer here, or stop ignoring it", p)
		}
	}

	// (4) The CHANGELOG entry is tied to the release config that owns it, so
	// the two cannot drift apart unnoticed.
	cfg, err := os.ReadFile(releaseItConfig)
	if err != nil {
		t.Fatalf("read %s: %v", releaseItConfig, err)
	}
	if !strings.Contains(string(cfg), "infile: 'CHANGELOG.md'") {
		t.Errorf("%s no longer declares `infile: 'CHANGELOG.md'`: the changelog a release writes moved or is configured another way, so tests.yml's paths-ignore names a file the release may not touch and may miss the one it does", releaseItConfig)
	}

	// The push trigger must still be branch-scoped: paths-ignore on an
	// unfiltered push would skip these files on every branch, which is not
	// what #3161 asked for.
	if len(wf.On.Push.Branches) != 1 || wf.On.Push.Branches[0] != "main" {
		t.Errorf("tests.yml's push trigger is scoped to %v, want exactly [main]: the release skip is reasoned about main's ref-keyed concurrency group alone", wf.On.Push.Branches)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
