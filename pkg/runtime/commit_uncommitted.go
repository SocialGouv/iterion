package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/treenoise"
)

// CommitUncommittedAndFinalize stages every change in a run's worktree
// (`git add -A`), commits with the operator-supplied message, then
// re-runs the worktree finalization so FinalCommit / FinalBranch land
// on the run record. Existing /merge UX takes over from there.
//
// Use case: bots that finish a work session without committing (e.g.
// whole_improve_loop's reviewer/fixer pairs leave a dirty workdir
// without a prepare_commit step). The operator can salvage the work
// from the run page instead of having to commit by hand in the
// workspace directory.
//
// Idempotence:
//   - bails when the run isn't a worktree run (nothing to finalize).
//   - bails when FinalBranch is already set (the run was already
//     finalized; the operator should use /merge instead).
//   - bails when the workdir is clean (no diff to commit).
//
// Safety:
//   - the message is operator-supplied; the runtime does no further
//     transformation beyond passing it to `git commit -m`.
//   - `git add -A` honors the project's .gitignore — untracked
//     sandbox runtime artifacts that the project has ignored stay
//     out. Untracked files NOT in .gitignore (e.g. the bot's new
//     ADR) are committed; surface this in the studio so the
//     operator can adjust .gitignore beforehand if needed.
func CommitUncommittedAndFinalize(
	ctx context.Context,
	st store.RunStore,
	r *store.Run,
	message string,
	logger *iterlog.Logger,
) error {
	if r == nil {
		return fmt.Errorf("runtime: commit-uncommitted: nil run")
	}
	if !r.Worktree || r.WorkDir == "" {
		return fmt.Errorf("runtime: commit-uncommitted: run %q is not a worktree run", r.ID)
	}
	if r.FinalBranch != "" || r.FinalCommit != "" {
		return fmt.Errorf("runtime: commit-uncommitted: run %q is already finalized (FinalBranch=%q, FinalCommit=%q) — use /merge instead", r.ID, r.FinalBranch, r.FinalCommit)
	}
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("runtime: commit-uncommitted: commit message is required")
	}

	porcelain, err := runGit(r.WorkDir, "status", "--porcelain", "-z")
	if err != nil {
		return fmt.Errorf("runtime: commit-uncommitted: probe workdir: %w (output: %s)", err, strings.TrimSpace(porcelain))
	}
	// The probe agrees with THIS gesture's staging (verdict 5, R138690): a
	// worktree whose only dirt is a tracked-and-modified devbox.lock is a
	// lock-only bump the merge-destined commit carries — refusing it here
	// left the studio's salvage action no path to bank it. Only the mirror
	// is set aside; the wip bank keeps the fuller IsNoise probe.
	work := commitWorkPaths(r.WorkDir, porcelain)
	if len(work) == 0 {
		if strings.TrimSpace(porcelain) != "" {
			return fmt.Errorf("runtime: commit-uncommitted: workdir %q is dirty with tree noise only — nothing of the run's to commit (see git status)", r.WorkDir)
		}
		return fmt.Errorf("runtime: commit-uncommitted: workdir %q has no changes to commit", r.WorkDir)
	}

	if err := stageCommitWork(r.WorkDir, work); err != nil {
		return fmt.Errorf("runtime: commit-uncommitted: git add: %w", err)
	}
	if out, err := gitCommitMessage(r.WorkDir, message); err != nil {
		return fmt.Errorf("runtime: commit-uncommitted: git commit: %w (output: %s)", err, strings.TrimSpace(out))
	}
	if logger != nil {
		logger.Info("runtime: committed uncommitted workdir for run %s", r.ID)
	}

	return RecoverFinalize(ctx, st, r, logger)
}

// stageWorkArgs stages the whole tree EXCEPT the canonical tree noise
// (pkg/treenoise): the `.claude/` mirror and a drifted devbox.lock are not
// the pass's work, and the WIP BANK's staging gesture agrees with the
// cleanliness probe — never merged, the lock is derivable from devbox.json.
// The operator-initiated commit-and-finalize deliberately disagrees: its
// commit is merge-destined, so it stages a tracked-and-modified lock (see
// commitStageArgs). dir is the work tree the gesture runs in: an exclusion
// git already ignores there is left unspelled (stagingExclusions). This is
// the gesture when NO work path sits under the mirror; when one does,
// stageTreeWork handles the mirror path by path (#1571).
func stageWorkArgs(dir string) []string {
	args := []string{"add", "-A", "--", ":/"}
	return append(args, stagingExclusions(dir, treenoise.Entries)...)
}

