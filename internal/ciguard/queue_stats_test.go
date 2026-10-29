package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The rebuild attribution of scripts/diagnostics/queue-stats.sh, run the way
// the report runs it: the shipped awk program over a builds table.
//
// Why this harness exists: `task ci:queue-stats` is the measurement #1979
// decides the queue's width on, and until now nothing in the tree could redden
// on its arithmetic. The measured defect it carries: the cause of a rebuild
// was blamed on a push only when the push's commit date fell within 90
// seconds of the rebuild, so GitHub rebuilding #1967's group 114 seconds after
// v3.217.0's commit was counted as the queue's own churn — an undercount of
// the one line the release hold exists to shrink.
const rebuildCauseProgram = "../../scripts/diagnostics/queue-rebuild-cause.awk"

// Epochs of the measured #1967 case, kept as the numbers the script hands the
// program (it works in epoch seconds; the dates are in the comments).
const (
	firstBuild   = "1790749680" // 2026-09-30T06:28:00Z — the build that was discarded
	releasePush  = "1790749801" // 2026-09-30T06:30:01Z — chore: release v3.217.0
	betweenTwo   = "1790749860" // 2026-09-30T06:31:00Z — after the push, before the rebuild
	theRebuild   = "1790749915" // 2026-09-30T06:31:55Z — 114 s after the release commit
	olderThanAll = "1790711520" // 2026-09-29T19:52:00Z
)

// buildRow is one row of the builds table the program reads, named so a case
// below reads as the facts it stands for rather than as ten tab-separated
// fields.
type buildRow struct {
	created   string // epoch
	pr        string
	baseKind  string // release, direct-merge, other-push, queue-merge, queue-chain
	outcome   string // success, failure, cancelled, or the status when unfinished
	runID     string
	updated   string // epoch
	status    string // completed, in_progress, queued
	inWindow  string // 1 inside the measured window, 0 on the day before it
	baseSHA   string
	baseDated string // epoch of the base commit, "-" when the base is off main
}

func (r buildRow) line() string {
	return strings.Join([]string{r.created, r.pr, r.baseKind, r.outcome, r.runID, r.updated, r.status, r.inWindow, r.baseSHA, r.baseDated}, "\t")
}

// rebuildReport runs the shipped program over the rows and returns its
// trimmed report lines, its stderr and its exit code.
func rebuildReport(t *testing.T, rows []buildRow) ([]string, string, int) {
	t.Helper()
	table := filepath.Join(t.TempDir(), "builds.tsv")
	var body strings.Builder
	for _, r := range rows {
		body.WriteString(r.line() + "\n")
	}
	if err := os.WriteFile(table, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("awk", "-f", scriptUnderTest(t, rebuildCauseProgram), table)
	cmd.Env = scriptEnv(t)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", rebuildCauseProgram, err)
		}
		code = exit.ExitCode()
	}
	var lines []string
	for _, l := range strings.Split(stdout.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, stderr.String(), code
}

// verdicts returns the "<state> -> <cause> <count>" rows and the summary line,
// sorted, so a case states what the program counted and nothing about the
// order a for-in loop walks an awk array in.
func verdicts(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "Rebuilds —") {
			continue
		}
		out = append(out, strings.Join(strings.Fields(l), " "))
	}
	sort.Strings(out)
	return out
}

const noDiscards = "Queue builds discarded by a direct push to main: 0 (already green 0, still running 0); pushes: release 0, direct merge 0, other 0"

