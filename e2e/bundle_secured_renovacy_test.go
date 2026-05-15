package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// expectedSecuredRenovacyNodes lists the node IDs the secured-renovacy
// bundle must expose, regardless of whether the workflow is compiled
// from the source directory or from a packed `.botz` archive. The list
// captures the three phases:
//   - Phase 0:  detect_stack, capture_start_sha
//   - Phase 1 (fast-tracks + per-package loop): discover_outdated,
//     bucket_patches, select_candidate, security_audit, changelog_review,
//     upgrade, install, align_code, validate_upgrade, fix_after_upgrade,
//     prepare_commit, commit_changes
//   - Phase 2 (alternating review): alt_review, reviewer_claude,
//     reviewer_gpt, streak_check, emit_sbom
//
// Tests assert these IDs are present rather than the exact node count
// because the bundle is allowed to grow incrementally without breaking
// the regression suite. Drift on a node *name* surfaces here.
var expectedSecuredRenovacyNodes = []string{
	"detect_stack",
	"capture_start_sha",
	"discover_outdated",
	"bucket_patches",
	"select_candidate",
	"security_audit",
	"changelog_review",
	"upgrade",
	"install",
	"align_code",
	"validate_upgrade",
	"fix_after_upgrade",
	"prepare_commit",
	"commit_changes",
	"alt_review",
	"reviewer_claude",
	"reviewer_gpt",
	"streak_check",
	"emit_sbom",
}

// TestBundle_SecuredRenovacy_PackOpenCompile exercises the full bundle
// pipeline: PackDir → Detect → Open → CompileBundleWorkflow. Validates
// that a freshly-packed bundle round-trips through the runtime loader
// and yields a compilable workflow with the expected node structure.
func TestBundle_SecuredRenovacy_PackOpenCompile(t *testing.T) {
	srcDir, err := filepath.Abs("../examples/secured-renovacy")
	if err != nil {
		t.Fatalf("resolve src: %v", err)
	}
	out := filepath.Join(t.TempDir(), "secured-renovacy.botz")

	packRes, err := bundle.PackDir(srcDir, out)
	if err != nil {
		t.Fatalf("PackDir: %v", err)
	}
	if packRes.Entries < 4 {
		t.Errorf("packed only %d entries, expected at least bot.bot + manifest.yaml + README.md", packRes.Entries)
	}

	kind, err := bundle.Detect(out)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if kind != bundle.KindBundle {
		t.Fatalf("kind = %v, want KindBundle", kind)
	}

	b, cleanup, err := bundle.Open(out, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	if b.Hash != packRes.Hash {
		t.Errorf("Open hash %s != Pack hash %s — non-deterministic round-trip", b.Hash, packRes.Hash)
	}
	if b.Manifest == nil {
		t.Fatalf("manifest is nil — bundle missing manifest.yaml")
	}
	if b.Manifest.Name != "secured-renovacy" {
		t.Errorf("manifest.name = %q, want %q", b.Manifest.Name, "secured-renovacy")
	}

	wf, wfHash, err := runview.CompileBundleWorkflow(b.IterPath, b)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow: %v", err)
	}
	if wfHash == "" {
		t.Errorf("workflow hash is empty")
	}

	for _, id := range expectedSecuredRenovacyNodes {
		if _, ok := wf.Nodes[id]; !ok {
			t.Errorf("workflow missing expected node %q (drift detector)", id)
		}
	}
}

// TestBundle_SecuredRenovacy_OpenDirMatchesPack ensures the dev path
// (running directly against the source directory via OpenDir) produces
// a workflow that is structurally identical — same node IDs — to the
// archive path. The hashes differ on purpose (OpenDir leaves Hash empty)
// but the runtime contract is that either path yields the same graph.
func TestBundle_SecuredRenovacy_OpenDirMatchesPack(t *testing.T) {
	srcDir, err := filepath.Abs("../examples/secured-renovacy")
	if err != nil {
		t.Fatalf("resolve src: %v", err)
	}

	bDir, err := bundle.OpenDir(srcDir)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	wfDir, _, err := runview.CompileBundleWorkflow(bDir.IterPath, bDir)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow (dir): %v", err)
	}

	out := filepath.Join(t.TempDir(), "secured-renovacy.botz")
	if _, err := bundle.PackDir(srcDir, out); err != nil {
		t.Fatalf("PackDir: %v", err)
	}
	bArchive, cleanup, err := bundle.Open(out, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()
	wfArchive, _, err := runview.CompileBundleWorkflow(bArchive.IterPath, bArchive)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow (archive): %v", err)
	}

	if len(wfDir.Nodes) != len(wfArchive.Nodes) {
		t.Errorf("node count drift: dir=%d archive=%d", len(wfDir.Nodes), len(wfArchive.Nodes))
	}
	for id := range wfDir.Nodes {
		if _, ok := wfArchive.Nodes[id]; !ok {
			t.Errorf("archive workflow missing node %q present in dir", id)
		}
	}
	for id := range wfArchive.Nodes {
		if _, ok := wfDir.Nodes[id]; !ok {
			t.Errorf("dir workflow missing node %q present in archive", id)
		}
	}
}

// TestBundle_SecuredRenovacy_RepackMatchesShippedBotz is a drift
// detector: pack the source directory and compare the resulting hash
// against the committed `examples/secured-renovacy.botz`. If they
// diverge, someone updated `examples/secured-renovacy/` without
// repacking the archive — a silent regression for any user running the
// bundle by its archive path.
//
// PackDir guarantees byte-identical output for the same source tree
// (see pkg/bundle/writer_test.go::TestPackDir_Deterministic), so hash
// equality is the right invariant.
func TestBundle_SecuredRenovacy_RepackMatchesShippedBotz(t *testing.T) {
	shippedPath, err := filepath.Abs("../examples/secured-renovacy.botz")
	if err != nil {
		t.Fatalf("resolve shipped path: %v", err)
	}
	if _, err := os.Stat(shippedPath); err != nil {
		// `.botz` archives are git-ignored build artefacts (see .gitignore
		// comment block). A fresh clone won't have one — skip rather than
		// fail so CI stays green without forcing every contributor to pack.
		t.Skipf("shipped .botz absent at %s (run `./iterion bundle pack examples/secured-renovacy --force --output examples/secured-renovacy.botz`)", shippedPath)
	}

	bShipped, cleanup, err := bundle.Open(shippedPath, t.TempDir())
	if err != nil {
		t.Fatalf("Open shipped: %v", err)
	}
	defer cleanup()

	srcDir, err := filepath.Abs("../examples/secured-renovacy")
	if err != nil {
		t.Fatalf("resolve src: %v", err)
	}
	out := filepath.Join(t.TempDir(), "repacked.botz")
	packRes, err := bundle.PackDir(srcDir, out)
	if err != nil {
		t.Fatalf("PackDir: %v", err)
	}

	if packRes.Hash != bShipped.Hash {
		t.Errorf("drift detected: shipped hash %s != repack hash %s\n"+
			"  Run: ./iterion bundle pack examples/secured-renovacy --force --output examples/secured-renovacy.botz",
			bShipped.Hash, packRes.Hash)
	}
}