// commitStageArgs stages the tree for the OPERATOR-initiated commit-and-
// finalize: the `.claude/` mirror stays excluded (iterion wrote it, the run
// did not — deliverables under it are staged by name), but a
// tracked-and-modified devbox.lock is STAGED here, not dropped: this
// commit is merge-destined, and a dependency bot's lock bump is half its
// deliverable — dropping it would merge devbox.json without its
// resolution and destroy the bump with the worktree (verdict 3, R5478b3).
// The wip bank keeps the fuller exclusion: it is never merged, and the
// lock is derivable from devbox.json. dir is the work tree the gesture runs
// in: where the mirror is already ignored there is no exclusion to spell
// (stagingExclusions).
func commitStageArgs(dir string) []string {
	args := []string{"add", "-A", "--", ":/"}
	return append(args, stagingExclusions(dir, []treenoise.Entry{treenoise.MirrorEntry()})...)
}

// stageRunWork is the wip bank's staging gesture: stageWorkArgs when no
// work path sits under the mirror, the path-by-path form of stageTreeWork
// when one does.
func stageRunWork(dir string, workPaths []string) error {
	return stageTreeWork(dir, treenoise.Entries, workPaths)
}

// stageCommitWork is the operator-initiated commit-and-finalize's staging
// gesture: commitStageArgs' mirror-only exclusion list, the same
// path-by-path mirror handling when a tracked file under it is work.
func stageCommitWork(dir string, workPaths []string) error {
	return stageTreeWork(dir, []treenoise.Entry{treenoise.MirrorEntry()}, workPaths)
}

// stageTreeWork stages the tree with entries as its noise exclusions. When
// a work path sits UNDER the mirror (#1571 — a tracked `.claude/**` file
// the run edited), the one exclusion `:(exclude,top).claude` can no longer
// express the gesture: it would keep the run's edit out with the mirror.
// The mirror is then handled path by path — the tree is staged without its
// exclusion (the mirror's untracked files and untouched tracked rewrites
// come along), the whole mirror is unstaged, and exactly the work paths are
// re-staged. A pathspec the porcelain printed is quoted with the literal
// magic so a space, a quote or a glob metacharacter in a file name cannot
// change what the re-stage names. Known limit: a RENAME whose source sits
// under the mirror is staged by its destination only — the source's
// deletion stays in the bank's tree, preserving rather than losing it.
func stageTreeWork(dir string, entries []treenoise.Entry, workPaths []string) error {
	var mirrorWork []string
	for _, p := range workPaths {
		if !treenoise.IsMirror(p) {
			continue
		}
		// The re-add below must be able to NAME the path: on disk, or in
		// HEAD (a deletion is staged by adding its path). A staged-then-
		// deleted ghost is in neither once the main `git add -A` has staged
		// its deletion, and a pathspec matching nothing fails the whole
		// gesture (exit 128, measured on git 2.55) — the probe already
		// excludes those records; this filter is the belt to its braces.
		if !pathOnDiskOrInHead(dir, p) {
			continue
		}
		mirrorWork = append(mirrorWork, p)
	}
	if len(mirrorWork) == 0 {
		args := []string{"add", "-A", "--", ":/"}
		return runGitInDir(dir, append(args, stagingExclusions(dir, entries)...)...)
	}
	kept := make([]treenoise.Entry, 0, len(entries))
	for _, e := range entries {
		if e != treenoise.MirrorEntry() {
			kept = append(kept, e)
		}
	}
	args := []string{"add", "-A", "--", ":/"}
	if err := runGitInDir(dir, append(args, stagingExclusions(dir, kept)...)...); err != nil {
		return err
	}
	if err := runGitInDir(dir, "reset", "-q", "--", ":(top)"+treenoise.MirrorPath); err != nil {
		return err
	}
	// `-f`: on a repository that IGNORES the mirror (this repository's own
	// `**/.claude/`), git's untracked walk classifies the `.claude` directory
	// on the rules alone and refuses ANY pathspec descending into it — exit 1
	// with the index right, the #1558 shape one level down.
	//
	// One shape the re-add must NOT take the add path for: a tracked path
	// replaced ON DISK by a directory (`.claude` itself or
	// `.claude/settings.json` deleted, a directory of the same name planted).
	// `git add` on that pathspec RECURSES and sweeps whatever untracked
	// mirror files the directory holds into the index — with `-f` punching
	// through the ignore rules, the mirror's noise lands in a merge-destined
	// commit (measured on git 2.55). The work the probe saw is the tracked
	// path's DISAPPEARANCE, so the gesture is the deletion and nothing more:
	// `git rm --cached` produces exactly `D <path>` and leaves the
	// replacement directory untracked.
	var add, remove []string
	for _, p := range mirrorWork {
		if info, err := os.Lstat(filepath.Join(dir, p)); err == nil && info.IsDir() {
			remove = append(remove, ":(top,literal)"+p)
			continue
		}
		add = append(add, ":(top,literal)"+p)
	}
	if len(remove) > 0 {
		if err := runGitInDir(dir, append([]string{"rm", "-q", "--cached", "--ignore-unmatch", "--"}, remove...)...); err != nil {
			return err
		}
	}
	if len(add) == 0 {
		return nil
	}
	return runGitInDir(dir, append([]string{"add", "-f", "-A", "--"}, add...)...)
}