// TestARebuildsCauseIsReadFromTheGroupsBase holds every rule of the
// attribution to a case, because the number it feeds — "queue builds discarded
// by a direct push" — is what decides whether the release hold and the queue's
// width are working.
//
// Mutations seen red, each on its own case (one line of
// queue-rebuild-cause.awk per mutation):
//
//   - re-adding the 90-second clock window (`&& $10 >= $1 - 90` on the push
//     condition): "a release 114 s before the rebuild" goes to cause=queue —
//     the measured #1967 undercount, reproduced;
//   - dropping the moved-base clause (`$9 != pbase[$2]`): "an entry ahead
//     failed on an unchanged base" blames the release that was already there;
//   - dropping the already-red exemption: "the replaced build had ejected its
//     pull request" is counted as a discarded build;
//   - reading the replaced build's state at the rebuild instead of at the
//     push (`at = $1`): a build that finished green between the push and the
//     rebuild is counted as "already green" when the push hit it running;
//   - counting rows from the day before the window ($8 == 0): the predecessor
//     rows become rebuilds the window never paid for.
func TestARebuildsCauseIsReadFromTheGroupsBase(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []buildRow
		want []string
	}{
		{
			// #1967, measured 2026-09-30: the release commit is the new
			// group's base, 114 seconds before the rebuild. The clock window
			// this replaces blamed the queue.
			name: "a release that moved the base is the cause whatever the delay",
			rows: []buildRow{
				{created: firstBuild, pr: "1967", baseKind: "queue-merge", outcome: "in_progress", runID: "1", updated: firstBuild, status: "in_progress", inWindow: "1", baseSHA: "85747aec0", baseDated: olderThanAll},
				{created: theRebuild, pr: "1967", baseKind: "release", outcome: "in_progress", runID: "2", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "b6a7334d8", baseDated: releasePush},
			},
			want: []string{
				"Queue builds discarded by a direct push to main: 1 (already green 0, still running 1); pushes: release 1, direct merge 0, other 0",
				"in-flight -> release 1",
			},
		},
		{
			// The expensive case the release hold exists to shrink: a full
			// green run thrown away.
			name: "a build already green when the push hit is counted apart",
			rows: []buildRow{
				{created: olderThanAll, pr: "1964", baseKind: "queue-chain", outcome: "success", runID: "1", updated: firstBuild, status: "completed", inWindow: "1", baseSHA: "aaaaaaaaa", baseDated: "-"},
				{created: theRebuild, pr: "1964", baseKind: "release", outcome: "in_progress", runID: "2", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "b6a7334d8", baseDated: releasePush},
			},
			want: []string{
				"Queue builds discarded by a direct push to main: 1 (already green 1, still running 0); pushes: release 1, direct merge 0, other 0",
				"success -> release 1",
			},
		},
		{
			// It finished green, but AFTER the push: it was running when main
			// moved under it, so the push discarded a running build. Reading
			// the state at the rebuild instead would call it already green.
			name: "a build that finished between the push and the rebuild was running when the push hit",
			rows: []buildRow{
				{created: firstBuild, pr: "1965", baseKind: "queue-chain", outcome: "success", runID: "1", updated: betweenTwo, status: "completed", inWindow: "1", baseSHA: "aaaaaaaaa", baseDated: "-"},
				{created: theRebuild, pr: "1965", baseKind: "release", outcome: "in_progress", runID: "2", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "b6a7334d8", baseDated: releasePush},
			},
			want: []string{
				"Queue builds discarded by a direct push to main: 1 (already green 0, still running 1); pushes: release 1, direct merge 0, other 0",
				"in-flight -> release 1",
			},
		},
		{
			// Same base sha: main did not move, so the queue re-formed the
			// group — an entry ahead failed or left. The base's KIND is a
			// release here on purpose: the sha is what decides.
			name: "an unchanged base is the queue re-forming the group, not the push that is already in it",
			rows: []buildRow{
				{created: firstBuild, pr: "1970", baseKind: "release", outcome: "in_progress", runID: "1", updated: firstBuild, status: "in_progress", inWindow: "1", baseSHA: "b6a7334d8", baseDated: releasePush},
				{created: theRebuild, pr: "1970", baseKind: "release", outcome: "in_progress", runID: "2", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "b6a7334d8", baseDated: releasePush},
			},
			want: []string{noDiscards, "in-flight -> queue 1"},
		},
		{
			// The replaced build had already reported red before main moved:
			// it had ejected its pull request, so the rebuild is a
			// re-enqueue the queue owns.
			name: "a build that had already finished red is a re-enqueue, not a discarded build",
			rows: []buildRow{
				{created: olderThanAll, pr: "1971", baseKind: "queue-chain", outcome: "failure", runID: "1", updated: firstBuild, status: "completed", inWindow: "1", baseSHA: "aaaaaaaaa", baseDated: "-"},
				{created: theRebuild, pr: "1971", baseKind: "release", outcome: "in_progress", runID: "2", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "b6a7334d8", baseDated: releasePush},
			},
			want: []string{noDiscards, "failure -> queue 1"},
		},
		{
			// The queue merging the entry ahead is its own progress, not a
			// push that discards work.
			name: "a base that moved to a queue merge is the queue",
			rows: []buildRow{
				{created: firstBuild, pr: "1972", baseKind: "queue-chain", outcome: "in_progress", runID: "1", updated: firstBuild, status: "in_progress", inWindow: "1", baseSHA: "aaaaaaaaa", baseDated: "-"},
				{created: theRebuild, pr: "1972", baseKind: "queue-merge", outcome: "in_progress", runID: "2", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "ccccccccc", baseDated: releasePush},
			},
			want: []string{noDiscards, "in-flight -> queue 1"},
		},
		{
			name: "a direct merge and any other push each name themselves",
			rows: []buildRow{
				{created: firstBuild, pr: "1973", baseKind: "queue-chain", outcome: "in_progress", runID: "1", updated: firstBuild, status: "in_progress", inWindow: "1", baseSHA: "aaaaaaaaa", baseDated: "-"},
				{created: betweenTwo, pr: "1973", baseKind: "direct-merge", outcome: "in_progress", runID: "2", updated: betweenTwo, status: "in_progress", inWindow: "1", baseSHA: "ddddddddd", baseDated: firstBuild},
				{created: theRebuild, pr: "1973", baseKind: "other-push", outcome: "in_progress", runID: "3", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "eeeeeeeee", baseDated: betweenTwo},
			},
			want: []string{
				"Queue builds discarded by a direct push to main: 2 (already green 0, still running 2); pushes: release 0, direct merge 1, other 1",
				"in-flight -> direct-merge 1",
				"in-flight -> other-push 1",
			},
		},
		{
			// The day before the window is read only to find the build a
			// rebuild replaced: its own rebuilds belong to the previous
			// period's count.
			name: "a rebuild created before the window is a predecessor, never a count",
			rows: []buildRow{
				{created: olderThanAll, pr: "1974", baseKind: "queue-chain", outcome: "in_progress", runID: "1", updated: olderThanAll, status: "in_progress", inWindow: "0", baseSHA: "aaaaaaaaa", baseDated: "-"},
				{created: firstBuild, pr: "1974", baseKind: "release", outcome: "in_progress", runID: "2", updated: firstBuild, status: "in_progress", inWindow: "0", baseSHA: "b6a7334d8", baseDated: olderThanAll},
				{created: theRebuild, pr: "1974", baseKind: "other-push", outcome: "in_progress", runID: "3", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "eeeeeeeee", baseDated: releasePush},
			},
			want: []string{
				"Queue builds discarded by a direct push to main: 1 (already green 0, still running 1); pushes: release 0, direct merge 0, other 1",
				"in-flight -> other-push 1",
			},
		},
		{
			name: "a first build is nobody's rebuild",
			rows: []buildRow{
				{created: theRebuild, pr: "1975", baseKind: "release", outcome: "in_progress", runID: "1", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "b6a7334d8", baseDated: releasePush},
			},
			want: []string{noDiscards},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, stderr, code := rebuildReport(t, tc.rows)
			if code != 0 {
				t.Fatalf("the program exited %d\nstderr: %s", code, stderr)
			}
			if len(lines) == 0 || !strings.HasPrefix(lines[0], "Rebuilds —") {
				t.Fatalf("the report does not open with its header: %q", lines)
			}
			got := verdicts(lines)
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("the attribution reports\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
		})
	}
}

