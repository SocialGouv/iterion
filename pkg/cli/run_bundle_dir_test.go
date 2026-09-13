package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// TestResolveWorkflow_BundleDirCompilesAMaterialisedCopyWithItsBundle: the
// detached runner hands `iterion run` the store's materialised copy of a
// bundle's main.bot — a name no promotion recognises — and the bundle it
// was admitted against; the compile merges the bundle's prompts and
// hashes as the pre-flight did, so a launch admitted with the bundle does
// not die on C003 in the subprocess. Without the flag the copy compiles
// alone, which is what arms the case.
func TestResolveWorkflow_BundleDirCompilesAMaterialisedCopyWithItsBundle(t *testing.T) {
	inTempWorkspace(t)
	p, _ := testPrinter()
	if err := BotsCreate(BotsCreateOptions{Slug: "mf", Template: "multi-file", Model: "anthropic/claude-opus-4-8", Backend: "claude_code"}, p); err != nil {
		t.Fatalf("BotsCreate: %v", err)
	}
	dir := filepath.Join("bots", "mf")
	src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	copyDir := filepath.Join("store", "inline-sources")
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(copyDir, "a1b2c3d4e5f6-main.bot")
	if err := os.WriteFile(copyPath, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, cleanup, err := resolveWorkflow(RunOptions{File: copyPath}); err == nil {
		_ = cleanup()
		t.Fatal("the materialised copy compiled alone; the fixture no longer arms the case")
	} else if !strings.Contains(err.Error(), "C003") {
		t.Fatalf("the copy alone failed for another reason: %v", err)
	}
	wf, hash, filePath, _, opened, cleanup, err := resolveWorkflow(RunOptions{File: copyPath, BundleDir: dir})
	if err != nil {
		t.Fatalf("resolveWorkflow with --bundle-dir: %v", err)
	}
	defer func() { _ = cleanup() }()
	if wf == nil || opened == nil || filePath != copyPath {
		t.Fatalf("resolveWorkflow = (wf %v, bundle %v, file %q), want the copy compiled against the bundle", wf != nil, opened != nil, filePath)
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, want, err := runview.CompileBundleWorkflow(copyPath, b)
	if err != nil {
		t.Fatal(err)
	}
	if hash != want {
		t.Fatalf("digest %s, want the pre-flight's %s", hash, want)
	}
}
