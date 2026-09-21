package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
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

// F1 of the round-1 adversarial on this PR: a mirror phase that did not
// complete must not leave the pruner holding a stale signal — the caller
// skips pruning when a phase reports complete=false (or aborts on a hard
// I/O error), and this test documents the invariant the skip relies on:
// no code path in bundle.go removes files outside pruneWorkspaceMirror.
func TestPruneWorkspaceMirror_SkippedWhenCallerReportsErrors(t *testing.T) {
	// When the caller does NOT call the pruner, a leftover survives.
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)
	installMirroredFile(t,
		filepath.Join(skillsDir, "kept.md"),
		filepath.Join(markerDir, "kept.md.sha256"),
		"body\n", "")
	// The caller skipped pruning (mirror incomplete) — nothing happens.
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

// R2-F1 of round-2 adversarial: a library mirror that could not complete
// must not leave the pruner holding stale signals. The current shape:
// `applyLibrarySkills` returns `(owned, complete, err)`; a hard I/O error
// aborts the run, an incomplete pass (ValidName miss, unresolved ref)
// skips the prune at all three call sites.
//
// This test documents the file-level invariant that gate relies on.
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

	// The library phase reported incomplete — the caller skips pruning.
	// Nothing calls pruneWorkspaceMirror here. Assert the file survives.
	if _, err := os.Stat(filepath.Join(skillsDir, "old-lib-skill", "SKILL.md")); err != nil {
		t.Errorf("library skill should survive when library mirror failed: %v", err)
	}
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

// #1500 R2-F1 HIGH: a broken plugin.yaml causes `plugin.Load()` to skip
// the plugin entirely — its files never enter the enumeration, their tier
// sidecars aren't refreshed, and the pruner used to treat them as orphans
// and DELETE them. The fix: mirrorPluginContributions reports
// complete=true only when the pass enumerated everything it was ASKED to,
// and the caller skips the pruner when complete is false.
//
// This test exercises the mirror's OWN complete signal on the
// per-plugin `MirrorFiles(kind)` failure path. Mutation: revert the
// `complete = false` set to a plain `continue` and the assertion
// `complete=false` reddens.
func TestMirrorPluginContributions_PartialFailureFlagsIncomplete(t *testing.T) {
	// A registry-load failure is hard to inject without touching global
	// state; the per-plugin MirrorFiles(kind) branch is what a plugin
	// with a broken skills/ dir would exercise. Simulate by installing
	// a plugin whose manifest declares a skill file that does NOT exist
	// — MirrorFiles then fails to `fs.ReadFile` it.
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	pluginDir := filepath.Join(home, "plugins", "broken")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Manifest promises `skills/missing.md` but the file doesn't exist.
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"),
		[]byte("name: broken\nversion: 1.0.0\nschema_version: 1\ndefault_enabled: true\ncontributes:\n  skills:\n    - skills/missing.md\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	_, complete, err := mirrorPluginContributions(workDir, nil, false, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if complete {
		t.Errorf("complete=true despite MirrorFiles failure — pruner would treat prior orphans wrongly")
	}
}

// #1500 R2-F1 HIGH: a declared library skill that fails `store.Resolve`
// marks the pass incomplete. Mutation: revert the `complete = false` in
// the unresolved branch and this test reddens.
func TestMirrorLibrarySkills_UnresolvedRefFlagsIncomplete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	// No skill installed — `store.Resolve("missing")` misses.
	workDir := t.TempDir()
	_, _, complete, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"missing"}, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if complete {
		t.Errorf("complete=true despite an unresolved library skill — pruner would treat prior orphans wrongly")
	}
}

// #1500 R9cbabe — decided shape: the prunable marker grammar stays
// LOWERCASE-ONLY (`.md.sha256`), never widened to chase `Deploy.MD`-style
// names. A bundle CAN ship `Deploy.MD` (hasMarkdownSuffix accepts it), so
// such a flat alias's marker (`Deploy.MD.sha256`) is outside the prunable
// set and its orphan lives forever — the same status as a pre-#1375
// leftover. The DIRECTORY form of the same skill still prunes
// (`Deploy.SKILL.md.sha256` matches), so the claude_code-visible orphan is
// cleaned and only the flat uppercase alias remains. One stale file is the
// accepted cost; a grammar that grew a case dimension to chase it is not.
//
// Mutation: widen `destPathFromMarkerName` back to case-insensitive
// `.sha256` matching and this test reddens (the file gets deleted).
func TestPruneWorkspaceMirror_UppercaseMdAliasIsOutsideTheGrammar(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	installMirroredFile(t,
		filepath.Join(skillsDir, "Deploy.MD"),
		filepath.Join(markerDir, "Deploy.MD.sha256"),
		"orphan\n", "")

	pruneWorkspaceMirror(workDir, true, nil)

	if _, err := os.Stat(filepath.Join(skillsDir, "Deploy.MD")); err != nil {
		t.Fatalf("Deploy.MD alias was pruned — the grammar was widened; the decided shape keeps it lowercase-only: %v", err)
	}
}

