package e2e

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// expectedSecAuditDepsNodes lists the node IDs the sec-audit-deps
// bundle must expose. See bots/sec-audit-deps/main.bot and its
// README for the pipeline:
//
//	enumerate_deps → run_eco_heuristics + run_generic_heuristics →
//	  heuristic_join → load_cache → filter_cached → llm_review →
//	  update_cache → done
//
// W2.4 consolidated the per-ecosystem run_js/py/go_heuristics nodes and the
// heuristic_fanout router into the single skill-data-driven run_eco_heuristics
// (the catalogue of scanners now lives in skills/lang-*.md, not the DSL).
var expectedSecAuditDepsNodes = []string{
	"enumerate_deps",
	"run_eco_heuristics",
	"run_generic_heuristics",
	"heuristic_join",
	"load_cache",
	"filter_cached",
	"llm_review",
	"update_cache",
}

func TestBundle_SecAuditDeps_PackOpenCompile(t *testing.T) {
	t.Parallel()
	srcDir, err := filepath.Abs("../bots/sec-audit-deps")
	if err != nil {
		t.Fatalf("resolve src: %v", err)
	}
	out := filepath.Join(t.TempDir(), "sec-audit-deps.botz")

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
	if b.Manifest.Name != "sec-audit-deps" {
		t.Errorf("manifest.name = %q, want %q", b.Manifest.Name, "sec-audit-deps")
	}

	wf, wfHash, err := runview.CompileBundleWorkflow(b.IterPath, b)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow: %v", err)
	}
	if wfHash == "" {
		t.Errorf("workflow hash is empty")
	}

	for _, id := range expectedSecAuditDepsNodes {
		if _, ok := wf.Nodes[id]; !ok {
			t.Errorf("workflow missing expected node %q (drift detector)", id)
		}
	}
}

func TestBundle_SecAuditDeps_OpenDirMatchesPack(t *testing.T) {
	t.Parallel()
	srcDir, err := filepath.Abs("../bots/sec-audit-deps")
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

	out := filepath.Join(t.TempDir(), "sec-audit-deps.botz")
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