// pathOnDiskOrInHead reports whether a staging gesture can name the
// repository-top-relative path p: present in the work tree, or present in
// HEAD (a tracked deletion is staged by adding its path).
func pathOnDiskOrInHead(dir, p string) bool {
	if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
		return true
	}
	_, err := runGit(dir, "cat-file", "-e", "HEAD:"+p)
	return err == nil
}

// stagingExclusions renders entries as the exclusion pathspecs a staging
// gesture may spell in dir. `git add` refuses an exclusion — exits 1 with
// "The following paths are ignored by one of your .gitignore files", the
// index correctly staged — when its untracked walk classified the path the
// pathspec's literal part spells as ignored: present in the work tree,
// covered by the ignore rules, and not skipped as indexed. A gesture
// reading that exit as failure leaves the run's work unbanked (#1558, on
// this repository's own `**/.claude/`). So every entry's literal path is
// asked the walk's own question first (gitAddRefusesExclusionOf), and an
// exclusion git would refuse is left unspelled. The omission excludes
// nothing git does not already keep out, with one floor: tracked files
// under an ignored DIRECTORY — the walk classifies a directory on the
// rules alone — ride the commit as any tracked file does, the gesture's
// behaviour before the exclusions existed. The probe agrees since #1571:
// a tracked mirror path the mirror did not leave unchanged is work
// (trackedMirrorWork), and stageTreeWork then stages it path by path. The
// literal part of a Prefix
// entry is a path like any other: a file named exactly `.iterion-script-`
// and ignored trips the wildcard exclusion too, and the scratch files
// beside it then ride the wip bank — never merged — rather than the run's
// work going unbanked. Where git cannot answer (dir is no work tree) every
// exclusion is spelled and the gesture reports the real failure itself.
func stagingExclusions(dir string, entries []treenoise.Entry) []string {
	top, inWorkTree := gitTopLevel(dir)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if inWorkTree && gitAddRefusesExclusionOf(dir, top, e.Path) {
			continue
		}
		out = append(out, e.Pathspec())
	}
	return out
}

// gitTopLevel is the root of the work tree dir belongs to; ok is false
// when git cannot say — dir absent, not inside a work tree, a bare
// repository.
func gitTopLevel(dir string) (top string, ok bool) {
	cmd, cancel := gitCmd("-C", dir, "rev-parse", "--show-toplevel")
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	top = strings.TrimSpace(string(out))
	return top, top != ""
}

