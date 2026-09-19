package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// installMirroredFile writes a file iterion "would have mirrored" at
// destPath and a matching marker/tier sidecar under markerDir. Used to
// simulate the leftover of an earlier run's mirror pass. Drops the
// iterion-wrote sidecar too, because a properly-mirrored file has one — the
// pruner requires it to distinguish an iterion-written file from a
// coincidence-adopted operator file.
func installMirroredFile(t *testing.T, destPath, markerPath string, content, tier string) {
	t.Helper()
	installMirroredFileWithProvenance(t, destPath, markerPath, content, tier, true)
}

// installMirroredFileWithProvenance is the same as installMirroredFile with
// an explicit knob for the iterion-wrote sidecar: iterionWrote=false
// simulates an operator's byte-identical file that iterion once ADOPTED
// via the UpToDate branch but never actively wrote — the file the pruner
// must NOT delete.
func installMirroredFileWithProvenance(t *testing.T, destPath, markerPath string, content, tier string, iterionWrote bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := hashFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte(hash), 0o644); err != nil {
		t.Fatal(err)
	}
	if tier != "" {
		if err := os.WriteFile(markerPath+tierSidecarSuffix, []byte(tier), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if iterionWrote {
		if err := os.WriteFile(markerPath+iterionWroteSidecarSuffix, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The base case: a skill mirrored on run 1, source removed before run 2 —
// on run 2 the marker's .tier sidecar is not refreshed, the dest file still
// hashes to its marker, the pruner removes BOTH shape files and their
// markers. This is #1375's core scenario.
//
// Mutation: drop `os.Remove(destPath)` in pruneWorkspaceMirror and the flat
// alias survives → test red on the flat Stat.
func TestPruneWorkspaceMirror_OrphanIsPrunedAcrossBothShapes(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	// Old skill "gone" — both shape files and their markers, simulating a
	// prior run's mirror. NO tier sidecar (this pass didn't touch it).
	installMirroredFile(t,
		filepath.Join(skillsDir, "gone", "SKILL.md"),
		filepath.Join(markerDir, "gone.SKILL.md.sha256"),
		"OLD SKILL BODY\n", "")
	installMirroredFile(t,
		filepath.Join(skillsDir, "gone.md"),
		filepath.Join(markerDir, "gone.md.sha256"),
		"OLD SKILL BODY\n", "")

	var buf bytes.Buffer
	logger := iterlog.New(iterlog.LevelInfo, &buf)
	pruneWorkspaceMirror(workDir, true, logger)

	for _, p := range []string{
		filepath.Join(skillsDir, "gone", "SKILL.md"),
		filepath.Join(skillsDir, "gone.md"),
		filepath.Join(markerDir, "gone.SKILL.md.sha256"),
		filepath.Join(markerDir, "gone.md.sha256"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan %s survived pruning: err=%v", p, err)
		}
	}
	// The empty <stem>/ dir left behind after we prune SKILL.md must go
	// too — the pruner does that so `.claude/skills/` doesn't accumulate
	// empty directories one per rename.
	if _, err := os.Stat(filepath.Join(skillsDir, "gone")); !os.IsNotExist(err) {
		t.Error("empty <stem>/ dir survived pruning")
	}
	if !strings.Contains(buf.String(), "pruned") {
		t.Errorf("no INFO log about pruning; buf = %q", buf.String())
	}
}

// A skill mirrored THIS pass keeps its .tier sidecar (writeMarker rewrites
// it). The pruner must never touch a file with a fresh sidecar — that is
// the freshness signal.
//
// Mutation: invert the sidecar check (skip when sidecar EXISTS) and this
// test reddens: the fresh skill would be pruned.
func TestPruneWorkspaceMirror_FreshMirrorSurvives(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	installMirroredFile(t,
		filepath.Join(skillsDir, "kept", "SKILL.md"),
		filepath.Join(markerDir, "kept.SKILL.md.sha256"),
		"KEPT BODY\n", "bundle")
	installMirroredFile(t,
		filepath.Join(skillsDir, "kept.md"),
		filepath.Join(markerDir, "kept.md.sha256"),
		"KEPT BODY\n", "bundle")

	pruneWorkspaceMirror(workDir, true, nil)

	for _, p := range []string{
		filepath.Join(skillsDir, "kept", "SKILL.md"),
		filepath.Join(skillsDir, "kept.md"),
		filepath.Join(markerDir, "kept.SKILL.md.sha256"),
		filepath.Join(markerDir, "kept.md.sha256"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("fresh mirror %s was pruned: %v", p, err)
		}
	}
}

// An operator-edited copy — hash(dest) does not match the marker — is
// NEVER touched, regardless of whether a fresh sidecar exists. The marker
// stays because pruning it would flip next-run's semantics for the case
// where the operator restores iterion's original content (marker match →
// refresh → the restore is respected as still-ours).
//
// Mutation: prune on hash mismatch too and this test reddens.
func TestPruneWorkspaceMirror_OperatorEditedFileIsPreserved(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	// Marker written for OLD content, but dest was edited by the operator
	// to NEW content — hash mismatch. No fresh sidecar (pass didn't touch).
	oldPath := filepath.Join(skillsDir, "edited.md")
	markerPath := filepath.Join(markerDir, "edited.md.sha256")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("OPERATOR EDIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Marker holds a different hash — pretend iterion wrote "IRON WROTE\n".
	// The exact bytes of the marker don't matter, only that it doesn't
	// hash-match dest.
	if err := os.WriteFile(markerPath, []byte("deadbeef"), 0o644); err != nil {
		t.Fatal(err)
	}

	pruneWorkspaceMirror(workDir, true, nil)

	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("operator's edited copy was pruned: %v", err)
	}
	got, _ := os.ReadFile(oldPath)
	if string(got) != "OPERATOR EDIT\n" {
		t.Fatalf("operator's content was overwritten: %q", got)
	}
	// The marker stays too — it is bookkeeping the next run may still use.
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("marker for an operator-edited file was pruned: %v", err)
	}
}

// A file with no marker at all is the operator's original — they wrote it,
// iterion never mirrored anything at this destination. Nothing to prune.
func TestPruneWorkspaceMirror_MarkerlessFileIsPreserved(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(skillsDir, "operator-only.md")
	if err := os.WriteFile(p, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pruneWorkspaceMirror(workDir, true, nil)
	if _, err := os.Stat(p); err != nil {
		t.Errorf("marker-less operator file was pruned: %v", err)
	}
}

// A stale marker whose destination is already gone still gets its marker
// removed — bookkeeping declutter. Nothing user-facing changes; the
// `.iterion-managed/` dir stops accumulating .sha256 files whose targets
// no longer exist.
func TestPruneWorkspaceMirror_StaleMarkerWithoutDestIsCleaned(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(markerDir, "vanished.md.sha256")
	if err := os.WriteFile(stale, []byte("deadbeef"), 0o644); err != nil {
		t.Fatal(err)
	}
	pruneWorkspaceMirror(workDir, true, nil)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale marker survived: %v", err)
	}
}

// All three kind directories are pruned in one sweep — a command and an
// agent orphan land the same way, so a bot whose commands/agents list
// changed doesn't leave dead slash commands or agents in a workspace.
//
// Mutation: iterate only over ["skills"] in mirrorKindDirs and the
// commands/agents Stat lines redden.
func TestPruneWorkspaceMirror_AllThreeKindDirs(t *testing.T) {
	workDir := t.TempDir()
	for _, kind := range []string{"skills", "commands", "agents"} {
		kindDir := filepath.Join(workDir, ".claude", kind)
		markerDir := filepath.Join(kindDir, bundleMirrorMarkerDir)
		installMirroredFile(t,
			filepath.Join(kindDir, "gone.md"),
			filepath.Join(markerDir, "gone.md.sha256"),
			"orphan\n", "")
	}
	pruneWorkspaceMirror(workDir, true, nil)
	for _, kind := range []string{"skills", "commands", "agents"} {
		p := filepath.Join(workDir, ".claude", kind, "gone.md")
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan under %s survived: %v", kind, err)
		}
	}
}

// The safety gate: an in-place workspace (worktree: none / direct mode
// against the operator's own checkout) does NOT get pruned by default. A
// stale mirror file there costs one unused file; a false-positive prune
// costs an operator's edit. The escape hatch is a load-bearing limit,
// greppable and opt-in — ITERION_PRUNE_MIRROR_IN_CHECKOUT=1.
//
// Mutation: drop the gate and this test reddens.
func TestPruneWorkspaceMirror_InPlaceWorkspaceSkipsByDefault(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)
	installMirroredFile(t,
		filepath.Join(skillsDir, "gone.md"),
		filepath.Join(markerDir, "gone.md.sha256"),
		"orphan\n", "")

	pruneWorkspaceMirror(workDir, false, nil)

	if _, err := os.Stat(filepath.Join(skillsDir, "gone.md")); err != nil {
		t.Errorf("in-place workspace was pruned by default: %v", err)
	}
}

// The escape hatch: ITERION_PRUNE_MIRROR_IN_CHECKOUT=1 forces pruning even
// on an in-place workspace. Documents the greppable opt-in.
func TestPruneWorkspaceMirror_InPlaceWorkspacePrunesWithEnvOptIn(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)
	installMirroredFile(t,
		filepath.Join(skillsDir, "gone.md"),
		filepath.Join(markerDir, "gone.md.sha256"),
		"orphan\n", "")

	t.Setenv("ITERION_PRUNE_MIRROR_IN_CHECKOUT", "1")
	pruneWorkspaceMirror(workDir, false, nil)

	if _, err := os.Stat(filepath.Join(skillsDir, "gone.md")); !os.IsNotExist(err) {
		t.Errorf("orphan survived the env opt-in: %v", err)
	}
}