// #1500 Q4: a child subbot handed its parent's live workspace must NEVER
// prune — the parent's own mirror is what wrote those files. Regression
// test at the caller-level: mirror complete + worktree=true still skips
// the pruner if parentRunID != "".
//
// Enforced by the three-part gate in runPersistWorkspace / both resume
// sites. This test counts the FULL gate expression — not the bare
// `e.parentRunID == ""` substring, which also appears in the
// workDirDelegated promotion branch (engine_run.go, unrelated to pruning)
// and would mask a deleted guard at one prune site (the round-3 whole-diff
// adversarial found exactly that off-by-one).
func TestPruneWorkspaceMirror_ChildSubbotNeverPrunes(t *testing.T) {
	const gate = `pluginsComplete && libraryComplete && e.parentRunID == ""`
	engineRun, err := os.ReadFile("engine_run.go")
	if err != nil {
		t.Fatal(err)
	}
	resume, err := os.ReadFile("resume.go")
	if err != nil {
		t.Fatal(err)
	}
	sites := strings.Count(string(engineRun), gate) + strings.Count(string(resume), gate)
	if sites != 3 {
		t.Fatalf("expected exactly 3 full-gate prune guards across engine_run.go + resume.go (launch + pause-resume + failure-resume), found %d", sites)
	}
	// And each occurrence sits immediately before a pruneWorkspaceMirror
	// call — a gate string appearing somewhere else would not protect
	// anything.
	for _, src := range []string{string(engineRun), string(resume)} {
		rest := src
		for i := strings.Index(rest, gate); i != -1; i = strings.Index(rest, gate) {
			after := rest[i+len(gate):]
			if !strings.Contains(after[:min(len(after), 400)], "pruneWorkspaceMirror(") {
				t.Fatalf("a full-gate occurrence is not followed by pruneWorkspaceMirror — wiring drifted")
			}
			rest = after
		}
	}
}

// R3-F1 HIGH of the whole-diff round: a BROKEN plugin.yaml makes
// `plugin.Load()` skip the plugin silently — `regErr` is nil and the
// plugin never enters `Enabled()`, so its files never enumerate and the
// pruner would read them as orphans. The fix surfaces `loadInstalled`'s
// skips via `Registry.LoadSkips()`, and mirrorPluginContributions marks
// the pass incomplete when any exist.
//
// This is the test the earlier attempt at this fix claimed in its doc
// comment but did not exercise — its fixture used a manifest-declared
// MISSING FILE (a MirrorFiles failure), not a broken manifest.
//
// Mutation: drop the `len(reg.LoadSkips()) > 0` check in
// mirrorPluginContributions and this test reddens (complete=true).
func TestMirrorPluginContributions_BrokenPluginYamlFlagsIncomplete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	pluginDir := filepath.Join(home, "plugins", "broken-yaml")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The incident shape: a truncated / unparseable plugin.yaml.
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.yaml"),
		[]byte("name: broken-yaml\nversion: 1.0.0\ndescription: unquoted: colon breaks yaml\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	_, complete, err := mirrorPluginContributions(workDir, nil, false, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if complete {
		t.Errorf("complete=true despite a broken plugin.yaml — the pruner would delete the broken plugin's previously-mirrored files while its manifest still declares them")
	}
}

