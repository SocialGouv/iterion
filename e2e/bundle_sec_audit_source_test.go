package e2e

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// expectedSecAuditSourceNodes lists the node IDs the sec-audit-source
// bundle must expose. Drift on a node *name* surfaces here. The
// detection → N-vote verdict → report backbone (see
// bots/sec-audit-source/main.bot):
//
//	detect_tech → run_*_scanners → scan_join → scan_health → cap_findings
//	  → triage → filter_cached_files → voter_v1/v2/v3 → majority_verdict
//	  → merge_with_cache → report_card → update_file_records → remediate_gate
var expectedSecAuditSourceNodes = []string{
	"detect_tech",
	"run_generic_scanners",
	// run_lang_scanners: single skill-guided adaptive scanner step (replaced
	// the old per-language run_{js,go,python}_scanners after the
	// universal-code-bots refactor — stack knowledge now lives in
	// skills/lang-*.md, not in the workflow graph).
	"run_lang_scanners",
	// Cap2 programmatic matchers: see skills/programmatic-matchers.md
	"run_custom_matchers",
	"scan_join",
	// scan_health: deterministic anti-façade smoke gate — hard-fails when
	// the generic scanners produced no output. See sec_audit_scan_health_test.go.
	"scan_health",
	// cap_findings: deterministic overflow guard — bounds scanner JSON
	// before triage. See sec_audit_cap_findings_test.go.
	"cap_findings",
	"triage",
	// Cap1 FileRecords: see skills/file-records.md
	"filter_cached_files",
	// N-vote adversarial revalidation (Seki uplift, 2026-06): three disprove
	// voters feed majority_verdict — together they replaced the single
	// revalidate node, and merge_with_cache replaced merge_verdicts.
	"voter_v1",
	"voter_v2",
	"voter_v3",
	"majority_verdict",
	"merge_with_cache",
	"report_card",
	"update_file_records",
	// remediate_gate: entry to the optional verified-remediation phase
	// (Seki uplift) — always on the main path; the phase beyond it is gated.
	"remediate_gate",
}

// TestBundle_SecAuditSource_PackOpenCompile exercises the full bundle
// pipeline: PackDir → Detect → Open → CompileBundleWorkflow.
func TestBundle_SecAuditSource_PackOpenCompile(t *testing.T) {
	t.Parallel()
	srcDir, err := filepath.Abs("../bots/sec-audit-source")
	if err != nil {
		t.Fatalf("resolve src: %v", err)
	}
	out := filepath.Join(t.TempDir(), "sec-audit-source.botz")

	packRes, err := bundle.PackDir(srcDir, out)
	if err != nil {
		t.Fatalf("PackDir: %v", err)
	}
	if packRes.Entries < 4 {
		t.Errorf("packed only %d entries, expected at least main.bot + manifest.yaml + README.md", packRes.Entries)
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
	if b.Manifest.Name != "sec-audit-source" {
		t.Errorf("manifest.name = %q, want %q", b.Manifest.Name, "sec-audit-source")
	}

	wf, wfHash, err := runview.CompileBundleWorkflow(b.IterPath, b)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow: %v", err)
	}
	if wfHash == "" {
		t.Errorf("workflow hash is empty")
	}

	for _, id := range expectedSecAuditSourceNodes {
		if _, ok := wf.Nodes[id]; !ok {
			t.Errorf("workflow missing expected node %q (drift detector)", id)
		}
	}
}

// TestBundle_SecAuditSource_OpenDirMatchesPack confirms the dev path
// (OpenDir on the source directory) yields a structurally identical
// workflow to the packed archive.
func TestBundle_SecAuditSource_OpenDirMatchesPack(t *testing.T) {
	t.Parallel()
	srcDir, err := filepath.Abs("../bots/sec-audit-source")
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

	out := filepath.Join(t.TempDir(), "sec-audit-source.botz")
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