// gitAddRefusesExclusionOf reports whether `git add`'s untracked walk, run
// in dir, classifies the repository-top path rel as ignored — the condition
// under which an exclusion pathspec naming it makes the gesture exit 1.
// The walk skips an indexed FILE before consulting the ignore rules but
// classifies a DIRECTORY on the rules alone, tracked content or not; the
// probe asks the same way — check-ignore consulting the index for a file
// (a tracked file is never ignored), the rules alone (--no-index) for a
// directory — about the top-anchored path (`:(top)`), the one the
// exclusion names, whatever dir is. Absent from the work tree, nothing is
// walked and nothing refused. Any answer but "ignored" (exit 0) — git
// unable to answer included — keeps the exclusion.
func gitAddRefusesExclusionOf(dir, top, rel string) bool {
	info, err := os.Lstat(filepath.Join(top, rel))
	if err != nil {
		return false
	}
	args := []string{"-C", dir, "check-ignore", "-q"}
	if info.IsDir() {
		args = append(args, "--no-index")
	}
	cmd, cancel := gitCmd(append(args, "--", ":(top)"+rel)...)
	defer cancel()
	return cmd.Run() == nil
}

// commitWorkPaths returns the porcelain entries the OPERATOR-initiated
// commit-and-finalize would stage: everything except the engine's own
// mirror — a tracked-and-modified devbox.lock IS the dependency work half
// the merge-destined commit carries (verdict 3, R5478b3). The probe must
// agree with this gesture, not with the wip bank's (verdict 5, R138690).
// Among mirror paths, a TRACKED file the mirror did not write or no longer
// matches is the run's work (trackedMirrorWork, #1571). dir is the work
// tree the mirror manifest is read from.
func commitWorkPaths(dir, porcelain string) []string {
	var out []string
	for _, rec := range porcelainRecords(porcelain) {
		if !treenoise.IsMirror(rec.Path) || trackedMirrorWork(dir, rec) {
			out = append(out, rec.Path)
		}
	}
	return out
}

// porcelainRecords normalizes `git status --porcelain -z` output into its
// records. The NUL-terminated form is the only porcelain this package
// reads: a rename carries destination and source as two fields (destination
// first), so a source literally named `x -> y.md` can no longer be cut at
// the wrong " -> ", and no path arrives C-quoted. The one record-walker is
// gitlib.ParseStatusPorcelainZ, shared with worktreepool (#1577).
func porcelainRecords(porcelain string) []gitlib.StatusRecord {
	records, err := gitlib.ParseStatusPorcelainZ(porcelain)
	if err != nil {
		// An unparseable status must never read as a clean tree: hand the
		// probes one opaque record whose path no noise entry matches and
		// whose status reads as tracked, so every caller takes its "dirty"
		// branch.
		if porcelain == "" {
			return nil
		}
		return []gitlib.StatusRecord{{Status: " M", Path: porcelain}}
	}
	return records
}

// porcelainPaths normalizes `git status --porcelain -z` output into the
// paths it reports — the record view (porcelainRecords) reduced to each
// record's path, the rename's destination.
func porcelainPaths(porcelain string) []string {
	records := porcelainRecords(porcelain)
	out := make([]string, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.Path)
	}
	return out
}

// trackedMirrorWork reports whether a porcelain record the path-only noise
// rules set aside is in fact the run's work: a TRACKED file under the
// `.claude` mirror (an untracked `??` or ignored `!!` path keeps the
// path-only classification, #1571's scope) whose current bytes are not what
// the mirror left there — the manifest distinction of
// mirrorTrackedEditIsWork.
//
// Two classes never reach the manifest:
//
//   - anything under an `.iterion-managed/` directory: the engine's own
//     bookkeeping (markers, sidecars, the manifest itself), which it wipes,
//     prunes and rewrites every pass. A bot that commits mid-run with
//     `git add -A` TRACKS it, and without the carve-out the next pass's
//     bookkeeping churn reads as work — a converged run wip-banked, and the
//     tracking propagated into the wip commit (the #1364 shape, one level
//     down). The engine already claims those directories everywhere else.
//   - a staged-then-deleted ghost (`AD` / `RD` / `CD`: added, renamed or
//     copied into the index, then removed from the worktree): the path is
//     in NEITHER HEAD nor the worktree, so there is nothing bankable at it
//     — and naming it in stageTreeWork's re-add fails the whole gesture
//     with "pathspec did not match" once the main `git add -A` has staged
//     the ghost's deletion (measured, git 2.55).
func trackedMirrorWork(dir string, rec gitlib.StatusRecord) bool {
	if !treenoise.IsMirror(rec.Path) || rec.Status == "??" || rec.Status == "!!" {
		return false
	}
	if strings.Contains(rec.Path, "/"+bundleMirrorMarkerDir+"/") {
		return false
	}
	if rec.Status[1] == 'D' && (rec.Status[0] == 'A' || rec.Status[0] == 'R' || rec.Status[0] == 'C') {
		return false
	}
	return mirrorTrackedEditIsWork(dir, rec.Path)
}

