package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// mirrorFloorFixture lays out one skill destination and its marker pair.
type mirrorFloorFixture struct {
	workDir    string
	destPath   string
	markerPath string
}

func newMirrorFloorFixture(t *testing.T, name string) mirrorFloorFixture {
	t.Helper()
	workDir := t.TempDir()
	dest := filepath.Join(workDir, ".claude", "skills")
	return mirrorFloorFixture{
		workDir:    workDir,
		destPath:   filepath.Join(dest, name, "SKILL.md"),
		markerPath: filepath.Join(dest, bundleMirrorMarkerDir, name+".SKILL.md.sha256"),
	}
}

// TestReconcileSkillFile_UntrustedArbitrationIgnoresThePlantedMarker.
//
// The on-disk marker and its tier sidecar live INSIDE the workspace: on a
// checkout the tenant did not write, both are attacker text. A marker naming
// the checkout's own file's hash and claiming the BUNDLE tier, read by the
// arbitration, shadows the library's playbook and leaves the checkout's file
// owned-and-served under the bot's skill name. On the fork lane the only tier
// the arbitration may read is the in-memory record of what THIS pass wrote.
func TestReconcileSkillFile_UntrustedArbitrationIgnoresThePlantedMarker(t *testing.T) {
	fx := newMirrorFloorFixture(t, "review")
	libSrc := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(libSrc, []byte("the library's playbook\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plant := func(t *testing.T) {
		t.Helper()
		writeSkill(t, filepath.Dir(fx.destPath), filepath.Base(fx.destPath), "attacker text\n")
		hash, err := hashFile(fx.destPath)
		if err != nil {
			t.Fatal(err)
		}
		// The plant is raw on purpose: it is the CHECKOUT's write, not a
		// mirror's, so no handle stands between it and the disk.
		if err := os.MkdirAll(filepath.Dir(fx.markerPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fx.markerPath, []byte(hash), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fx.markerPath+tierSidecarSuffix, []byte(skillTierBundle), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	plant(t)
	outcome, err := reconcileSkillFile(libSrc, fx.destPath, fx.markerPath, skillTierLibrary,
		testMirrorPass(t, fx.workDir, store.RunTrustFork), nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if outcome == skillOutcomeShadowed {
		t.Errorf("the planted marker shadowed the library's playbook — the arbitration read the workspace's claim")
	}
	if got, _ := os.ReadFile(fx.destPath); string(got) != "the library's playbook\n" {
		t.Errorf("the fork run serves %q under the bot's skill name, want the library's playbook", got)
	}

	// Control arm: the SAME plant on the trusted lane DOES route the
	// decision — workspace wins, the marker proves the run's earlier
	// ownership claim. Without it the overwrite above could mean the
	// arbitration never reads markers at all.
	fx2 := newMirrorFloorFixture(t, "review")
	fx = fx2
	plant(t)
	outcome, err = reconcileSkillFile(libSrc, fx.destPath, fx.markerPath, skillTierLibrary,
		testMirrorPass(t, fx.workDir, store.RunTrustDefault), nil)
	if err != nil {
		t.Fatalf("control reconcile: %v", err)
	}
	if outcome != skillOutcomeShadowed {
		t.Errorf("control: the trusted arbitration did not honor the marker claim (outcome %v) — the plant proves nothing", outcome)
	}
	if got, _ := os.ReadFile(fx.destPath); string(got) != "attacker text\n" {
		t.Errorf("control: the trusted workspace's file was overwritten: %q", got)
	}
}

// TestReconcileSkillFile_UntrustedUpToDateDoesNotRecordThePlantsTierClaim:
// the write side of the same floor. The destination already holds the
// library's own bytes (UpToDate — nothing to copy), but the planted marker
// pair claims the BUNDLE tier. If the arbitration read that claim, the
// marker it re-writes would record bundle-tier ownership of a file this pass
// mirrored from the library — the checkout's claim laundered into the run's
// own bookkeeping.
func TestReconcileSkillFile_UntrustedUpToDateDoesNotRecordThePlantsTierClaim(t *testing.T) {
	fx := newMirrorFloorFixture(t, "review")
	libSrc := filepath.Join(t.TempDir(), "review.md")
	if err := os.WriteFile(libSrc, []byte("the library's playbook\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The plant carries the library's EXACT bytes — the UpToDate branch —
	// plus a marker pair claiming them for the bundle tier.
	writeSkill(t, filepath.Dir(fx.destPath), filepath.Base(fx.destPath), "the library's playbook\n")
	hash, err := hashFile(fx.destPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(fx.markerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.markerPath, []byte(hash), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.markerPath+tierSidecarSuffix, []byte(skillTierBundle), 0o644); err != nil {
		t.Fatal(err)
	}

	outcome, err := reconcileSkillFile(libSrc, fx.destPath, fx.markerPath, skillTierLibrary,
		testMirrorPass(t, fx.workDir, store.RunTrustFork), nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if outcome != skillOutcomeUpToDate {
		t.Fatalf("outcome = %v, want UpToDate — the fixture is the library's own bytes", outcome)
	}
	if _, tier := readMarker(fx.markerPath); tier != skillTierLibrary {
		t.Errorf("the re-written marker records tier %q, want %q — the checkout's claim entered the run's own bookkeeping", tier, skillTierLibrary)
	}
}

// TestReconcileSkillFile_UntrustedPrecedenceIsNotMirrorOrder: on the fork
// lane the tier arbitration reads ONLY the in-memory record of what this
// pass wrote, so a name two tiers ship converges to the ADR-059 winner in
// EITHER mirror order. Dropping the in-memory read makes ORDER the winner —
// the library's text served under the bundle's skill name, a different
// playbook than a trusted run of the same bot gets.
func TestReconcileSkillFile_UntrustedPrecedenceIsNotMirrorOrder(t *testing.T) {
	srcDir := t.TempDir()
	bundleSrc := filepath.Join(srcDir, "bundle.md")
	libSrc := filepath.Join(srcDir, "library.md")
	if err := os.WriteFile(bundleSrc, []byte("the bundle's playbook\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libSrc, []byte("the library's playbook\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Bundle first, library second: the later, LOWER-tier reconcile must
	// shadow, from the pass's record alone.
	fx := newMirrorFloorFixture(t, "review")
	pass := testMirrorPass(t, fx.workDir, store.RunTrustFork)
	// prepareSkillDir + the marker dir, as mirrorBundleSkills/
	// mirrorFileSkill arrange: reconcileSkillFile owns the arbitration, the
	// directories' existence is the caller's.
	if err := prepareSkillDir(pass, filepath.Dir(fx.destPath), skillTierBundle); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := pass.root().MkdirAll(filepath.Dir(fx.markerPath), 0o755); err != nil {
		t.Fatalf("marker dir: %v", err)
	}
	if _, err := reconcileSkillFile(bundleSrc, fx.destPath, fx.markerPath, skillTierBundle, pass, nil); err != nil {
		t.Fatalf("bundle reconcile: %v", err)
	}
	outcome, err := reconcileSkillFile(libSrc, fx.destPath, fx.markerPath, skillTierLibrary, pass, nil)
	if err != nil {
		t.Fatalf("library reconcile: %v", err)
	}
	if outcome != skillOutcomeShadowed {
		t.Errorf("the lower-tier reconcile did not shadow (outcome %v) — mirror order beat precedence", outcome)
	}
	if got, _ := os.ReadFile(fx.destPath); string(got) != "the bundle's playbook\n" {
		t.Errorf("after bundle-then-library the run serves %q, want the bundle's playbook", got)
	}

	// Library first, bundle second: the later, HIGHER-tier reconcile
	// overwrites. Both orders converge on the bundle.
	fx2 := newMirrorFloorFixture(t, "review")
	pass2 := testMirrorPass(t, fx2.workDir, store.RunTrustFork)
	if err := prepareSkillDir(pass2, filepath.Dir(fx2.destPath), skillTierLibrary); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := pass2.root().MkdirAll(filepath.Dir(fx2.markerPath), 0o755); err != nil {
		t.Fatalf("marker dir: %v", err)
	}
	if _, err := reconcileSkillFile(libSrc, fx2.destPath, fx2.markerPath, skillTierLibrary, pass2, nil); err != nil {
		t.Fatalf("library reconcile: %v", err)
	}
	if _, err := reconcileSkillFile(bundleSrc, fx2.destPath, fx2.markerPath, skillTierBundle, pass2, nil); err != nil {
		t.Fatalf("bundle reconcile: %v", err)
	}
	if got, _ := os.ReadFile(fx2.destPath); string(got) != "the bundle's playbook\n" {
		t.Errorf("after library-then-bundle the run serves %q, want the bundle's playbook", got)
	}
}
