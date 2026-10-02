package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// #1571: `git status` cannot tell the mirror's rewrite of a TRACKED file
// under `.claude/` from the run's edit of one — the path-only noise rules
// read both as mirror noise, so a run whose ONLY change was the edit banked
// nothing and the worktree removal destroyed the deliverable in silence.
// The mirror knows which one it was: it records every destination with the
// content hash it laid (mirror_manifest.go), and the probes treat as noise,
// among tracked mirror paths, only what the mirror wrote AND left unchanged
// since. These tests run the REAL mirror against throwaway repositories and
// read the probes through the real porcelain.

// mirrorKeepSkill mirrors a bundle whose keep.md goes v1 → v2 over two
// passes against ws, whose repository TRACKS .claude/skills/keep/SKILL.md
// at v1. Pass 1 adopts the tracked file (identical content → UpToDate, the
// manifest records it), pass 2 refreshes it to v2 — the engine's own
// rewrite of a tracked file, the #1364 shape.
func mirrorKeepSkill(t *testing.T, ws, skillsSrc string) {
	t.Helper()
	if _, err := mirrorBundleSkills(ws, &bundle.Bundle{SkillsDir: skillsSrc}, nil); err != nil {
		t.Fatalf("mirror pass: %v", err)
	}
}

func keepSkillRepo(t *testing.T) (ws, skillsSrc string) {
	t.Helper()
	ws = t.TempDir()
	gittest.InitRepo(t, ws)
	if err := os.MkdirAll(filepath.Join(ws, ".claude", "skills", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, ws, filepath.Join(".claude", "skills", "keep", "SKILL.md"), "# keep v1\n", "track the mirror target")
	skillsSrc = t.TempDir()
	writeFile(t, filepath.Join(skillsSrc, "keep.md"), "# keep v1\n")
	mirrorKeepSkill(t, ws, skillsSrc) // pass 1: adopt the tracked file
	writeFile(t, filepath.Join(skillsSrc, "keep.md"), "# keep v2\n")
	mirrorKeepSkill(t, ws, skillsSrc) // pass 2: refresh it to v2 — the mirror's own rewrite
	return ws, skillsSrc
}

func porcelainOf(t *testing.T, dir string) string {
	t.Helper()
	porcelain, err := runGit(dir, "status", "--porcelain", "-z")
	if err != nil {
		t.Fatal(err)
	}
	return porcelain
}

// The ticket's repro, verbatim: a throwaway repository, a TRACKED file
// under `.claude/` modified, no manifest in play (the mirror never wrote
// that file) — the probes must read it as the run's work. Pre-fix every
// assertion below came back empty, which is exactly how the deliverable was
// destroyed. Mutation: drop the trackedMirrorWork distinction and the
// path-only rules return nothing — this test goes red.
func TestRunOutputPathsATrackedMirrorEditIsWork(t *testing.T) {
	ws := t.TempDir()
	gittest.InitRepo(t, ws)
	if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, ws, filepath.Join(".claude", "settings.json"), "{\"permissions\":{}}\n", "track the repo's own settings")
	writeFile(t, filepath.Join(ws, ".claude", "settings.json"), "{\"permissions\":{\"allow\":[]}}\n")

	porcelain := porcelainOf(t, ws)
	if !strings.HasPrefix(porcelain, " M .claude/settings.json\x00") {
		t.Fatalf("the fixture's porcelain is not the ticket's ` M .claude/settings.json`: %q", porcelain)
	}
	if got := runOutputPaths(ws, porcelain); len(got) != 1 || got[0] != ".claude/settings.json" {
		t.Fatalf("runOutputPaths = %q, want [.claude/settings.json] — the run's only edit must bank, not be set aside", got)
	}
	if got := commitWorkPaths(ws, porcelain); len(got) != 1 || got[0] != ".claude/settings.json" {
		t.Fatalf("commitWorkPaths = %q, want [.claude/settings.json] — the operator's commit-and-finalize agrees", got)
	}
	if got := noisePaths(ws, porcelain); len(got) != 0 {
		t.Fatalf("noisePaths = %q, want empty — the edit is work, nothing was set aside", got)
	}
}