// TestTheAttributionRefusesRowsItsCallerDidNotSort closes the one way this
// program could report a measurement built on an accident: which build a
// rebuild replaced is read from the ROW ORDER, and nothing in a table of rows
// carries that order. The script sorts (`sort -n` on the creation column); a
// caller that does not would otherwise get a plausible report built on the
// wrong predecessors.
//
// The witness: drop the guard and this case gets a report — specifically
// "in-flight -> release 1" from reading the rebuild as the predecessor of the
// build it replaced.
func TestTheAttributionRefusesRowsItsCallerDidNotSort(t *testing.T) {
	lines, stderr, code := rebuildReport(t, []buildRow{
		{created: theRebuild, pr: "1967", baseKind: "release", outcome: "in_progress", runID: "2", updated: theRebuild, status: "in_progress", inWindow: "1", baseSHA: "b6a7334d8", baseDated: releasePush},
		{created: firstBuild, pr: "1967", baseKind: "queue-merge", outcome: "in_progress", runID: "1", updated: firstBuild, status: "in_progress", inWindow: "1", baseSHA: "85747aec0", baseDated: olderThanAll},
	})
	if code == 0 {
		t.Errorf("the program accepted unsorted rows and reported %q — a report read off the wrong predecessors looks exactly like a measurement", lines)
	}
	if len(lines) != 0 {
		t.Errorf("the program printed a report before refusing: %q", lines)
	}
	if !strings.Contains(stderr, "sorted by creation") {
		t.Errorf("the refusal does not say what is wrong with the input: %q", stderr)
	}
}

// TestTheQueueStatsScriptRunsTheProgramThisHarnessRuns pins the pair: a
// program a harness runs and a report that runs a copy of it is a harness
// that proves nothing. It also holds the table's column count, which is the
// contract between the two halves.
func TestTheQueueStatsScriptRunsTheProgramThisHarnessRuns(t *testing.T) {
	script := string(mustRead(t, "../../scripts/diagnostics/queue-stats.sh"))
	if !strings.Contains(script, `awk -f "$cause_program" "$work/builds.tsv"`) {
		t.Errorf("queue-stats.sh no longer runs the rebuild attribution as a program file — the harness in this file would then exercise a copy nothing ships")
	}
	if !strings.Contains(script, "queue-rebuild-cause.awk") {
		t.Errorf("queue-stats.sh does not name %s", rebuildCauseProgram)
	}
	// The row the script writes and the row the program reads are one
	// contract: ten fields, the last two being what the attribution reads.
	if !strings.Contains(script, `"\t" p[2] "\t" w }`) {
		t.Errorf("queue-stats.sh no longer appends the base sha and the base commit date to builds.tsv — the attribution reads fields 9 and 10 and would silently compare empty strings")
	}
	program := string(mustRead(t, rebuildCauseProgram))
	if !strings.Contains(program, "$9 != pbase[$2]") {
		t.Errorf("%s no longer compares the base sha against the replaced build's base — the cause would be read from the base's KIND alone, which blames a release still sitting in an unchanged base", rebuildCauseProgram)
	}
}
