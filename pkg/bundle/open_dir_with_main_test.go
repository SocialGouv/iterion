package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A bundle whose main is handed over opens without a main.bot on disk — the
// author document's case — with its manifest and its resource directories;
// the main has to sit at the bundle's root, and OpenDir still wants the file.
func TestOpenDirWithMainNeedsNoMainOnDisk(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"manifest.yaml": "schema_version: 1\nname: probe\n", "prompts/ask.md": "Say hello.\n"})
	main := filepath.Join(dir, "main.bot")
	b, err := OpenDirWithMain(dir, main)
	if err != nil {
		t.Fatalf("OpenDirWithMain: %v", err)
	}
	if b.IterPath != main {
		t.Fatalf("IterPath = %s, want the main handed over %s", b.IterPath, main)
	}
	if b.PromptsDir == "" || b.Manifest == nil || b.Manifest.Name != "probe" || b.Kind != KindBundleDir {
		t.Fatalf("the bundle around the main was not assembled: prompts=%q manifest=%+v kind=%v", b.PromptsDir, b.Manifest, b.Kind)
	}
	if _, err := OpenDir(dir); err == nil {
		t.Fatal("OpenDir opened a bundle that has no main.bot")
	}
	if _, err := OpenDirWithMain(dir, filepath.Join(dir, "lib", "main.bot")); err == nil {
		t.Fatal("a main outside the bundle's root was accepted")
	}
	if _, err := OpenDirWithMain(dir, ""); err == nil {
		t.Fatal("an empty main was accepted")
	}
}

// A manifest export naming the main handed over is that main, written or
// not; every other export is still a file to find.
func TestOpenDirWithMainTakesAnExportOfTheMainItHandsOver(t *testing.T) {
	dir := t.TempDir()
	exports := "schema_version: 1\nname: probe\nexports:\n  workflows:\n    - id: main\n      path: main.bot\n    - id: other\n      path: workflows/other.bot\n"
	writeTree(t, dir, map[string]string{"manifest.yaml": exports, "workflows/other.bot": "dsl: 2\n"})
	main := filepath.Join(dir, "main.bot")
	if _, err := OpenDirWithMain(dir, main); err != nil {
		t.Fatalf("the export of the main handed over was looked for on disk: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "workflows", "other.bot")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDirWithMain(dir, main); err == nil || !strings.Contains(err.Error(), `export "other"`) {
		t.Fatalf("another export missing on disk: %v, want the refusal naming it", err)
	}
}

// OpenForWorkflowHandedOver opens the bundle of a workflow file that is not
// written — an author document's .bot — where a manifest export names it;
// OpenForWorkflow still wants the file, and every other export is still a
// file to find.
func TestOpenForWorkflowHandedOverTakesAnExportOfTheWorkflow(t *testing.T) {
	dir := t.TempDir()
	exports := "schema_version: 1\nname: probe\nexports:\n  workflows:\n    - id: other\n      path: workflows/other.bot\n"
	writeTree(t, dir, map[string]string{"manifest.yaml": exports, "main.bot": "dsl: 2\n", "workflows/.keep": ""})
	other := filepath.Join(dir, "workflows", "other.bot")
	if _, err := OpenForWorkflow(other); err == nil {
		t.Fatal("OpenForWorkflow opened a bundle whose export is missing on disk")
	}
	b, err := OpenForWorkflowHandedOver(other)
	if err != nil {
		t.Fatalf("the export of the workflow handed over was looked for on disk: %v", err)
	}
	if b == nil || b.Dir != dir {
		t.Fatalf("the bundle of the workflow handed over: %+v, want the one at %s", b, dir)
	}
	writeTree(t, dir, map[string]string{"manifest.yaml": exports + "    - id: gone\n      path: workflows/gone.bot\n"})
	if _, err := OpenForWorkflowHandedOver(other); err == nil || !strings.Contains(err.Error(), `export "gone"`) {
		t.Fatalf("another export missing on disk: %v, want the refusal naming it", err)
	}
}

// The workflow handed over is known as a file, not a spelling: an export
// naming it through a symlinked directory of the bundle, or the workflow
// handed over through one, is that workflow — and an export naming another
// file through the same link is still looked for on disk.
func TestTheWorkflowHandedOverIsKnownWhateverItsSpelling(t *testing.T) {
	for name, tc := range map[string]struct{ export, handed string }{
		"the export spelled through the link":       {"wf/other.bot", "workflows/other.bot"},
		"the workflow handed over through the link": {"workflows/other.bot", "wf/other.bot"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			exports := "schema_version: 1\nname: probe\nexports:\n  workflows:\n    - id: other\n      path: " + tc.export + "\n"
			writeTree(t, dir, map[string]string{"manifest.yaml": exports, "main.bot": "dsl: 2\n", "workflows/.keep": ""})
			if err := os.Symlink("workflows", filepath.Join(dir, "wf")); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenForWorkflowHandedOver(filepath.Join(dir, tc.handed)); err != nil {
				t.Fatalf("the export of the workflow handed over was looked for on disk: %v", err)
			}
			if _, err := OpenForWorkflowHandedOver(filepath.Join(dir, "wf", "third.bot")); err == nil || !strings.Contains(err.Error(), `export "other"`) {
				t.Fatalf("an export of another file passed for the workflow handed over: %v", err)
			}
		})
	}
}
