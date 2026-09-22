package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// runOutputPaths decides what counts as the pass's work; the tree-noise list
// (pkg/treenoise) is what it sets aside. F6 of the plan review: git's
// `:(exclude,top).claude` hides a top-level FILE named .claude too, so the
// predicate must match the exact name as well as the prefix — otherwise a
// worktree "dirty" with noise alone sends finalize into an empty commit and
// a PreserveWorktree warning.
func TestRunOutputPathsLeaveTheTreeNoiseOut(t *testing.T) {
	porcelain := strings.Join([]string{
		" M docs/adr/0009-record.md",            // real work, tracked modification
		"?? .claude/settings.json",              // the engine's mirror
		"?? .claude",                            // a top-level FILE named .claude (F6)
		" M devbox.lock",                        // the drift every devbox run writes
		"?? devbox.json",                        // NOT noise: a dependency bot's deliverable
		"?? .claudeish",                         // a sibling that merely starts alike
		"R  old.md -> docs/new.md",              // a rename: the destination decides
		`?? "docs/caf\303\251 note.md"`,         // C-quoted by core.quotePath: decoded to its bytes
		`?? ".claude/caf\303\251.md"`,           // the same quoting on a mirror file: still noise
		`R  "a b.md" -> "docs/na\303\257ve.md"`, // quoted rename: the decoded destination decides
	}, "\n")
	got := runOutputPaths(porcelain)
	want := []string{"docs/adr/0009-record.md", "devbox.json", ".claudeish", "docs/new.md", "docs/café note.md", "docs/naïve.md"}
	if len(got) != len(want) {
		t.Fatalf("runOutputPaths = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runOutputPaths[%d] = %q, want %q (full: %q)", i, got[i], want[i], got)
		}
	}
}

// F4 (plan review): a dependency bot that dies before its commit leaves its
// DELIVERABLE dirty — devbox.json (real work, banked) beside the lock that
// travels with it (noise, dropped). Dropping the lock is safe on purpose:
// the lock is derivable from devbox.json, and banking it would pin a half-
// written resolution into a wip commit nothing ever merges.
func TestRunOutputPathsKeepTheDeliverableAndDropItsLock(t *testing.T) {
	porcelain := strings.Join([]string{
		" M devbox.json",
		" M devbox.lock",
	}, "\n")
	got := runOutputPaths(porcelain)
	if len(got) != 1 || got[0] != "devbox.json" {
		t.Fatalf("runOutputPaths = %q, want [devbox.json] — the deliverable is banked, the lock travels with it unbanked", got)
	}
}

// stagingRepo is a work tree in the run's shape: the engine's mirror laid
// untracked (a dir-only ignore rule such as this repository's `**/.claude/`
// resolves against an existing directory), a lock beside it, when gitignore
// is non-empty a committed .gitignore holding the rules, and any extra
// untracked file a case needs.
func stagingRepo(t *testing.T, gitignore string, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	gittest.InitRepo(t, dir)
	if gitignore != "" {
		addCommit(t, dir, ".gitignore", gitignore, "ignore rules")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".claude", "skills", "mirrored.md"), "iterion wrote this\n")
	writeFile(t, filepath.Join(dir, "devbox.lock"), "plugin_version: 0.0.5\n")
	for _, rel := range extra {
		writeFile(t, filepath.Join(dir, rel), "laid by the case\n")
	}
	return dir
}