// The inverse, the #1364 guard the ticket says "tracked ⇒ work" would
// break: the mirror REFRESHED a tracked file this pass (bundle v1 → v2),
// so the porcelain reads ` M .claude/skills/keep/SKILL.md` — the engine's
// own write, not the run's. The manifest names it with the hash it laid,
// the content still matches, and the probes stay quiet. Mutation: classify
// every tracked mirror path as work and this test goes red — a converged
// run wip-banked by iterion's own scaffolding.
func TestMirrorRewriteOfATrackedFileStaysNoise(t *testing.T) {
	ws, _ := keepSkillRepo(t)

	porcelain := porcelainOf(t, ws)
	if !strings.Contains(porcelain, " M .claude/skills/keep/SKILL.md\x00") {
		t.Fatalf("the fixture must show the mirror's own rewrite as modified: %q", porcelain)
	}
	if got := runOutputPaths(ws, porcelain); len(got) != 0 {
		t.Fatalf("runOutputPaths = %q, want empty — the mirror's rewrite of a tracked file is not the run's work (#1364)", got)
	}
	if got := commitWorkPaths(ws, porcelain); len(got) != 0 {
		t.Fatalf("commitWorkPaths = %q, want empty — the operator's probe agrees", got)
	}
}

// The same refreshed file, edited by the agent AFTER the mirror laid it:
// the content no longer hashes to what the manifest recorded, and the
// distinction flips — the edit is work.
func TestAgentEditAfterTheMirrorIsWork(t *testing.T) {
	ws, _ := keepSkillRepo(t)
	writeFile(t, filepath.Join(ws, ".claude", "skills", "keep", "SKILL.md"), "# keep v2 + the agent's touch\n")

	if got := runOutputPaths(ws, porcelainOf(t, ws)); len(got) != 1 || got[0] != ".claude/skills/keep/SKILL.md" {
		t.Fatalf("runOutputPaths = %q, want [.claude/skills/keep/SKILL.md] — an edit after the mirror's write is work", got)
	}
}

// Two deletion shapes, opposite classifications: the orphan PRUNER removing
// a tracked file it wrote is the engine's own act (tombstoned — a converged
// run must not bank it), while the AGENT deleting the same file is the
// run's work (banked). The pruner runs after ClearMirroredTierMarkers so no
// marker carries a fresh tier — every mirrored destination is an orphan
// candidate, and the tracked one still matches its marker.
func TestPrunerDeletionOfATrackedMirrorFileIsNoise(t *testing.T) {
	ws, _ := keepSkillRepo(t)
	ClearMirroredTierMarkers(ws, nil)
	pruneWorkspaceMirror(ws, true, nil)
	if _, err := os.Lstat(filepath.Join(ws, ".claude", "skills", "keep", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("the pruner did not remove the orphan: %v", err)
	}

	porcelain := porcelainOf(t, ws)
	if !strings.Contains(porcelain, " D .claude/skills/keep/SKILL.md\x00") {
		t.Fatalf("the fixture must show the pruned tracked file as deleted: %q", porcelain)
	}
	if got := runOutputPaths(ws, porcelain); len(got) != 0 {
		t.Fatalf("runOutputPaths = %q, want empty — the pruner's own deletion is not the run's work", got)
	}
}

func TestAgentDeletionOfATrackedMirrorFileIsWork(t *testing.T) {
	ws, _ := keepSkillRepo(t)
	if err := os.Remove(filepath.Join(ws, ".claude", "skills", "keep", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	porcelain := porcelainOf(t, ws)
	if !strings.Contains(porcelain, " D .claude/skills/keep/SKILL.md\x00") {
		t.Fatalf("the fixture must show the deleted tracked file: %q", porcelain)
	}
	if got := runOutputPaths(ws, porcelain); len(got) != 1 || got[0] != ".claude/skills/keep/SKILL.md" {
		t.Fatalf("runOutputPaths = %q, want [.claude/skills/keep/SKILL.md] — the agent's deletion is the run's work", got)
	}
}

// End to end, the ticket's data loss: the run's ONLY change is a tracked
// `.claude/settings.json` edit. Pre-fix finalize logged "nothing to bank"
// and the worktree removal destroyed the edit; now the wip bank carries it,
// and the untracked mirror file beside it still stays out.
func TestFinalizeWorktree_WipBankStagesTheTrackedMirrorWork(t *testing.T) {
	repo, _ := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, wt, filepath.Join(".claude", "settings.json"), "{\"permissions\":{}}\n", "track the repo's own settings")
	originalTip := gittest.Run(t, wt, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(wt, ".claude", "settings.json"), "{\"permissions\":{\"allow\":[]}}\n")
	if err := os.MkdirAll(filepath.Join(wt, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".claude", "skills", "mirrored.md"), "iterion wrote this\n")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "wip-1571", runID: "run_m", autoMerge: true, mergeStrategy: "merge"}, nil)

	if !res.WipBanked || res.PreserveWorktree {
		t.Fatalf("the run's only edit must bank, got %+v", res)
	}
	show := gittest.Run(t, repo, "show", "--stat", "--format=%s", res.FinalCommit)
	if !strings.Contains(show, ".claude/settings.json") {
		t.Fatalf("banked commit missing the tracked mirror edit:\n%s", show)
	}
	if strings.Contains(show, "mirrored.md") {
		t.Fatalf("banked commit carries the untracked mirror file:\n%s", show)
	}
}

