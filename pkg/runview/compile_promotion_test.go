package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// promptedBundle writes a bundle whose main.bot references a prompt that
// lives ONLY in prompts/ — a bare compile of its main.bot fails C003, so a
// green compile is the promotion itself. Marked as a bundle by skills/
// alone: no manifest, the case where Bundle.Manifest is nil.
func promptedBundle(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mf")
	for _, sub := range []string{"skills", "prompts"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "mission.md"), []byte("Do the thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pinned model and backend: the compile refuses C018 on a
	// credential-less host, and the test measures the promotion.
	src := "schema out:\n  ok: bool\n\nagent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCompileForLaunchPromotesABareMainBot: the compile behind
// Service.Launch — the studio's file picker, the dispatcher's service path,
// the trigger launcher — given `<bundle>/main.bot` compiles the BUNDLE: its
// prompts/*.md in scope, its hash, its handle for the engine, the way the
// CLI does. A bare compile of the same file is refused (C003), which is
// what makes the green compile evidence of the promotion, not of the
// fixture.
func TestCompileForLaunchPromotesABareMainBot(t *testing.T) {
	dir := promptedBundle(t)
	mainBot := filepath.Join(dir, "main.bot")
	if _, _, err := CompileWorkflowWithHash(mainBot); err == nil || !strings.Contains(err.Error(), "C003") {
		t.Fatalf("the bare compile did not fail on the prompt reference (err=%v); the fixture no longer arms the case", err)
	}
	wf, hash, b, err := compileForLaunch(mainBot, "", "")
	if err != nil {
		t.Fatalf("compileForLaunch on the bare main.bot: %v", err)
	}
	if wf == nil || b == nil || filepath.Clean(b.IterPath) != filepath.Clean(mainBot) {
		t.Fatalf("compileForLaunch returned workflow=%v bundle=%+v, want the workflow and the bundle at %s", wf != nil, b, mainBot)
	}
	opened, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, want, err := CompileBundleWorkflow(opened.IterPath, opened)
	if err != nil {
		t.Fatal(err)
	}
	if hash != want {
		t.Fatalf("hash = %s, want the bundle's %s: a run launched here would not resume from the CLI without --force", hash, want)
	}
	if b.Manifest != nil || b.Name() != "" {
		t.Fatalf("a bundle marked by skills/ alone has no manifest and no name; got manifest=%+v name=%q", b.Manifest, b.Name())
	}
	// Inline source and a stored bundle dir keep their own paths: no
	// promotion by path label, the bundle handed over is the one opened.
	if _, _, ib, err := compileForLaunch(mainBot, "schema out:\n  ok: bool\n\nagent a:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  output: out\n\nworkflow w:\n  entry: a\n  a -> done\n", ""); err != nil || ib != nil {
		t.Fatalf("inline source: bundle=%+v err=%v, want no bundle and no error", ib, err)
	}
	if _, _, sb, err := compileForLaunch(mainBot, "", dir); err != nil || sb == nil || filepath.Clean(sb.Dir) != filepath.Clean(dir) {
		t.Fatalf("stored bundle dir: bundle=%+v err=%v, want the bundle at %s", sb, err, dir)
	}
	// The studio's file picker: the file's source inline, materialised
	// under a name no promotion recognises, and the bundle dir stamped by
	// the server from the path the operator named — the compile merges
	// the prompts through that dir, and hashes as the bundle.
	src, err := os.ReadFile(mainBot)
	if err != nil {
		t.Fatal(err)
	}
	materialised := filepath.Join(t.TempDir(), "a1b2c3d4e5f6-main.bot")
	if _, _, _, err := compileForLaunch(materialised, string(src), ""); err == nil {
		t.Fatal("inline source with no bundle dir compiled the prompt reference; the fixture no longer arms the case")
	}
	if _, h, ib, err := compileForLaunch(materialised, string(src), dir); err != nil || ib == nil || h != want {
		t.Fatalf("inline source + bundle dir: hash=%s bundle=%+v err=%v, want the bundle's hash %s", h, ib, err, want)
	}
}

// TestCompileForLaunchRefusesABundleThatDoesNotOpen: a main.bot beside a
// manifest that does not decode IS a bundle by pkg/bundle's markers, so
// every path-driven compile refuses it by name — never a bare compile that
// starts the run without its prompts and skills — while a loose .bot beside
// nothing is neither promoted nor refused.
func TestCompileForLaunchRefusesABundleThatDoesNotOpen(t *testing.T) {
	dir := promptedBundle(t)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.OpenDir(dir); err == nil {
		t.Fatal("the fixture manifest still opens; the case it guards is not exercised")
	}
	mainBot := filepath.Join(dir, "main.bot")
	if b, err := ResolveBundleFromFilePath(mainBot); err == nil || b != nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("ResolveBundleFromFilePath = (%+v, %v), want an error naming the manifest", b, err)
	}
	if _, _, _, err := compileForLaunch(mainBot, "", ""); err == nil || !strings.Contains(err.Error(), "does not open") {
		t.Fatalf("compileForLaunch = %v, want the bundle refused", err)
	}
	if _, _, _, err := CompileWorkflowPath(mainBot); err == nil {
		t.Fatal("CompileWorkflowPath compiled a main.bot whose bundle does not open")
	}
	if name := BundleNameForPath(mainBot); name != "" {
		t.Fatalf("BundleNameForPath = %q on a bundle that does not open, want \"\"", name)
	}
	loose := filepath.Join(t.TempDir(), "main.bot")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := ResolveBundleFromFilePath(loose); err != nil || b != nil {
		t.Fatalf("a loose main.bot resolved to (%+v, %v), want (nil, nil)", b, err)
	}
}
