package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// TestPrintRewindFiles_NamesWhatItTook is the reporting half of issue
// #380. A rewind writes to the operator's live checkout, so "workspace
// reverted" on its own is not enough to notice that files moved — the
// count, the paths, and above all what was TAKEN have to be on screen,
// together with the command that undoes it.
func TestPrintRewindFiles_NamesWhatItTook(t *testing.T) {
	var buf bytes.Buffer
	printRewindFiles(&buf, &runview.RewindResult{
		RunID:  "run-1",
		NodeID: "implement",
		Files: &runview.FileRevertResult{
			Reverted:   true,
			BackupRef:  "1786-0007",
			Scope:      string(runview.RestoreScopeProduced),
			ScopeCount: 3,
			Restored: &workspacetrack.RestoreReport{
				Written: 2, Deleted: 1, Unchanged: 0,
				WrittenPaths: []string{"docs/intro.md", "src/a.go"},
				DeletedPaths: []string{"docs/api.md"},
			},
			Overwritten:      []string{"src/a.go"},
			OverwrittenCount: 1,
			LeftInPlace:      []string{"notes/human.md"},
			LeftInPlaceCount: 1,
		},
	})
	got := buf.String()
	for _, want := range []string{
		"3 path(s) this run recorded changing",
		"2 written, 1 deleted",
		"docs/intro.md",
		"docs/api.md",
		"were overwritten",
		"src/a.go",
		"--restore-snapshot 1786-0007", // the undo, on the same screen
		"left in place",
		"notes/human.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not mention %q:\n%s", want, got)
		}
	}
}

// TestPrintRewindFiles_EmptyScopeDoesNotClaimSuccess: when nothing was
// restored the operator must not read a line saying the workspace was
// reverted — they would resume onto whatever is actually on disk.
func TestPrintRewindFiles_EmptyScopeDoesNotClaimSuccess(t *testing.T) {
	var buf bytes.Buffer
	printRewindFiles(&buf, &runview.RewindResult{
		RunID:  "run-1",
		NodeID: "implement",
		Files: &runview.FileRevertResult{
			Reverted:         false,
			Scope:            string(runview.RestoreScopeProduced),
			SkipReason:       `no execution of this run is recorded as having changed any file after "implement" started, so nothing was restored`,
			LeftInPlace:      []string{"docs/debris.md"},
			LeftInPlaceCount: 1,
		},
	})
	got := buf.String()
	if strings.Contains(got, "workspace reverted") {
		t.Errorf("an empty scope reported as a revert:\n%s", got)
	}
	if !strings.Contains(got, "NOT restored") || !strings.Contains(got, "docs/debris.md") {
		t.Errorf("output must say nothing was restored and name what was left:\n%s", got)
	}
}

// TestJoinCapped_CountsFromTheTotal: the slice is already capped inside
// the result struct, so the remainder has to come from the exact count —
// deriving it from len(paths) would report "+0 more" on a 400-path
// restore.
func TestJoinCapped_CountsFromTheTotal(t *testing.T) {
	got := joinCapped([]string{"a", "b", "c"}, 400, 2)
	if got != "a, b (+398 more)" {
		t.Errorf("joinCapped = %q", got)
	}
	if got := joinCapped([]string{"a"}, 1, 5); got != "a" {
		t.Errorf("joinCapped with no remainder = %q", got)
	}
}