// #1500 R6 high (21:08Z verdict): a cloud resume whose contributions payload
// dropped a still-declared library ref must NOT bless the pass complete — the
// prior copy of that skill is exactly what the pruner would delete, and the
// local path's own semantics (declared ref produced by no mirror ⇒
// complete=false) already guard the same class. The injected path verifies the
// declared union against what was actually produced this pass: the payload,
// the bundle/plugin mirrors that outrank the library, or a marker-less
// producer the pruner cannot touch.
//
// Seen RED on f00eed935: complete=true, and the seeded skill was deleted by
// the resume prune.
//
// Mutation: hardcode complete=true in mirrorLibrarySkills' injected branch
// and this test reddens.
func TestMirrorLibrarySkills_InjectedPayloadMissingDeclaredRefFlagsIncomplete(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	// The launch pass mirrored library skill "alpha"; the resume payload
	// dropped it while the workflow still declares it.
	installMirroredFile(t,
		filepath.Join(skillsDir, "alpha", "SKILL.md"),
		filepath.Join(markerDir, "alpha.SKILL.md.sha256"),
		"ALPHA BODY\n", "library")
	// The real mirror sequence wipes every tier sidecar at pass start; the
	// launch pass's sidecar must not count as fresh for THIS pass.
	ClearMirroredTierMarkers(workDir)

	wf := wfWithSkills([]string{"alpha", "beta"}, nil)
	inj := &Contributions{Library: []LibrarySkillFile{{Name: "beta", Description: "b", Content: []byte("BETA BODY\n")}}}
	_, _, complete, err := mirrorLibrarySkills(workDir, "", wf, nil, inj, nil)
	if err != nil {
		t.Fatalf("mirrorLibrarySkills: %v", err)
	}
	if complete {
		t.Errorf("complete=true although declared skill %q arrived in no mirror this pass — the pruner would delete its prior copy", "alpha")
	}
}

// #1500 R6 medium: a dispatch that arrived WITHOUT the contributions payload
// must not fall back to local resolution and read "0 enabled" as the
// declaration — a runner pod's iterion home is empty by design, so local
// resolution proves nothing about the launching instance's set. The mirror
// reports the pass incomplete so the pruner skips instead of deleting the
// launch pass's plugin contributions.
//
// Seen RED on f00eed935 (probe): complete=true, and the seeded plugin file
// was deleted by the resume prune.
//
// Mutation: drop mirrorPluginContributions' ambientUnresolved branch and this
// test reddens (the local path runs: empty home → 0 enabled → complete=true).
func TestMirrorPluginContributions_NilPayloadOnRunnerFlagsIncomplete(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir()) // empty pod home: local resolution finds nothing
	workDir := t.TempDir()
	_, complete, err := mirrorPluginContributions(workDir, nil, true, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if complete {
		t.Errorf("complete=true although the ambient declaration could not be verified (nil payload) — the pruner would delete the launch pass's plugin files")
	}
}