// The staging gesture must agree with the probe that decides whether
// staging is needed at all: both carry the tree-noise pathspecs, or the
// wip bank sweeps the mirror and the drifted lock the moment anything real
// is dirty — the disagreement #1364's fix removed for the probe alone.
// Spelled, that is, where `git add` would not refuse the exclusion: one
// naming a present, ignored, unindexed path makes the gesture exit 1 with
// the index right (#1558), so exactly those entries are left out. The
// wildcard exclusion is spelled under a rule for the PATTERN (its literal
// part names no path) and dropped when a file spelled exactly like that
// literal part exists and is ignored — the one shape git refuses it in.
// Asked from a subdirectory the probe still names the repository-top path
// the exclusion names.
func TestStageWorkArgsCarryTheTreeNoisePathspecs(t *testing.T) {
	all := []string{"add", "-A", "--", ":/", ":(exclude,top).claude", ":(exclude,top)devbox.lock", ":(exclude,top).iterion-script-*"}
	cases := []struct {
		name, gitignore, sub string
		extra                []string
		want                 []string
	}{
		{"nothing ignored", "", "", nil, all},
		{"the mirror ignored, as this repository does", "**/.claude/\n", "", nil, []string{"add", "-A", "--", ":/", ":(exclude,top)devbox.lock", ":(exclude,top).iterion-script-*"}},
		{"the lock ignored", "devbox.lock\n", "", nil, []string{"add", "-A", "--", ":/", ":(exclude,top).claude", ":(exclude,top).iterion-script-*"}},
		{"every plain entry ignored, the scratch rule too", "**/.claude/\ndevbox.lock\n.iterion-script-*\n", "", nil, []string{"add", "-A", "--", ":/", ":(exclude,top).iterion-script-*"}},
		{"a file spelled exactly like the scratch prefix, ignored", ".iterion-script-\n", "", []string{".iterion-script-"}, []string{"add", "-A", "--", ":/", ":(exclude,top).claude", ":(exclude,top)devbox.lock"}},
		{"asked from a subdirectory of a repository ignoring the mirror", "**/.claude/\n", "sub", nil, []string{"add", "-A", "--", ":/", ":(exclude,top)devbox.lock", ":(exclude,top).iterion-script-*"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := stagingRepo(t, tc.gitignore, tc.extra...)
			if tc.sub != "" {
				dir = filepath.Join(dir, tc.sub)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if got := stageWorkArgs(dir); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("stageWorkArgs(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
	t.Run("no work tree: every exclusion, the gesture reports the failure itself", func(t *testing.T) {
		if got := stageWorkArgs(filepath.Join(t.TempDir(), "absent")); !reflect.DeepEqual(got, all) {
			t.Fatalf("stageWorkArgs(absent dir) = %q, want %q", got, all)
		}
	})
	// Git cannot answer here: `rev-parse --show-toplevel` refuses (128,
	// "must be run in a work tree"), and so would check-ignore — a reading
	// that took either failure for "ignored" would drop every exclusion.
	t.Run("bare repository: git cannot answer, every exclusion is spelled", func(t *testing.T) {
		dir := t.TempDir()
		gittest.Run(t, dir, "init", "--bare", "-q")
		if got := stageWorkArgs(dir); !reflect.DeepEqual(got, all) {
			t.Fatalf("stageWorkArgs(bare repo) = %q, want %q", got, all)
		}
	})
}

// A TRACKED file the rules also cover is skipped by `git add`'s untracked
// walk before the rules are consulted: the exclusion is not refused there,
// so it stays spelled — and excludes the drifted lock as it should. A
// probe asking the rules alone (--no-index) for a file would drop it and
// bank the lock.
func TestStageWorkArgsKeepTheExclusionOfATrackedFileTheRulesAlsoIgnore(t *testing.T) {
	dir := t.TempDir()
	gittest.InitRepo(t, dir)
	addCommit(t, dir, "devbox.lock", "plugin_version: 0.0.4\n", "track the lock")
	addCommit(t, dir, ".gitignore", "devbox.lock\n", "then ignore it")
	writeFile(t, filepath.Join(dir, "devbox.lock"), "plugin_version: 0.0.5\n")
	writeFile(t, filepath.Join(dir, "real.md"), "the pass's work\n")

	args := stageWorkArgs(dir)
	if !slices.Contains(args, ":(exclude,top)devbox.lock") {
		t.Fatalf("the tracked lock's exclusion was dropped: %q", args)
	}
	gittest.Run(t, dir, args...) // exit 1 here would be the #1558 failure on this shape
	staged := strings.Split(strings.TrimSpace(gittest.Run(t, dir, "diff-index", "--cached", "--name-only", "HEAD")), "\n")
	if want := []string{"real.md"}; !reflect.DeepEqual(staged, want) {
		t.Fatalf("staged = %q, want %q — the drifted tracked lock stays out", staged, want)
	}
}

// The floor the godoc of stagingExclusions names, held so a change here is
// a decision and not an accident: a file TRACKED under the mirror before
// the ignore rule arrived (this repository carves such paths out of its
// own `**/.claude/`), then modified, rides the wip bank as any tracked
// file does — `git add` classifies the ignored directory on the rules
// alone, tracked content or not, so no exclusion can be spelled there, and
// the gesture that spelled one exited 1 with the work unbanked (#1558). A
// probe consulting the index for the directory would keep spelling it. The
// untracked mirror file beside it stays out by git's own rule.
func TestStageWorkArgsLetATrackedFileUnderTheIgnoredMirrorRide(t *testing.T) {
	dir := t.TempDir()
	gittest.InitRepo(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, dir, filepath.Join(".claude", "rules", "tracked.md"), "committed before the rule\n", "track a rule under the mirror")
	addCommit(t, dir, ".gitignore", "**/.claude/\n", "then ignore the mirror")
	writeFile(t, filepath.Join(dir, ".claude", "rules", "tracked.md"), "modified during the run\n")
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".claude", "skills", "mirrored.md"), "iterion wrote this\n")
	writeFile(t, filepath.Join(dir, "real.md"), "the pass's work\n")

	args := stageWorkArgs(dir)
	for _, a := range args {
		if a == ":(exclude,top).claude" {
			t.Fatalf("the ignored mirror is still spelled: %q", args)
		}
	}
	gittest.Run(t, dir, args...) // exit 1 here is the #1558 failure itself
	staged := strings.Split(strings.TrimSpace(gittest.Run(t, dir, "diff-index", "--cached", "--name-only", "HEAD")), "\n")
	want := []string{".claude/rules/tracked.md", "real.md"}
	if !reflect.DeepEqual(staged, want) {
		t.Fatalf("staged = %q, want %q — the tracked file rides, the untracked mirror file stays out", staged, want)
	}
}

// The operator-initiated commit-and-finalize disagrees on purpose (verdict
// 3, R5478b3): a tracked-and-modified devbox.lock is the dependency work
// half the commit carries, so the gesture excludes ONLY the mirror — the
// wip bank keeps the fuller list, this path does not. Ignoring the lock
// changes nothing here; a repository ignoring the mirror leaves the gesture
// with no exclusion to spell at all (#1558).
func TestCommitStageArgsExcludeOnlyTheMirror(t *testing.T) {
	mirrorOnly := []string{"add", "-A", "--", ":/", ":(exclude,top).claude"}
	bare := []string{"add", "-A", "--", ":/"}
	for _, tc := range []struct {
		name, gitignore string
		want            []string
	}{
		{"nothing ignored", "", mirrorOnly},
		{"the lock ignored", "devbox.lock\n", mirrorOnly},
		{"the mirror ignored", "**/.claude/\n", bare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := commitStageArgs(stagingRepo(t, tc.gitignore)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("commitStageArgs(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// End to end, on the real finalize path: a worktree whose dirt is the
// engine's mirror, a drifted lock and ONE real file banks the real file
// only — the wip commit the operator is shown never carries tree noise.
func TestFinalizeWorktree_WipBankLeavesTheTreeNoiseOut(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	// The tracked lock, as the run found it.
	addCommit(t, wt, "devbox.lock", "plugin_version: 0.0.4\n", "baseline with a lock")

	// The noise (the mirror, untracked; the lock, drifted) and the run's
	// own work beside it.
	if err := os.MkdirAll(filepath.Join(wt, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".claude", "skills", "mirrored.md"), "iterion wrote this\n")
	writeFile(t, filepath.Join(wt, "devbox.lock"), "plugin_version: 0.0.5\n")
	writeFile(t, filepath.Join(wt, "real.md"), "the pass's work\n")
	// A tool-node script scratch file the cleanup lost the race against:
	// the wip bank must set it aside with the rest of the noise, not bank
	// it as the pass's work (the round-1 sweep's entry, witness wanted).
	writeFile(t, filepath.Join(wt, ".iterion-script-probe.sh"), "iterion scratch\n")

	var logBuf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &logBuf)
	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "wip-noise-test", runID: "run_n", autoMerge: true, mergeStrategy: "merge"}, logger)

	if !res.WipBanked {
		t.Fatalf("expected WipBanked=true, got %+v", res)
	}
	show := gittest.Run(t, repo, "show", "--stat", "--format=%s", res.FinalCommit)
	if !strings.Contains(show, "real.md") {
		t.Fatalf("banked commit missing the run's work:\n%s", show)
	}
	if strings.Contains(show, ".claude") {
		t.Fatalf("banked commit carries the .claude/ mirror:\n%s", show)
	}
	if strings.Contains(show, ".iterion-script-probe") {
		t.Fatalf("banked commit carries the tool-node script scratch:\n%s", show)
	}
	// The banked commit never carries the noise, so the log must NAME what
	// was set aside — a silent exclusion is the failure mode the split
	// exists to close (verdict 8). The set-aside emit is human-facing and
	// does not truncate (pkg/log), and git's porcelain order is
	// deterministic, so each named entry is asserted where it lands.
	for _, want := range []string{"tree noise set aside", "devbox.lock", ".iterion-script-probe.sh"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Fatalf("finalize log misses %q:\n%s", want, logBuf.String())
		}
	}
	lockShow, lockErr := gittest.Try(repo, "show", res.FinalCommit+":devbox.lock")
	if lockErr == nil && strings.Contains(lockShow, "0.0.5") {
		t.Fatalf("banked commit carries the drifted devbox.lock:\n%s", lockShow)
	}
}

// The run's real shape on a repository that IGNORES the mirror — this
// repository's own `**/.claude/` — laid untracked by the engine all the
// same: git stages the work but exits 1 for the exclusion naming an ignored
// path, and a wip bank reading that exit as failure leaves the run's work
// unbanked in a preserved worktree (#1558, measured on the #1464 dogfood).
// The bank must succeed here exactly as it does when nothing is ignored;
// the scratch rule beside it holds the claim that git reports no wildcard
// exclusion, ignored scratch or not.
func TestFinalizeWorktree_WipBankSucceedsWhenTheMirrorIsIgnored(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	addCommit(t, wt, ".gitignore", "**/.claude/\n.iterion-script-*\n", "ignore the mirror and the scratch, as this repository does")
	addCommit(t, wt, "devbox.lock", "plugin_version: 0.0.4\n", "baseline with a lock")
	if err := os.MkdirAll(filepath.Join(wt, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".claude", "skills", "mirrored.md"), "iterion wrote this\n")
	writeFile(t, filepath.Join(wt, ".iterion-script-probe.sh"), "iterion scratch\n")
	writeFile(t, filepath.Join(wt, "devbox.lock"), "plugin_version: 0.0.5\n")
	writeFile(t, filepath.Join(wt, "real.md"), "the pass's work\n")

	var logBuf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &logBuf)
	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "wip-ignored-mirror", runID: "run_i", autoMerge: true, mergeStrategy: "merge"}, logger)

	if !res.WipBanked || res.PreserveWorktree {
		t.Fatalf("the wip bank must succeed with the mirror ignored, got %+v\nlog:\n%s", res, logBuf.String())
	}
	show := gittest.Run(t, repo, "show", "--stat", "--format=%s", res.FinalCommit)
	if !strings.Contains(show, "real.md") {
		t.Fatalf("banked commit missing the run's work:\n%s", show)
	}
	for _, noise := range []string{".claude", ".iterion-script-probe"} {
		if strings.Contains(show, noise) {
			t.Fatalf("banked commit carries the tree noise %q:\n%s", noise, show)
		}
	}
	lockShow, lockErr := gittest.Try(repo, "show", res.FinalCommit+":devbox.lock")
	if lockErr == nil && strings.Contains(lockShow, "0.0.5") {
		t.Fatalf("banked commit carries the drifted devbox.lock:\n%s", lockShow)
	}
	// The ignored mirror and scratch never reach the porcelain; the drifted
	// lock does, and is still NAMED as set aside.
	for _, want := range []string{"tree noise set aside", "devbox.lock"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Fatalf("finalize log misses %q:\n%s", want, logBuf.String())
		}
	}
}