// The #1364 guard end to end: on a repository TRACKING its mirror targets,
// a run that changed nothing but what the mirror itself refreshed has
// nothing of its own to bank — finalize produces no wip commit and no
// branch.
func TestFinalizeWorktree_ConvergedRunOnARepoTrackingMirrorTargetsDoesNotBank(t *testing.T) {
	repo, _ := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	if err := os.MkdirAll(filepath.Join(wt, ".claude", "skills", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, wt, filepath.Join(".claude", "skills", "keep", "SKILL.md"), "# keep v1\n", "track the mirror target")
	originalTip := gittest.Run(t, wt, "rev-parse", "HEAD")

	skillsSrc := t.TempDir()
	writeFile(t, filepath.Join(skillsSrc, "keep.md"), "# keep v1\n")
	mirrorKeepSkill(t, wt, skillsSrc)
	writeFile(t, filepath.Join(skillsSrc, "keep.md"), "# keep v2\n")
	mirrorKeepSkill(t, wt, skillsSrc)

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "converged-1364", runID: "run_c", autoMerge: true, mergeStrategy: "merge"}, nil)

	if res.WipBanked || res.FinalCommit != "" || res.PreserveWorktree {
		t.Fatalf("a converged run must not bank the mirror's own rewrite, got %+v", res)
	}
}

