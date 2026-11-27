package main

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// TestTheReplayAccountsForEachCommit feeds a fake first-parent log through
// the accounting: the skipped release and tap commits, a global trigger, and
// two selections whose shares are known by hand.
func TestTheReplayAccountsForEachCommit(t *testing.T) {
	log := []logEntry{ // newest first, as git log gives them
		{"e5", "docs: a page"},
		{"d4", "fix: lib"},
		{"c3", "chore(brew): update tap"},
		{"b2", "ci: a workflow"},
		{"a1", "chore: release 1.0"},
	}
	diffs := map[string][]change{
		"e5": {{Path: "docs/a.md"}},
		"d4": {{Path: "pkg/lib/lib.go"}},
		"b2": {{Path: ".github/workflows/x.yml"}},
	}
	weights := map[string]float64{mod + "/pkg/lib": 10}
	sum, commits, err := replay(fixtureGraph(), fixtureInputs(), weights, log, func(sha string) ([]change, error) {
		c, ok := diffs[sha]
		if !ok {
			return nil, fmt.Errorf("diff of skipped commit %s asked", sha)
		}
		return c, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Eight test packages: pkg/lib weighs 10, the seven others 1 — 17.
	// e5 selects pkg/sweeper (1/17), d4 lib, app, builder, checker (13/17),
	// b2 everything (17/17).
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }
	if sum.Commits != 3 || sum.Skipped != 2 || sum.All != 1 || len(commits) != 3 {
		t.Fatalf("commits %d skipped %d all %d (%d listed), want 3, 2, 1", sum.Commits, sum.Skipped, sum.All, len(commits))
	}
	if sum.Oldest != "a1" || sum.Newest != "e5" {
		t.Errorf("window %s..%s, want a1..e5 (skipped commits bound it too)", sum.Oldest, sum.Newest)
	}
	if !near(sum.TimeShareMean, (1.0+13+17)/(3*17)) || !near(sum.TimeShareMed, 13.0/17) || !near(sum.AllShare, 1.0/3) {
		t.Errorf("all commits: time mean %v median %v, all share %v", sum.TimeShareMean, sum.TimeShareMed, sum.AllShare)
	}
	if w := sum.WithoutAll; w.Count != 2 || !near(w.TimeShareMean, 7.0/17) || !near(w.PkgShareP90, 4.0/8) || !near(w.PkgShareMedian, 1.0/8) {
		t.Errorf("without a global trigger: %+v, want 2 commits, time mean 7/17, packages median 1/8, p90 4/8", w)
	}
	if sum.LegCounts["unit"] != 3 || sum.LegCounts["lonely"] != 1 {
		t.Errorf("leg counts %v", sum.LegCounts)
	}
	var out bytes.Buffer
	sum.Since = "2026-11-19T00:00:00+01:00"
	printReplay(sum, &out)
	for _, want := range []string{"replayed 3 commits (2 release/tap commits skipped), since 2026-11-19T00:00:00+01:00: a1..e5", "without a global trigger (2)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary %q lacks %q", out.String(), want)
		}
	}
}

func TestAWindowOfSkippedCommitsIsAnError(t *testing.T) {
	_, _, err := replay(fixtureGraph(), fixtureInputs(), nil, []logEntry{{"a1", "chore: release 1"}}, nil)
	if err == nil {
		t.Fatal("a window of release commits replayed")
	}
}

// A bare date is that day's midnight: git's approxidate would fill in the
// time of day of the run and move the window.
func TestAReplayBoundIsAnExplicitMidnight(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	for in, want := range map[string]string{
		"2026-11-19":                "2026-11-19T00:00:00+01:00",
		"2026-11-19T12:30:00Z":      "2026-11-19T12:30:00Z",
		"2026-11-19T00:00:00+01:00": "2026-11-19T00:00:00+01:00",
	} {
		if got, err := replayBound(in, loc); err != nil || got != want {
			t.Errorf("replayBound(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"yesterday", "2026-11-19 12:00", "19/11/2026"} {
		if got, err := replayBound(in, loc); err == nil {
			t.Errorf("replayBound(%q) = %q, want an error", in, got)
		}
	}
}