// The set-aside list the operator reads is what the bank did NOT carry, not
// what the classification calls noise: a file tracked under the ignored
// mirror rides the wip commit (the staging floor), and naming it as set
// aside would tell the operator the opposite of what happened. The drifted
// lock, unstaged, is still named; the untracked mirror file never reaches
// the porcelain. (#1571 tracks the classification itself.)
func TestFinalizeWorktree_WipBankNamesOnlyTheNoiseItSetAside(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	if err := os.MkdirAll(filepath.Join(wt, ".claude", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, wt, filepath.Join(".claude", "rules", "tracked.md"), "committed before the rule\n", "track a rule under the mirror")
	addCommit(t, wt, ".gitignore", "**/.claude/\n", "then ignore the mirror")
	addCommit(t, wt, "devbox.lock", "plugin_version: 0.0.4\n", "baseline with a lock")
	writeFile(t, filepath.Join(wt, ".claude", "rules", "tracked.md"), "modified during the run\n")
	if err := os.MkdirAll(filepath.Join(wt, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".claude", "skills", "mirrored.md"), "iterion wrote this\n")
	writeFile(t, filepath.Join(wt, "devbox.lock"), "plugin_version: 0.0.5\n")
	writeFile(t, filepath.Join(wt, "real.md"), "the pass's work\n")

	var logBuf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &logBuf)
	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "wip-set-aside", runID: "run_s", autoMerge: true, mergeStrategy: "merge"}, logger)

	if !res.WipBanked || res.PreserveWorktree {
		t.Fatalf("the wip bank must succeed, got %+v\nlog:\n%s", res, logBuf.String())
	}
	show := gittest.Run(t, repo, "show", "--stat", "--format=%s", res.FinalCommit)
	for _, want := range []string{"real.md", ".claude/rules/tracked.md"} {
		if !strings.Contains(show, want) {
			t.Fatalf("banked commit misses %q:\n%s", want, show)
		}
	}
	if strings.Contains(show, "mirrored.md") {
		t.Fatalf("banked commit carries the untracked mirror file:\n%s", show)
	}
	log := logBuf.String()
	if !strings.Contains(log, "tree noise set aside: devbox.lock") {
		t.Fatalf("the set-aside list must name exactly the unstaged lock:\n%s", log)
	}
	if strings.Contains(log, "tracked.md") {
		t.Fatalf("the tracked mirror file rode the bank yet is named as set aside:\n%s", log)
	}
}