// A bot that commits mid-run with `git add -A` TRACKS the engine's own
// bookkeeping (`.iterion-managed/` markers, sidecars, the manifest). The
// next mirror pass rewrites it, and without the carve-out that churn reads
// as the run's work: a converged run wip-banked, and the bookkeeping
// re-staged into the wip commit — the tracking propagated. The engine
// claims those directories everywhere (wipe, prune, manifest); the
// classifier does too, tracked or not.
func TestTrackedIterionManagedBookkeepingStaysNoise(t *testing.T) {
	ws, skillsSrc := keepSkillRepo(t)
	// The mid-run commit: everything on disk becomes tracked, the
	// `.iterion-managed/` bookkeeping included.
	gittest.Run(t, ws, "add", "-A")
	gittest.Run(t, ws, "commit", "-qm", "the bot commits its work, and the bookkeeping with it")

	// The next pass refreshes the skill and rewrites the bookkeeping.
	writeFile(t, filepath.Join(skillsSrc, "keep.md"), "# keep v3\n")
	mirrorKeepSkill(t, ws, skillsSrc)

	porcelain := porcelainOf(t, ws)
	if !strings.Contains(porcelain, ".iterion-managed/") {
		t.Fatalf("the fixture must show modified tracked bookkeeping: %q", porcelain)
	}
	if !strings.Contains(porcelain, " M .claude/skills/keep/SKILL.md\x00") {
		t.Fatalf("the fixture must show the mirror's refresh of the tracked skill: %q", porcelain)
	}
	if got := runOutputPaths(ws, porcelain); len(got) != 0 {
		t.Fatalf("runOutputPaths = %q, want empty — the engine's bookkeeping is never the run's work, tracked or not", got)
	}
	if got := commitWorkPaths(ws, porcelain); len(got) != 0 {
		t.Fatalf("commitWorkPaths = %q, want empty — the operator's merge-destined commit must not carry the bookkeeping either", got)
	}
}

// A staged-then-deleted ghost (`AD`: added to the index, then removed from
// the worktree) is in NEITHER HEAD nor the worktree. Counting it as work
// sends it to stageTreeWork's re-add, which fails the WHOLE gesture with
// "pathspec did not match" once the main `git add -A` has staged the
// ghost's deletion — the wip bank aborted, the real work beside it unbanked
// (measured on git 2.55). The probe excludes the ghost; the staging must
// succeed and carry the real work.
func TestStageRunWorkSkipsTheStagedThenDeletedGhost(t *testing.T) {
	for _, shape := range []string{"AD", "RD"} {
		t.Run(shape, func(t *testing.T) {
			ws := t.TempDir()
			gittest.InitRepo(t, ws)
			if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			addCommit(t, ws, "real.md", "base\n", "base")
			var ghost string
			switch shape {
			case "AD":
				writeFile(t, filepath.Join(ws, ".claude", "tmp.md"), "staged then deleted\n")
				gittest.Run(t, ws, "add", ".claude/tmp.md")
				if err := os.Remove(filepath.Join(ws, ".claude", "tmp.md")); err != nil {
					t.Fatal(err)
				}
				ghost = ".claude/tmp.md"
			case "RD":
				addCommit(t, ws, filepath.Join(".claude", "r.md"), "tracked under the mirror\n", "track the rename source")
				gittest.Run(t, ws, "mv", ".claude/r.md", ".claude/renamed.md")
				if err := os.Remove(filepath.Join(ws, ".claude", "renamed.md")); err != nil {
					t.Fatal(err)
				}
				ghost = ".claude/renamed.md"
			}
			writeFile(t, filepath.Join(ws, "real.md"), "the pass's work\n")

			porcelain := porcelainOf(t, ws)
			if !strings.Contains(porcelain, shape+" "+ghost+"\x00") {
				t.Fatalf("the fixture must carry the %s ghost record: %q", shape, porcelain)
			}
			work := runOutputPaths(ws, porcelain)
			if len(work) != 1 || work[0] != "real.md" {
				t.Fatalf("runOutputPaths = %q, want [real.md] — the ghost is not bankable work", work)
			}
			if err := stageRunWork(ws, work); err != nil {
				t.Fatalf("stageRunWork with a %s ghost beside real work: %v — the gesture must not fail wholesale", shape, err)
			}
			staged := gittest.Run(t, ws, "diff-index", "--cached", "--name-only", "HEAD")
			if !strings.Contains(staged, "real.md") {
				t.Fatalf("the real work is not staged: %q", staged)
			}
		})
	}
}