// The flag is set ONLY for a cloud dispatch whose payload did not arrive. A
// local CLI/studio run keeps local resolution, where "0 enabled plugins" IS
// the truth and the pruner must stay armed — #1375's core scenario (a plugin
// disabled between two local passes leaves real orphans to sweep).
//
// Mutation: make mirrorPluginContributions return incomplete whenever the
// flag is consulted-or-set regardless of resolution, and this test reddens.
func TestMirrorPluginContributions_LocalResolutionStillTrustedWithoutTheFlag(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	workDir := t.TempDir()
	_, complete, err := mirrorPluginContributions(workDir, nil, false, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if !complete {
		t.Errorf("complete=false for a local run with an empty registry — local resolution IS the declaration; the pruner must stay armed")
	}
}

// The healthy cross-case: a cloud resume whose payload carries everything the
// launch pass mirrored must stay COMPLETE and keep the pruner armed — a true
// orphan (mirrored once, absent from the payload and from every other tier)
// is swept, and the payload-carried skills survive untouched.
//
// Mutation: make the injected library check veto unconditionally (or make
// mirrorInjectedPluginFiles report incomplete on an empty wire) and this
// test reddens: the true orphan survives, or the pass reports incomplete.
func TestPruneWorkspaceMirror_HealthyCloudResumePrunesTrueOrphans(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	// "gone" was mirrored by an earlier pass and is in NO tier anymore — the
	// true orphan the pruner exists for.
	installMirroredFile(t,
		filepath.Join(skillsDir, "gone", "SKILL.md"),
		filepath.Join(markerDir, "gone.SKILL.md.sha256"),
		"OLD BODY\n", "library")
	// "kept" is declared by the workflow and rides the resume payload.
	installMirroredFile(t,
		filepath.Join(skillsDir, "kept", "SKILL.md"),
		filepath.Join(markerDir, "kept.SKILL.md.sha256"),
		"KEPT BODY\n", "library")

	wf := wfWithSkills([]string{"kept"}, nil)
	inj := &Contributions{Library: []LibrarySkillFile{{Name: "kept", Description: "k", Content: []byte("KEPT BODY\n")}}}

	ClearMirroredTierMarkers(workDir)
	_, pluginsComplete, err := mirrorPluginContributions(workDir, inj, false, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	_, _, libraryComplete, err := mirrorLibrarySkills(workDir, "", wf, nil, inj, nil)
	if err != nil {
		t.Fatalf("mirrorLibrarySkills: %v", err)
	}
	if !pluginsComplete || !libraryComplete {
		t.Fatalf("a healthy payload must not veto the prune (plugins=%v library=%v)", pluginsComplete, libraryComplete)
	}
	pruneWorkspaceMirror(workDir, true, nil)

	if _, err := os.Stat(filepath.Join(skillsDir, "gone", "SKILL.md")); !os.IsNotExist(err) {
		t.Errorf("true orphan survived a healthy resume prune (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "kept", "SKILL.md")); err != nil {
		t.Errorf("payload-carried skill was harmed: %v", err)
	}
}

// R17fd85 (unanswered prior-verdict inline high, same class as R6): on the
// LOCAL path, a library store outage on resume must not leave the pass
// complete. A leftover from a PREVIOUS pass satisfies the bare
// alreadyMirrored stat but has no fresh tier sidecar — it is exactly the file
// the pruner would delete while the workflow still declares it.
//
// Mutation: restore the bare alreadyMirrored call in mirrorLibrarySkills'
// Resolve-miss branch and this test reddens (complete=true).
func TestMirrorLibrarySkills_StaleLeftoverDoesNotSatisfyDeclaredRef(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir()) // the store outage: Resolve misses
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	// The previous pass mirrored "alpha" from the library; the marker's
	// tier sidecar is wiped by this pass's ClearMirroredTierMarkers.
	installMirroredFile(t,
		filepath.Join(skillsDir, "alpha", "SKILL.md"),
		filepath.Join(markerDir, "alpha.SKILL.md.sha256"),
		"ALPHA BODY\n", "library")
	ClearMirroredTierMarkers(workDir)

	_, _, complete, err := mirrorLibrarySkills(workDir, "", wfWithSkills([]string{"alpha"}, nil), nil, nil, nil)
	if err != nil {
		t.Fatalf("mirrorLibrarySkills: %v", err)
	}
	if complete {
		t.Errorf("complete=true although the declared ref is satisfied only by a previous pass's un-refreshed copy — the pruner would delete it on a transient store outage")
	}
}

// The runner-side flag reaches the mirror through the engine field: the
// option sets it, the mirror consumes it (the effect is pinned at
// TestMirrorPluginContributions_NilPayloadOnRunnerFlagsIncomplete, the wiring
// at pkg/runner's TestContributionsEngineOptions_WiredOnBothDispatchPaths).
func TestWithContributionsUnresolved_FlagsTheEngine(t *testing.T) {
	e := &Engine{}
	WithContributionsUnresolved()(e)
	if !e.contributionsUnresolved {
		t.Fatal("WithContributionsUnresolved must set the engine's unresolved flag")
	}
}

// Composition witness for the R6-medium veto: the flag must survive the REAL
// production pass — engine built the way the runner builds it, workspace
// carrying the launch pass's plugin mirrors — not just the mirror function in
// isolation. A mutation that breaks the flag threading AT THE CALL SITE
// (engine_run.go / either resume site) lets the empty-pod local resolution
// report complete=true and deletes the seeded file; this test reddens on it
// (proven: the whole committed suite stayed green under exactly that
// mutation before this test existed).
func TestRunPersistWorkspace_UnresolvedContributionsSkipPrune(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())             // empty pod home: local resolution finds nothing
	t.Setenv("ITERION_PRUNE_MIRROR_IN_CHECKOUT", "1") // in-place workspace: the documented prune opt-in
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	// The launch pass mirrored a plugin contribution into this workspace.
	installMirroredFile(t,
		filepath.Join(skillsDir, "deploy-skill", "SKILL.md"),
		filepath.Join(markerDir, "deploy-skill.SKILL.md.sha256"),
		"DEPLOY BODY\n", "plugin")

	wf := &ir.Workflow{Name: "unresolved", Nodes: map[string]ir.Node{}}
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	runID := "unresolved-contribs"
	run, err := s.CreateRun(context.Background(), runID, wf.Name, nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	eng := New(wf, s, nil, WithWorkDir(workDir), WithContributionsUnresolved())
	if err := eng.runPersistWorkspace(context.Background(), runID, run, false, worktreeContext{}); err != nil {
		t.Fatalf("runPersistWorkspace: %v", err)
	}

	if _, err := os.Stat(filepath.Join(skillsDir, "deploy-skill", "SKILL.md")); err != nil {
		t.Fatalf("seeded launch leftover DELETED although the engine held WithContributionsUnresolved — flag-to-mirror wiring broken: %v", err)
	}
}

// Same composition witness through the FAILURE-RESUME path
// (restoreResumeWorkspace), which re-mirrors and prunes with r.Worktree —
// the pause-resume site (resumeRebuildState) shares the same predicate and
// the same mirror call.
func TestRestoreResumeWorkspace_UnresolvedContributionsSkipPrune(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	t.Setenv("ITERION_PRUNE_MIRROR_IN_CHECKOUT", "1")
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	markerDir := filepath.Join(skillsDir, bundleMirrorMarkerDir)

	installMirroredFile(t,
		filepath.Join(skillsDir, "deploy-skill", "SKILL.md"),
		filepath.Join(markerDir, "deploy-skill.SKILL.md.sha256"),
		"DEPLOY BODY\n", "plugin")

	wf := &ir.Workflow{Name: "unresolved", Nodes: map[string]ir.Node{}}
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	eng := New(wf, s, nil, WithContributionsUnresolved())
	r := &store.Run{ID: "unresolved-resume", WorkDir: workDir}
	if err := eng.restoreResumeWorkspace(r); err != nil {
		t.Fatalf("restoreResumeWorkspace: %v", err)
	}

	if _, err := os.Stat(filepath.Join(skillsDir, "deploy-skill", "SKILL.md")); err != nil {
		t.Fatalf("seeded launch leftover DELETED on failure-resume although the engine held WithContributionsUnresolved: %v", err)
	}
}

// The publisher's own confession travels: a payload flagged Degraded mirrors
// what arrived but must NOT bless the prune — entries the launch pass
// mirrored are absent (the cloud twin of the local path's LoadSkips veto,
// whose shape it mirrors exactly: skip ⇒ complete=false, files survive).
//
// Mutation: drop mirrorPluginContributions' inj.Degraded branch and this
// test reddens (complete=true, pruner armed on an amputated declaration).
func TestMirrorPluginContributions_DegradedPayloadFlagsIncomplete(t *testing.T) {
	workDir := t.TempDir()
	inj := &Contributions{
		Plugin:   []ContributionFile{{Kind: "skills", Name: "ok.md", Content: []byte("OK\n")}},
		Degraded: true,
	}
	_, complete, err := mirrorPluginContributions(workDir, inj, false, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if complete {
		t.Errorf("complete=true on a Degraded payload — the amputated declaration blessed the prune; the broken plugin's still-enabled mirrors would be deleted on resume")
	}
}

// A healthy payload with the SAME shape stays complete — Degraded is a
// per-payload fact, not a cloud-wide veto. Mutation: make the Degraded
// branch veto every injected pass and this test reddens.
func TestMirrorPluginContributions_HealthyPayloadStaysComplete(t *testing.T) {
	workDir := t.TempDir()
	inj := &Contributions{
		Plugin: []ContributionFile{{Kind: "skills", Name: "ok.md", Content: []byte("OK\n")}},
	}
	_, complete, err := mirrorPluginContributions(workDir, inj, false, nil)
	if err != nil {
		t.Fatalf("mirrorPluginContributions: %v", err)
	}
	if !complete {
		t.Errorf("complete=false on a healthy payload — Degraded must be a per-payload fact, not a cloud-wide veto")
	}
}

// The marker-less arm of skillCoveredThisPass: a bundle can satisfy a
// declared ref with a DIRECTORY skill (mirrorBundleSkills writes no marker
// for directory sources). The ref is covered — and must not veto the prune —
// because the pruner cannot touch a name without a marker anyway. A weak
// predicate that required a marker would turn every such cloud run into a
// STATIONARY veto.
//
// Mutation: make skillCoveredThisPass's marker-less arm return false and
// this test reddens (complete=false on a healthy cloud run).
func TestMirrorLibrarySkills_BundleDirFormSatisfiesInjectedDeclaration(t *testing.T) {
	workDir := t.TempDir()
	skillsSrc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(skillsSrc, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsSrc, "alpha", "SKILL.md"), []byte("DIRFORM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &bundle.Bundle{SkillsDir: skillsSrc}
	if _, err := mirrorBundleSkills(workDir, b, nil); err != nil {
		t.Fatalf("mirrorBundleSkills: %v", err)
	}

	// The payload carries no library skills; the workflow declares only the
	// bundle-owned ref.
	wf := wfWithSkills([]string{"alpha"}, nil)
	inj := &Contributions{}
	_, _, complete, err := mirrorLibrarySkills(workDir, "", wf, nil, inj, nil)
	if err != nil {
		t.Fatalf("mirrorLibrarySkills: %v", err)
	}
	if !complete {
		t.Errorf("complete=false although declared ref %q is owned by a marker-less bundle directory skill — stationary veto on a healthy cloud run", "alpha")
	}
}
