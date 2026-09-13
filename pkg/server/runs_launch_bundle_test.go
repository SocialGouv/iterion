package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestLaunchBundleDirFor: a file launch of `<bundle>/main.bot` compiles
// against its bundle — read from the path the OPERATOR named, because the
// studio's file picker sends the source inline and the materialised copy
// is `<store>/inline-sources/<hash>-main.bot`, a name no promotion
// recognises. A loose file, a child of the bundle, and a path the server
// cannot place are label-only; a bundle that does not open is refused.
func TestLaunchBundleDirFor(t *testing.T) {
	srv, _ := newTestServer(t)
	tpl, ok := botscaffold.TemplateByID("multi-file")
	if !ok {
		t.Fatal("the multi-file template is gone")
	}
	spec := tpl.Spec
	spec.Slug = "mf"
	spec.Model, spec.Backend = "anthropic/claude-opus-4-8", "claude_code"
	dir := filepath.Join(srv.cfg.WorkDir, "bots", spec.Slug)
	if _, err := botscaffold.Scaffold(dir, spec); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	loose := filepath.Join(srv.cfg.WorkDir, "loose.bot")
	if err := os.WriteFile(loose, []byte("workflow x:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := srv.launchBundleDirFor("bots/mf/main.bot")
	if err != nil || filepath.Clean(got) != filepath.Clean(dir) {
		t.Fatalf("launchBundleDirFor(bots/mf/main.bot) = (%q, %v), want the bundle dir %s", got, err, dir)
	}
	for _, p := range []string{"", "loose.bot", "bots/mf/worker.bot", "../outside/main.bot", "nowhere/main.bot"} {
		if got, err := srv.launchBundleDirFor(p); err != nil || got != "" {
			t.Errorf("launchBundleDirFor(%q) = (%q, %v), want no bundle and no error", p, got, err)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.OpenDir(dir); err == nil {
		t.Fatal("the fixture manifest still opens; the case it guards is not exercised")
	}
	if got, err := srv.launchBundleDirFor("bots/mf/main.bot"); err == nil || got != "" || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("launchBundleDirFor on a bundle that does not open = (%q, %v), want an error naming the manifest", got, err)
	}
}