// End to end: the ghost beside the run's real work must not abort the wip
// bank — pre-fix the re-add's exit 128 preserved the worktree with the real
// work unbanked.
func TestFinalizeWorktree_WipBankSurvivesAStagedThenDeletedGhost(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".claude", "tmp.md"), "staged then deleted\n")
	gittest.Run(t, wt, "add", ".claude/tmp.md")
	if err := os.Remove(filepath.Join(wt, ".claude", "tmp.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "real.md"), "the pass's work\n")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "wip-ghost", runID: "run_g", autoMerge: true, mergeStrategy: "merge"}, nil)

	if !res.WipBanked || res.PreserveWorktree {
		t.Fatalf("the wip bank must survive the ghost, got %+v", res)
	}
	show := gittest.Run(t, repo, "show", "--stat", "--format=%s", res.FinalCommit)
	if !strings.Contains(show, "real.md") {
		t.Fatalf("banked commit missing the run's work:\n%s", show)
	}
}

// The diverged shadow KEEPS the manifest entry (pruneWorkspaceMirror's
// marker-keep doctrine): an agent's edit reads as work while it diverges,
// and an edit restoring exactly what the mirror wrote reads as still-ours.
// Dropping the entry on shadow would read the restore as work instead.
func TestShadowKeepsTheManifestEntrySoARestoreReadsAsStillOurs(t *testing.T) {
	ws, skillsSrc := keepSkillRepo(t)
	skillPath := filepath.Join(ws, ".claude", "skills", "keep", "SKILL.md")
	writeFile(t, skillPath, "# the agent's divergence\n")
	if got := runOutputPaths(ws, porcelainOf(t, ws)); len(got) != 1 {
		t.Fatalf("the divergent edit must read as work first, got %q", got)
	}
	// The next pass shadows the diverged file — and keeps its entry.
	mirrorKeepSkill(t, ws, skillsSrc)
	// The agent restores exactly what the mirror wrote.
	writeFile(t, skillPath, "# keep v2\n")
	if got := runOutputPaths(ws, porcelainOf(t, ws)); len(got) != 0 {
		t.Fatalf("runOutputPaths = %q, want empty — a restore of the mirror's exact bytes is still-ours", got)
	}
}

// A tracked mirror path replaced ON DISK by a directory: the probe reads
// the disappearance as work (no manifest entry names it), but stageTreeWork
// must not take the `git add` path for it — an add pathspec naming a
// directory RECURSES and sweeps the untracked mirror files it holds into
// the index, `-f` punching through the ignore rules (measured on git 2.55).
// The gesture is the deletion alone: `D <path>` in the index, the
// replacement directory left untracked.
func TestStageRunWorkDeletesATrackedPathReplacedByADirectory(t *testing.T) {
	cases := []struct {
		name  string
		plant func(t *testing.T, ws string) (path string)
	}{
		{"a tracked file replaced by a directory", func(t *testing.T, ws string) string {
			addCommit(t, ws, filepath.Join(".claude", "note.md"), "tracked\n", "track a mirror file")
			if err := os.Remove(filepath.Join(ws, ".claude", "note.md")); err != nil {
				t.Fatal(err)
			}
			return ".claude/note.md"
		}},
		{".claude itself, a tracked symlink replaced by a directory", func(t *testing.T, ws string) string {
			if err := os.Symlink("real-mirror", filepath.Join(ws, ".claude-link-target-check")); err == nil {
				_ = os.Remove(filepath.Join(ws, ".claude-link-target-check"))
			}
			if err := os.RemoveAll(filepath.Join(ws, ".claude")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("elsewhere", filepath.Join(ws, ".claude")); err != nil {
				t.Fatal(err)
			}
			gittest.Run(t, ws, "add", ".claude")
			gittest.Run(t, ws, "commit", "-qm", "track .claude as a symlink")
			if err := os.Remove(filepath.Join(ws, ".claude")); err != nil {
				t.Fatal(err)
			}
			return ".claude"
		}},
		{"tracked settings.json replaced by a directory", func(t *testing.T, ws string) string {
			addCommit(t, ws, filepath.Join(".claude", "settings.json"), "{}\n", "track settings")
			if err := os.Remove(filepath.Join(ws, ".claude", "settings.json")); err != nil {
				t.Fatal(err)
			}
			return ".claude/settings.json"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			gittest.InitRepo(t, ws)
			if err := os.MkdirAll(filepath.Join(ws, ".claude"), 0o755); err != nil {
				t.Fatal(err)
			}
			addCommit(t, ws, "real.md", "base\n", "base")
			path := tc.plant(t, ws)
			// The replacement directory, holding what looks like mirror noise.
			if err := os.MkdirAll(filepath.Join(ws, path), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(ws, path, "mirrored.md"), "untracked mirror noise\n")
			writeFile(t, filepath.Join(ws, "real.md"), "the pass's work\n")

			work := runOutputPaths(ws, porcelainOf(t, ws))
			if len(work) != 2 {
				t.Fatalf("runOutputPaths = %q, want the deleted tracked path and real.md", work)
			}
			if err := stageRunWork(ws, work); err != nil {
				t.Fatalf("stageRunWork: %v", err)
			}
			staged := gittest.Run(t, ws, "diff-index", "--cached", "--name-status", "HEAD")
			if !strings.Contains(staged, "D\t"+path) {
				t.Fatalf("the tracked path's deletion is not staged: %q", staged)
			}
			if strings.Contains(staged, "mirrored.md") {
				t.Fatalf("the re-add recursed into the replacement directory and swept the noise in: %q", staged)
			}
			if !strings.Contains(staged, "M\treal.md") {
				t.Fatalf("the real work is not staged: %q", staged)
			}
		})
	}
}

// After a child in place exits, the restore puts back the parent's borrowed
// entries — the mirror manifest included, or the child's manifest syncs
// survive against the parent's restored bytes and a TRACKED file of the
// parent's owned copy hash-mismatches what the manifest last recorded: the
// parent's converged run would wip-bank its own mirror state. The fixture
// runs a real parent+child pair over a repository tracking
// `.claude/iterion-skills/shared.md` at a content the parent's mirror
// refreshes.
func TestChildInPlaceRestoreResyncsTheManifest(t *testing.T) {
	work := t.TempDir()
	gittest.InitRepo(t, work)
	if err := os.MkdirAll(filepath.Join(work, ".claude", "iterion-skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	addCommit(t, work, filepath.Join(".claude", "iterion-skills", "shared.md"), "v0\n", "track the owned copy target")
	st := tmpStore(t)
	bundles := map[string]*bundle.Bundle{"parent": resourceBundle(t, "parent"), "child": resourceBundle(t, "child")}
	var launch func(ctx context.Context, name, parent string) error
	launch = func(ctx context.Context, name, parent string) error {
		ex := newStubExecutor()
		wf := resourceWorkflow("")
		if name == "parent" {
			wf = resourceWorkflow("child")
		}
		eng := New(wf, st, ex, WithWorkDir(work), WithBundle(bundles[name]), WithParentRunID(parent), WithSandboxOverride("none"), WithSubbotRunner(func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
			return map[string]any{}, launch(ctx, req.Source, req.ParentRunID)
		}))
		return eng.Run(ctx, name, nil)
	}
	if err := launch(context.Background(), "parent", ""); err != nil {
		t.Fatal(err)
	}

	// The parent's bytes are back (resource doctrine), and the manifest must
	// agree with them: the tracked file still shows ` M` against the v0
	// baseline — the parent's own mirror rewrite, not work.
	if raw, err := os.ReadFile(filepath.Join(work, ".claude", "iterion-skills", "shared.md")); err != nil || string(raw) != "parent" {
		t.Fatalf("the parent's owned copy was not restored: %q, %v", raw, err)
	}
	porcelain := porcelainOf(t, work)
	if !strings.Contains(porcelain, " M .claude/iterion-skills/shared.md\x00") {
		t.Fatalf("the fixture must show the parent's refreshed tracked file: %q", porcelain)
	}
	if got := runOutputPaths(work, porcelain); len(got) != 0 {
		t.Fatalf("runOutputPaths = %q, want empty — the child's manifest syncs must not survive the restore", got)
	}
}