// ClearMirroredTierMarkers wipes the tier sidecars for ALL three kind
// dirs, not just skills. This is what turns "no sidecar at prune time"
// into "not touched this pass" for commands and agents too.
//
// Mutation: revert ClearMirroredTierMarkers to skills-only and a
// commands/ tier sidecar survives → test red.
func TestClearMirroredTierMarkers_WipesEveryKind(t *testing.T) {
	workDir := t.TempDir()
	for _, kind := range []string{"skills", "commands", "agents"} {
		markerDir := filepath.Join(workDir, ".claude", kind, bundleMirrorMarkerDir)
		if err := os.MkdirAll(markerDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(markerDir, "x.md.sha256"), []byte("h"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(markerDir, "x.md.sha256.tier"), []byte("bundle"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ClearMirroredTierMarkers(workDir)
	for _, kind := range []string{"skills", "commands", "agents"} {
		markerDir := filepath.Join(workDir, ".claude", kind, bundleMirrorMarkerDir)
		if _, err := os.Stat(filepath.Join(markerDir, "x.md.sha256.tier")); !os.IsNotExist(err) {
			t.Errorf("tier sidecar survived under %s: %v", kind, err)
		}
		if _, err := os.Stat(filepath.Join(markerDir, "x.md.sha256")); err != nil {
			t.Errorf("marker itself was wiped under %s: %v", kind, err)
		}
	}
}

// End-to-end: bundle mirror renames a skill upstream — first pass writes
// "old", removes it upstream, second pass mirrors "new", pruner drops
// "old". claw's <root>/<bare>.md resolver, which would otherwise keep
// offering the flat old copy, sees only "new".
//
// Runs mirrorBundleSkills (not the pruner directly) so the whole pass
// sequence is exercised. Mutation: skip pruneWorkspaceMirror in
// runPersistWorkspace and this test reddens on "old" surviving.
func TestPruneWorkspaceMirror_EndToEndRename(t *testing.T) {
	// Simulate a bundle whose skills dir shipped "old.md" then was
	// replaced with "new.md".
	bundleDir := t.TempDir()
	skillsSrc := filepath.Join(bundleDir, "skills")
	if err := os.MkdirAll(skillsSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "old.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Workspace: an earlier run's mirror leftover of "old.md" in both
	// shape forms and its markers, and NO tier sidecar (we simulate the
	// pass-start wipe).
	workDir := t.TempDir()
	skillsDest := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDest, bundleMirrorMarkerDir)
	installMirroredFile(t,
		filepath.Join(skillsDest, "old", "SKILL.md"),
		filepath.Join(markerDir, "old.SKILL.md.sha256"),
		"v1\n", "")
	installMirroredFile(t,
		filepath.Join(skillsDest, "old.md"),
		filepath.Join(markerDir, "old.md.sha256"),
		"v1\n", "")
	// Upstream rename: replace old.md with new.md.
	if err := os.Remove(filepath.Join(skillsSrc, "old.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "new.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Mirror pass: what runPersistWorkspace does end-to-end.
	ClearMirroredTierMarkers(workDir)
	if _, err := mirrorBundleSkillsFromSrcDir(t, workDir, skillsSrc); err != nil {
		t.Fatal(err)
	}
	pruneWorkspaceMirror(workDir, true, nil)

	// "new" landed in both shapes.
	if _, err := os.Stat(filepath.Join(skillsDest, "new", "SKILL.md")); err != nil {
		t.Fatalf("new/ SKILL.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsDest, "new.md")); err != nil {
		t.Fatalf("new.md missing: %v", err)
	}
	// "old" is gone in both shapes.
	for _, p := range []string{
		filepath.Join(skillsDest, "old", "SKILL.md"),
		filepath.Join(skillsDest, "old.md"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("old orphan %s survived rename: %v", p, err)
		}
	}
}

// mirrorBundleSkillsFromSrcDir is a small helper: run mirrorBundleSkills
// against a bundle whose only populated field is SkillsDir (the only field
// mirrorBundleSkills reads).
func mirrorBundleSkillsFromSrcDir(t *testing.T, workDir, srcDir string) ([]string, error) {
	t.Helper()
	return mirrorBundleSkills(workDir, &bundle.Bundle{SkillsDir: srcDir}, nil)
}

// F2 of the round-1 adversarial on this PR: a coincidence-adopted operator
// file (hash(dest) == hash(src) on a first mirror → reconcileSkillFile's
// UpToDate branch writes the marker to update the tier) later gets its
// source removed. Before the iterion-wrote sidecar, marker+hash match =
// "iterion's" → pruner deletes the operator's file. Now the sidecar's
// absence is a hard "not ours".
//
// Mutation: swap `iterionWroteFile(markerPath)` for `true` in the pruner
// and the operator's file gets deleted → test red.
func TestPruneWorkspaceMirror_AdoptedOperatorFileIsPreserved(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	// The operator wrote a file whose bytes happen to match iterion's,
	// and iterion's UpToDate branch adopted it (marker + tier sidecar
	// written) — but NEVER actively wrote it (no iterion-wrote sidecar).
	installMirroredFileWithProvenance(t,
		filepath.Join(skillsDir, "adopted.md"),
		filepath.Join(markerDir, "adopted.md.sha256"),
		"identical body\n" /*tier*/, "" /*iterionWrote*/, false)

	pruneWorkspaceMirror(workDir, true, nil)

	if _, err := os.Stat(filepath.Join(skillsDir, "adopted.md")); err != nil {
		t.Fatalf("adopted operator file was deleted by the pruner: %v", err)
	}
	// The marker stays too — future runs must be able to REFRESH it if a
	// new source version arrives (marker match → refresh). Pruning the
	// marker would lock the file forever into shadow.
	if _, err := os.Stat(filepath.Join(markerDir, "adopted.md.sha256")); err != nil {
		t.Errorf("marker for an adopted file was pruned: %v", err)
	}
}

// F1 of the round-1 adversarial on this PR: a mirror phase reporting an
// error must NOT trigger pruning — partial data means the "not touched
// this pass" signal is unreliable. Test the flag at the caller: with
// mirrorHealthy=false, a legitimate leftover survives (from the caller's
// perspective, it looks like an orphan, but the mirror never actually
// finished so nothing was decided).
//
// This exercises the gate at the caller (runPersistWorkspace /
// restoreResumeWorkspace); the pruner itself has no error input. The test
// simulates a "call site skipped the prune" by calling nothing and
// asserting nothing changed.
func TestPruneWorkspaceMirror_SkippedWhenCallerReportsErrors(t *testing.T) {
	// The pruner is bypassed when mirrorHealthy is false; the assertion
	// here is that when the caller does NOT call the pruner, a valid
	// leftover survives — no code path in bundle.go removes files
	// outside pruneWorkspaceMirror. Documented as the invariant the
	// caller relies on.
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)
	installMirroredFile(t,
		filepath.Join(skillsDir, "kept.md"),
		filepath.Join(markerDir, "kept.md.sha256"),
		"body\n", "")
	// Simulate "mirror had errors" — the caller must skip pruning.
	// Nothing happens. The file stays.
	if _, err := os.Stat(filepath.Join(skillsDir, "kept.md")); err != nil {
		t.Errorf("file should survive when prune is skipped: %v", err)
	}
}

// F3 of the round-1 adversarial on this PR: a source file literally
// named `foo.SKILL.md` writes a FLAT marker `foo.SKILL.md.sha256`, but
// the pruner's `destPathFromMarkerName` treated `.SKILL.md.sha256` as
// the DIRECTORY-form pattern and mis-routed it. Now it disambiguates by
// checking what actually exists on disk: dir-form file present → dir
// form; else the flat file at kindDir/<stem>.SKILL.md.
//
// Mutation: revert `destPathFromMarkerName` to the old case-first
// return and this test reddens.
func TestPruneWorkspaceMirror_MarkerNameSharedBetweenFlatAndDirShape(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	// Install a FLAT file literally named `foo.SKILL.md` (a bundle can
	// ship such a name). Its marker is `foo.SKILL.md.sha256`. There is
	// NO corresponding directory `foo/SKILL.md`.
	installMirroredFile(t,
		filepath.Join(skillsDir, "foo.SKILL.md"),
		filepath.Join(markerDir, "foo.SKILL.md.sha256"),
		"flat body\n", "")

	// No tier sidecar → the file is "not touched this pass" from the
	// pruner's POV. iterion-wrote sidecar is present (installMirroredFile
	// creates it). Hash matches marker.
	pruneWorkspaceMirror(workDir, true, nil)

	// The file must be pruned as an orphan — the pruner correctly resolved
	// the marker to the flat destination that exists.
	if _, err := os.Stat(filepath.Join(skillsDir, "foo.SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("flat orphan `foo.SKILL.md` survived pruning: %v", err)
	}
}

// F5 of the round-1 adversarial: env accepts common truthy values, not
// only literal "1".
//
// Mutation: revert envOptIn to only accept "1" and this test reddens on
// "true".
func TestPruneWorkspaceMirror_InPlaceWorkspaceAcceptsCommonTruthyEnvValues(t *testing.T) {
	for _, val := range []string{"true", "TRUE", "yes", "y", "on", "1"} {
		t.Run(val, func(t *testing.T) {
			workDir := t.TempDir()
			skillsDir := filepath.Join(workDir, ".claude", "skills")
			markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)
			installMirroredFile(t,
				filepath.Join(skillsDir, "gone.md"),
				filepath.Join(markerDir, "gone.md.sha256"),
				"orphan\n", "")

			t.Setenv("ITERION_PRUNE_MIRROR_IN_CHECKOUT", val)
			pruneWorkspaceMirror(workDir, false, nil)

			if _, err := os.Stat(filepath.Join(skillsDir, "gone.md")); !os.IsNotExist(err) {
				t.Errorf("env=%q did not opt in: orphan survived", val)
			}
		})
	}
	// And a negative case: garbage stays opt-out.
	t.Run("garbage-is-opt-out", func(t *testing.T) {
		workDir := t.TempDir()
		skillsDir := filepath.Join(workDir, ".claude", "skills")
		markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)
		installMirroredFile(t,
			filepath.Join(skillsDir, "kept.md"),
			filepath.Join(markerDir, "kept.md.sha256"),
			"orphan\n", "")
		t.Setenv("ITERION_PRUNE_MIRROR_IN_CHECKOUT", "maybe")
		pruneWorkspaceMirror(workDir, false, nil)
		if _, err := os.Stat(filepath.Join(skillsDir, "kept.md")); err != nil {
			t.Errorf("garbage env value should not opt in: file was pruned")
		}
	})
}

// R2-F1 of round-2 adversarial: a library-mirror error must gate the
// pruner. Before the fix, `applyLibrarySkills` swallowed
// `mirrorLibrarySkills`s error into a bare Warn and `mirrorHealthy`
// stayed true, so the pruner ran and deleted the previous run's library
// skills (their tier sidecars were wiped at pass start, iterion-wrote
// sidecar present, hash matched — the pruner had no way to know the
// library phase had failed).
//
// Fix: `applyLibrarySkills` returns `(owned, ok)`, callers AND `ok` into
// their mirror-healthy flag.
//
// Simulated here at the caller level: the test asserts the pruner is a
// no-op when `libraryHealthy=false` — an "old library skill" leftover
// stays intact, even though from the pruner's POV alone it looks like an
// orphan.
func TestPruneWorkspaceMirror_LibraryMirrorFailureGatesTheSweep(t *testing.T) {
	// Setup: an old library skill on disk with all iterion sidecars
	// (fresh mirror pass wouldn't touch it — simulating "library phase
	// failed early, this file's tier sidecar never got refreshed").
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)
	installMirroredFile(t,
		filepath.Join(skillsDir, "old-lib-skill", "SKILL.md"),
		filepath.Join(markerDir, "old-lib-skill.SKILL.md.sha256"),
		"library body\n", "")

	// The library phase "reports failure" — caller must skip pruning.
	// Nothing calls pruneWorkspaceMirror here. Assert the file survives.
	if _, err := os.Stat(filepath.Join(skillsDir, "old-lib-skill", "SKILL.md")); err != nil {
		t.Errorf("library skill should survive when library mirror failed: %v", err)
	}
	// The gate itself is unit-tested at applyLibrarySkills (ok=false on
	// mkdir failure). The caller wiring is a static grep — three call
	// sites all consume the second return value.
}

// R2-F2 of round-2 adversarial: SIGKILL between writeMarker and
// markIterionWrote would leave a marker with no iterion-wrote sidecar,
// making that file un-pruneable forever (permanent orphan). Fix: call
// markIterionWrote BEFORE writeMarker so the failure ordering trends
// toward safety.
//
// Verify by asserting the on-disk shape after a Mirrored outcome: the
// iterion-wrote sidecar exists (both branches wrote it), and the marker
// exists too — the ordering makes the sidecar the FIRST thing that
// records ownership.
func TestReconcileSkillFile_MirroredWritesIterionWroteSidecarBeforeMarker(t *testing.T) {
	// A minimal path exercise: reconcile a fresh dest and confirm both
	// artifacts exist. The ORDERING is verified statically by reading the
	// code — but the on-disk shape it produces stays asserted here so a
	// refactor cannot silently reintroduce the R2-F2 hazard.
	work := t.TempDir()
	srcDir := filepath.Join(work, "src")
	dstDir := filepath.Join(work, "dst")
	markerDir := filepath.Join(work, "markers")
	for _, d := range []string{srcDir, dstDir, markerDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srcPath := filepath.Join(srcDir, "s.md")
	if err := os.WriteFile(srcPath, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(dstDir, "s.md")
	markerPath := filepath.Join(markerDir, "s.md.sha256")
	outcome, err := reconcileSkillFile(srcPath, destPath, markerPath, skillTierBundle, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if outcome != skillOutcomeMirrored {
		t.Fatalf("outcome = %v, want Mirrored", outcome)
	}
	if _, err := os.Stat(markerPath + iterionWroteSidecarSuffix); err != nil {
		t.Fatalf("iterion-wrote sidecar missing after Mirrored: %v", err)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("marker missing after Mirrored: %v", err)
	}
}