// runOutputPaths returns the porcelain entries that stand for work the RUN
// produced, dropping the scaffolding iterion mirrored in itself. dir is the
// work tree the mirror manifest is read from.
//
// Counting that mirror as run output has a cost that is not cosmetic. Finalize
// reads a dirty tree as "the bot left work uncommitted" and banks it as a wip
// commit — and a wip-banked HEAD is NEVER merged, by design. So a lot whose
// gate CONVERGED does not land, and the cause is iterion's own bundle
// scaffolding sitting untracked beside it. Measured on a run that converged
// and whose entire wip bank was 638 lines of mirrored skill files.
//
// `.git/info/exclude` is not the place to fix it: git reads that file from the
// COMMON dir, so a linked worktree cannot carry its own, and writing there
// would silently start ignoring `.claude/` in the operator's checkout too.
//
// A repository that TRACKS files under `.claude/` keeps what matters (#1571):
// the path-only rules cannot tell the mirror's rewrite of a tracked file
// from the agent's edit of one, but the mirror can — it records what it
// wrote (mirror_manifest.go), and a tracked mirror path is work unless the
// mirror wrote it AND left it unchanged since. Without that distinction the
// run whose ONLY change is a tracked `.claude/settings.json` edit banked
// nothing, and the worktree removal destroyed the deliverable in silence.
// The exclusion is scoped to this decision and nothing else: it never
// removes a file, and never touches what the run committed.
func runOutputPaths(dir, porcelain string) []string {
	var out []string
	for _, rec := range porcelainRecords(porcelain) {
		if !treenoise.IsNoise(rec.Path) || trackedMirrorWork(dir, rec) {
			out = append(out, rec.Path)
		}
	}
	return out
}

// noisePaths returns the porcelain paths the noise list sets aside — the
// complement of runOutputPaths. The wip bank's warn line names them, so a
// banked commit never silently swallows what it excluded (verdict 8).
func noisePaths(dir, porcelain string) []string {
	work := map[string]bool{}
	for _, p := range runOutputPaths(dir, porcelain) {
		work[p] = true
	}
	var out []string
	for _, p := range porcelainPaths(porcelain) {
		if !work[p] {
			out = append(out, p)
		}
	}
	return out
}

// wipSetAside is the tree noise the wip bank did NOT carry: the porcelain's
// noise paths minus what the staging gesture put in the index, read between
// `git add` and the commit. A file tracked under an ignored mirror rides
// the bank (stagingExclusions, its floor) while the classification still
// calls it noise (#1571); the operator reading the storage branch is told
// what was set aside, never the opposite of what happened. Both sides are
// the paths' raw bytes: `-z` prints them as the index and the file system
// carry them.
func wipSetAside(dir, porcelain string) ([]string, error) {
	staged, err := runGit(dir, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return nil, fmt.Errorf("read the staged set: %w (output: %s)", err, strings.TrimSpace(staged))
	}
	carried := map[string]bool{}
	for _, p := range strings.Split(staged, "\x00") {
		if p != "" {
			carried[p] = true
		}
	}
	var out []string
	for _, p := range noisePaths(dir, porcelain) {
		if !carried[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

func runGitInDir(workdir string, args ...string) error {
	out, err := runGit(workdir, args...)
	if err != nil {
		return fmt.Errorf("%w (output: %s)", err, strings.TrimSpace(out))
	}
	return nil
}