// The guarantee behind the set-aside list, held on names git QUOTES in its
// porcelain (core.quotePath: a non-ASCII byte, a quote, a backslash): what
// the log names as set aside is exactly the noise the banked commit does
// not carry. The porcelain's C-quoted form and the raw bytes `-z` and the
// commit carry must decode to the same path, or a file that rode the bank
// is reported as set aside — the inversion the list exists to prevent.
func TestFinalizeWorktree_WipBankSetAsideIsExactlyWhatItDidNotCarry(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	if err := os.MkdirAll(filepath.Join(wt, ".claude", "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	quoted := []string{"café accent.md", `quo"te.md`, `back\slash.md`}
	for _, name := range quoted {
		addCommit(t, wt, filepath.Join(".claude", "rules", name), "committed before the rule\n", "track "+name)
	}
	addCommit(t, wt, ".gitignore", "**/.claude/\n", "then ignore the mirror")
	addCommit(t, wt, "devbox.lock", "plugin_version: 0.0.4\n", "baseline with a lock")
	for _, name := range quoted {
		writeFile(t, filepath.Join(wt, ".claude", "rules", name), "modified during the run\n")
	}
	writeFile(t, filepath.Join(wt, "devbox.lock"), "plugin_version: 0.0.5\n")
	writeFile(t, filepath.Join(wt, "réel.md"), "the pass's work\n")
	// Read through the production helper: gittest.Run trims the output,
	// and a trimmed porcelain loses its first line's status column.
	porcelain, err := runGit(wt, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(porcelain, `"`) {
		t.Fatalf("the fixture must make git quote a path, porcelain:\n%s", porcelain)
	}

	var logBuf bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &logBuf)
	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "wip-quoted", runID: "run_q", autoMerge: true, mergeStrategy: "merge"}, logger)
	if !res.WipBanked || res.PreserveWorktree {
		t.Fatalf("the wip bank must succeed, got %+v\nlog:\n%s", res, logBuf.String())
	}

	carried := map[string]bool{}
	for _, f := range strings.Split(gittest.Run(t, repo, "show", "--name-only", "--format=", "-z", res.FinalCommit), "\x00") {
		if f = strings.TrimSpace(f); f != "" {
			carried[f] = true
		}
	}
	for _, name := range quoted {
		if !carried[".claude/rules/"+name] {
			t.Fatalf("the tracked mirror file %q did not ride the bank; carried: %v", name, carried)
		}
	}
	if !carried["réel.md"] {
		t.Fatalf("the run's work did not ride the bank; carried: %v", carried)
	}

	log := logBuf.String()
	i := strings.Index(log, "tree noise set aside: ")
	if i < 0 {
		t.Fatalf("no set-aside list in the log:\n%s", log)
	}
	listed := strings.TrimSpace(strings.SplitN(log[i+len("tree noise set aside: "):], "\n", 2)[0])
	// The concrete expectation first — a broken decoder would list the
	// quoted names on BOTH sides of the property below and hide there.
	if listed != "devbox.lock" {
		t.Fatalf("set aside = %q, want exactly the unstaged lock\nlog:\n%s", listed, log)
	}
	// Then the property the list promises: nothing listed rode the bank,
	// and every noise path the bank left out is listed.
	for _, p := range strings.Split(listed, ", ") {
		if carried[p] {
			t.Fatalf("%q is named as set aside and rode the bank", p)
		}
	}
	for _, p := range noisePaths(porcelain) {
		if !carried[p] && !strings.Contains(listed, p) {
			t.Fatalf("noise path %q left out of the bank is missing from the set-aside list %q", p, listed)
		}
	}
}
