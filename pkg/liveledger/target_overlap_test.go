package liveledger

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestNoTwoRecordingTargetsSelectTheSameLiveTest is the #2784 guard: a
// target's -run pattern must select exactly the live tests its name
// claims. Two cheap targets carried an UNANCHORED pattern
// (TestLive_FeatureDev, TestLive_VibeReviewAlternating) that also matched
// their _Real sibling in the same file — so asking for the cheap target
// paid for the expensive one, the cheap target's 90m/45m timeout sat
// under a _Real half budgeted 2h/3h (a `panic: test timed out` records a
// fail verdict that is not a finding), and the cheap target's ledger row
// merged both fail-sticky, answering neither "does the cheap one pass?"
// nor "does the real one?".
//
// The rule: among targets whose selection is scoped to ONE source file
// (cross-file aggregates like test:live / test:live:feats:all are exempt —
// they own no per-test verdict by design), no two targets may match the
// same TestLive_* function. The deliberate one-file pair
// (test:live:openai-compatible: two functions, one file, one row) trips
// nothing because no SECOND target selects either function.
func TestNoTwoRecordingTargetsSelectTheSameLiveTest(t *testing.T) {
	root := repoRoot(t)
	targets, err := EnumerateLiveTargets(filepath.Join(root, TaskfileRelPath))
	if err != nil {
		t.Fatalf("EnumerateLiveTargets: %v", err)
	}
	sources, err := readLiveTestSources(filepath.Join(root, "e2e"))
	if err != nil {
		t.Fatalf("read live test sources: %v", err)
	}
	overlaps, err := singleFileTargetOverlaps(targets, liveTestFiles(sources))
	if err != nil {
		t.Fatalf("singleFileTargetOverlaps: %v", err)
	}
	if len(overlaps) > 0 {
		var lines []string
		for _, fn := range sortedKeys(overlaps) {
			lines = append(lines, fmt.Sprintf("  - %s is selected by %s", fn, strings.Join(overlaps[fn], ", ")))
		}
		t.Fatalf("%d live test(s) are selected by more than one single-file target — the cheaper target silently runs (and times out, and bills, and fail-marks) the other target's test:\n%s\n\nAnchor the cheaper target's -run pattern (TestLive_FeatureDev$ is the shape) so each target selects exactly the tests its name claims.",
			len(overlaps), strings.Join(lines, "\n"))
	}
}

// singleFileTargetOverlaps maps every live test matched by MORE THAN ONE
// single-file-scoped target to the targets matching it. A target is
// single-file-scoped when its -run pattern matches at least one live
// test and every match sits in one source file; empty patterns (they
// filter nothing) and cross-file selections (aggregates) are out of the
// rule's scope.
func singleFileTargetOverlaps(targets []LiveTarget, fileOf map[string]string) (map[string][]string, error) {
	byTest := map[string][]string{}
	for _, tt := range targets {
		matched, files, err := matchLiveTests(tt, fileOf)
		if err != nil {
			return nil, err
		}
		if len(matched) == 0 || len(files) > 1 {
			continue
		}
		for _, fn := range matched {
			byTest[fn] = append(byTest[fn], tt.Name)
		}
	}
	out := map[string][]string{}
	for fn, names := range byTest {
		if len(names) > 1 {
			sort.Strings(names)
			out[fn] = names
		}
	}
	return out, nil
}

// matchLiveTests answers which live test functions a target's -run
// pattern selects and which files they live in, using Go's own -run
// semantics: an unanchored regexp over the test name, where the part
// before the first `/` applies to the top-level function.
func matchLiveTests(tt LiveTarget, fileOf map[string]string) (matched []string, files map[string]bool, err error) {
	files = map[string]bool{}
	if tt.RunPattern == "" {
		return nil, files, nil
	}
	top := tt.RunPattern
	if i := strings.Index(top, "/"); i >= 0 {
		top = top[:i]
	}
	re, err := regexp.Compile(top)
	if err != nil {
		return nil, nil, fmt.Errorf("target %s: -run pattern %q does not compile as a Go test filter: %w", tt.Name, tt.RunPattern, err)
	}
	for fn, file := range fileOf {
		if re.MatchString(fn) {
			matched = append(matched, fn)
			files[file] = true
		}
	}
	sort.Strings(matched)
	return matched, files, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSingleFileTargetOverlaps pins the rule on fixtures: the #2784 shape
// (a cheap target's unanchored pattern reaching its _Real sibling) is
// detected, the anchored fix is clean, the deliberate one-file pair with
// no second target is clean, and a cross-file aggregate is out of scope.
func TestSingleFileTargetOverlaps(t *testing.T) {
	fileOf := map[string]string{
		"TestLive_FeatureDev":                      "live_bot_feature_dev_test.go",
		"TestLive_FeatureDev_Real":                 "live_bot_feature_dev_test.go",
		"TestLive_Feat_OpenAICompatible_InProcess": "live_feat_openai_compatible_test.go",
		"TestLive_Feat_OpenAICompatible_Sandboxed": "live_feat_openai_compatible_test.go",
	}

	overlapping := []LiveTarget{
		{Name: "test:live:feature-dev", RunPattern: "TestLive_FeatureDev"},
		{Name: "test:live:feature-dev-real", RunPattern: "TestLive_FeatureDev_Real$"},
	}
	got, err := singleFileTargetOverlaps(overlapping, fileOf)
	if err != nil {
		t.Fatal(err)
	}
	names := got["TestLive_FeatureDev_Real"]
	if len(names) != 2 {
		t.Fatalf("the unanchored pair must be caught: got %v", got)
	}
	if _, ok := got["TestLive_FeatureDev"]; ok {
		t.Fatalf("the cheap test is claimed by its own target alone: got %v", got)
	}

	fixed := []LiveTarget{
		{Name: "test:live:feature-dev", RunPattern: "TestLive_FeatureDev$"},
		{Name: "test:live:feature-dev-real", RunPattern: "TestLive_FeatureDev_Real$"},
		{Name: "test:live:openai-compatible", RunPattern: "TestLive_Feat_OpenAICompatible"},
		{Name: "test:live", RunPattern: "TestLive_"},
		{Name: "test:live:status", RunPattern: ""},
	}
	got, err = singleFileTargetOverlaps(fixed, fileOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("anchored targets, the deliberate one-file pair, the cross-file aggregate and the pattern-less target must all be clean: got %v", got)
	}

	if _, err := singleFileTargetOverlaps([]LiveTarget{{Name: "broken", RunPattern: "TestLive_(["}}, fileOf); err == nil {
		t.Fatal("a -run pattern that does not compile must be an explicit error")
	}
}
